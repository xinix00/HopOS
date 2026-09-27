package mve

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/codec"
)

// Trace krijgt elk bericht dat de firmware stuurt, als hij gezet is. Voor de
// bring-up: zonder te zien wát de firmware zegt is een stille sessie niet van
// een verkeerd begrepen sessie te onderscheiden.
var Trace func(code uint16, body []byte)

// pump is de enige plek waar de firmware iets van ons gedaan krijgt: hij
// bedient het geheugenverzoek, leest de berichten, en haalt de buffers op die
// klaar zijn. Alles gebeurt vanuit Poll; de driver heeft geen interrupthandler.
// Zo draait er geen codec-werk in interruptcontext en heeft de aanroeper de
// regie over zijn eigen tempo, net als bij onze NIC's.
func (s *session) pump() {
	s.serveRPC()
	s.drainMessages()
	s.drainBuffers(&s.bin, true)
	s.drainBuffers(&s.bout, false)
	// Pas hier, na de bufferringen: de firmware geeft eerst alle
	// uitvoerbuffers terug en meldt dán pas OUTPUT_FLUSHED. Wie op het
	// bericht afgaat vóór hij de ring leegt, ziet die teruggegeven buffers
	// aan voor gedecodeerde beelden.
	if s.outAck {
		s.outAck = false
		s.outHold = false
		pend := s.outPend
		s.outPend = nil
		for _, b := range pend {
			if err := s.sendOutput(b); err != nil {
				s.fail(err)
				return
			}
		}
	}
}

// holdOutput voert de handshake uit die de firmware na SEQUENCE_PARAMETERS
// eist. Hij staat met zoveel woorden in mve_protocol_def.h: de firmware "will
// not make any progress until the host sends an output-flush command". Dus:
// flush sturen, hem zijn uitvoerbuffers laten teruggeven, en pas na
// OUTPUT_FLUSHED opnieuw aanbieden.
func (s *session) holdOutput() {
	if s.cfg.Dir != codec.Decode || s.outHold {
		return
	}
	s.outHold = true
	s.outAck = false
	s.stat.flushes++
	if err := s.msg.send(reqOutputFlush, nil); err != nil {
		s.fail(err)
		return
	}
	s.startJob()
	s.schedule()
}

// drainMessages leest de berichtenring leeg.
func (s *session) drainMessages() {
	var buf [256]byte
	for {
		code, n, ok, err := s.msg.recv(buf[:])
		if err != nil {
			s.fail(err)
			return
		}
		if !ok {
			return
		}
		if Trace != nil {
			Trace(code, buf[:n])
		}
		s.handleMessage(code, buf[:n])
	}
}

// handleMessage vertaalt één firmware-bericht naar wat de aanroeper ervan moet
// merken. Het meeste is boekhouding die niemand buiten deze driver aangaat
// (in- en uitwisselen van cores, bevestigingen); alleen de streamparameters,
// het einde en de fouten komen naar buiten.
func (s *session) handleMessage(code uint16, body []byte) {
	le := binary.LittleEndian
	switch code {
	case respSeqParams:
		if len(body) < 7 {
			return
		}
		s.seq.known = true
		s.seq.chroma = body[1]
		s.seq.bitdepth = body[2]
		s.seq.minBuffers = int(body[4])
		s.publishLayout()
		s.holdOutput()

	case respFrameAllocParm:
		if len(body) < 20 {
			return
		}
		s.alloc.known = true
		s.alloc.width = le.Uint16(body[0:])
		s.alloc.height = le.Uint16(body[2:])
		s.alloc.cropX = le.Uint16(body[16:])
		s.alloc.cropY = le.Uint16(body[18:])
		s.publishLayout()

	case respError:
		if len(body) < 4 {
			s.fail(errors.New("mve: firmware reported an error"))
			return
		}
		s.fail(fmt.Errorf("mve: firmware error %d: %s",
			le.Uint32(body), cstring(body[4:])))

	case respStateChange:
		// 0 = gestopt: alles wat de firmware nog vasthield is terug, en na een
		// EOS is dit het punt waarop de stream werkelijk afgelopen is.
		if len(body) >= 4 && le.Uint32(body) == 0 {
			s.post(codec.Event{Kind: codec.Done})
		}

	case respIdle:
		// De firmware legt zich te slapen omdat er niets te doen is. Alleen
		// bevestigen — anders blijft hij de interruptlijn eraan houden. Zijn
		// job blijft staan: IDLE is geen einde van een beurt, en wie hier een
		// nieuwe job stuurt loopt zijn wachtrij vol ("no space in job
		// queue"). De volgende buffer wekt hem via de deurbel.
		_ = s.msg.send(reqIdleAck, nil)
		s.kick()

	case respJobDequeued:
		// Dít is het einde van een beurt, en het enige moment waarop er een
		// nieuwe bij mag. Altijd meteen: zo staat er precies één job open en
		// hoeft geen enkel ander pad erover na te denken.
		s.jobLive = false
		s.startJob()
		s.schedule()

	case respEvent:
		// De firmware praat hier over de STREAM, niet over een buffer:
		// corrupt, niet-ondersteund, of "beeld klaar". Alleen de eerste twee
		// zijn nieuws voor de aanroeper, en die komen als tekst.
		if len(body) >= 4 {
			switch le.Uint32(body) {
			case evStreamCorrupt:
				s.stat.streamCorrupt++
			case evStreamUnsupported:
				s.fail(fmt.Errorf("mve: firmware cannot decode this stream: %s",
					cstring(body[4:])))
			}
		}

	case respOutputFlushed:
		// Niet hier afhandelen: de buffers die bij deze flush horen staan nog
		// in de ring. Pump maakt het af zodra die leeg is.
		s.outAck = true

	case respInput, respOutput, respSwitchedIn, respSwitchedOut,
		respOptionConfirm, respPong,
		respInputFlushed, respRefFrameUnused:
		// Seintjes; het werk zelf staat in de bufferringen.

	case respOptionFail:
		s.fail(errors.New("mve: firmware rejected an option"))
	}
}

// publishLayout meldt het beeldformaat zodra beide helften bekend zijn: wat
// voor stream het is (respSeqParams) en hoe groot een buffer moet zijn
// (respFrameAllocParm). Vóór dat moment weet niemand — ook de firmware niet —
// hoeveel geheugen een frame kost.
func (s *session) publishLayout() {
	if !s.seq.known || !s.alloc.known || s.cfg.Dir != codec.Decode {
		return
	}
	w, h := int(s.alloc.width), int(s.alloc.height)
	stride := s.stride(w)
	luma := uint64(stride * h)

	// De firmware meldt hier hoe groot een BUFFER moet zijn. Wat er zichtbaar
	// van is staat per beeld in de frame-descriptor; tot het eerste frame
	// binnen is, is de gealloceerde maat de beste schatting.
	l := codec.Layout{
		Width:       w,
		Height:      h,
		AllocWidth:  w,
		AllocHeight: h,
		Pixel:       s.cfg.Pixel,
		MinBuffers:  s.seq.minBuffers,
	}
	l.Planes[0] = codec.Plane{Off: 0, Stride: stride}
	switch s.cfg.Pixel {
	case codec.I420:
		l.Planes[1] = codec.Plane{Off: luma, Stride: stride / 2}
		l.Planes[2] = codec.Plane{Off: luma + luma/4, Stride: stride / 2}
		l.FrameSize = luma * 3 / 2
	case codec.Y8:
		l.FrameSize = luma
	default:
		l.Planes[1] = codec.Plane{Off: luma, Stride: stride}
		l.FrameSize = luma * 3 / 2
	}
	if l.MinBuffers == 0 {
		l.MinBuffers = 1
	}
	if s.layout == l {
		return
	}
	s.layout = l
	s.post(codec.Event{Kind: codec.Format, Layout: l})
}

// drainBuffers haalt de buffers op die de firmware teruggeeft. input=true is
// de ring met verwerkte invoer, false die met resultaten.
func (s *session) drainBuffers(r *ring, input bool) {
	var buf [bfSize + 32]byte
	for {
		code, n, ok, err := r.recv(buf[:])
		if err != nil {
			s.fail(err)
			return
		}
		if !ok {
			return
		}
		body := buf[:n]
		le := binary.LittleEndian
		if n < 24 {
			continue
		}
		s.stat.returns++
		h := s.take(le.Uint64(body[bfHostHandle:]))
		if h == nil {
			s.stat.unknown++
			continue // al opgeruimd, bijvoorbeeld door een flush
		}
		if !input && s.outHold {
			// Teruggegeven door de flush, niet gedecodeerd. Terug in de
			// wachtrij; hij gaat opnieuw naar de firmware zodra de poort weer
			// open is, op zijn vaste plek (zie place()) en met een vers handvat.
			s.stat.flushBack++
			s.outPend = append(s.outPend, h.buf)
			continue
		}
		ev := codec.Event{Buf: h.buf, Tag: h.tag}
		switch {
		case input:
			ev.Kind = codec.Consumed
		case code == bufFrame && n >= bfSize:
			// De firmware neemt de tijdstempel van de invoer mee naar het
			// uitvoerframe. h.tag hoort bij de lege uitvoerbuffer en is
			// normaal nul.
			ev.Tag = le.Uint64(body[bfUserTag:])
			flags := le.Uint32(body[bfFlags:])
			if flags&frFlagCorrupt != 0 {
				s.stat.corrupt++
				s.fail(errors.New("mve: decoder returned a corrupt frame"))
				return
			}
			switch {
			case flags&frFlagDecOnly != 0:
				s.stat.decodeOnly++
			case flags&frFlagRejected != 0:
				s.stat.rejected++
			}
			if flags&frFlagRefFrame != 0 {
				s.stat.refFrame++
			}
			ev.Kind = codec.Produced
			ev.Layout = s.layout
			ev.Layout.Width = int(le.Uint16(body[bfVisibleWidth:]))
			ev.Layout.Height = int(le.Uint16(body[bfVisibleHeight:]))
			ev.Bytes = s.layout.FrameSize
			// Een beeld dat de firmware wel decodeerde maar niet bedoelt om te
			// tonen (referentie buiten de weergave, of te klein/kapot) is geen
			// beeld voor de aanroeper. De buffer gaat wél terug — hij is van
			// hem — maar met nul bytes erin, zodat een archiveerder hem
			// overslaat in plaats van een verkeerd beeld weg te schrijven.
			if flags&(frFlagDecOnly|frFlagRejected) != 0 {
				ev.Bytes = 0
			}
			// EOS op een frame betekent "dit is het LAATSTE beeld", niet
			// "dit is geen beeld". Wie hier alleen Done post gooit het
			// slotbeeld weg — precies één beeld, elke stream opnieuw.
			if flags&frFlagEOS != 0 {
				s.stat.eos++
				ev = s.splitEOS(ev)
			}
		case code == bufBitstream && n >= bsSize:
			ev.Kind = codec.Produced
			ev.Bytes = uint64(le.Uint32(body[bsFilledLen:]))
			ev.Key = le.Uint32(body[bsFlags:])&bsFlagSyncFrame != 0
			if le.Uint32(body[bsFlags:])&bsFlagEOS != 0 {
				s.stat.eos++
				ev = s.splitEOS(ev)
			}
		default:
			continue
		}
		s.post(ev)
	}
}

// splitEOS knipt een laatste buffer in twee gebeurtenissen. EOS betekent "dit
// is het LAATSTE resultaat", niet "dit is geen resultaat": wie alleen Done
// post gooit het slotbeeld weg, elke stream opnieuw precies één. Draagt de
// buffer inhoud, dan gaat hij als resultaat naar buiten en is de Done die
// erop volgt leeg; is hij leeg, dan reist hij mee met de Done zodat de
// aanroeper zijn geheugen hoe dan ook terugkrijgt.
func (s *session) splitEOS(ev codec.Event) codec.Event {
	if ev.Bytes == 0 {
		return codec.Event{Kind: codec.Done, Buf: ev.Buf, Tag: ev.Tag}
	}
	s.post(ev)
	return codec.Event{Kind: codec.Done, Tag: ev.Tag}
}

// take haalt een buffer uit de boekhouding. De MAPPING blijft staan — de
// aanroeper biedt dezelfde buffer zo meteen weer aan en krijgt dan dezelfde
// plek terug; zie session.place().
func (s *session) take(handle uint64) *held {
	h, ok := s.bufs[handle]
	if !ok {
		return nil
	}
	delete(s.bufs, handle)
	if p, ok := s.placed[h.buf.PA]; ok {
		p.live = false
	}
	return h
}

// failRPC maakt een geweigerd geheugenverzoek zichtbaar. De firmware krijgt
// nul terug en gaat er vrolijk in schrijven — wat een halve seconde later een
// MMU ABORT geeft, zonder dat iemand weet waarom. Dít is waarom.
func (s *session) failRPC(what string, size uint32) {
	s.fail(fmt.Errorf("mve: firmware asked for %d MB and %s ran out",
		(uint64(size)+(1<<20)-1)>>20, what))
}

// pagesFor rondt een bytegrootte van de firmware af op hele pagina's. In 64
// bits: vlak onder 4GB loopt de optelling in 32 bits over naar bijna niets.
func pagesFor(size uint32) uint32 {
	return uint32((uint64(size) + pageSize - 1) / pageSize)
}

// serveRPC bedient het geheugenverzoek van de firmware. Een decoder vraagt
// zelf om zijn referentieframes zodra hij de stream kent — bij 4K HEVC gaat
// dat om honderden megabytes, en dat is precies waarom de arena van deze
// driver geen vaste kleine regio is zoals bij een NIC.
func (s *session) serveRPC() {
	if dev.Read32(s.rpc+rpcState) != rpcStateParam {
		return
	}
	call := dev.Read32(s.rpc + rpcCallID)
	var ret uint32
	switch call {
	case rpcPrintf:
		// De firmware praat; alleen interessant tijdens bring-up.

	case rpcAlloc:
		// mem_alloc: size u32 | max_size u32 | region u8 | log2_alignment u8.
		size := dev.Read32(s.rpc + rpcParams)
		maxSize := dev.Read32(s.rpc + rpcParams + 4)
		tail := dev.Read32(s.rpc + rpcParams + 8)
		region, align := uint8(tail), uint8(tail>>8)
		ret = s.rpcAllocate(size, maxSize, align, region == rpcRegionProtected)

	case rpcResize:
		va := dev.Read32(s.rpc + rpcParams)
		newSize := dev.Read32(s.rpc + rpcParams + 4)
		ret = s.rpcResize(va, newSize)

	case rpcFree:
		s.rpcRelease(dev.Read32(s.rpc + rpcParams))
	}

	dev.Write32(s.rpc+rpcParams, ret)
	dev.Write32(s.rpc+rpcSize, 4)
	dev.MB()
	dev.Write32(s.rpc+rpcState, rpcStateReturn)
	dev.MB()
	s.d.r.writeLSID(s.lsid, lsIRQHost, 1)
}

// rpcAllocate geeft de firmware geheugen en meldt op welk virtueel adres het
// staat. 0 betekent: niet gelukt — de firmware handelt dat zelf netjes af met
// een OUT_OF_MEMORY-fout, dus dit is geen paniekpad.
func (s *session) rpcAllocate(size, maxSize uint32, log2align uint8, protected bool) uint32 {
	if maxSize < size {
		maxSize = size
	}
	pages := pagesFor(size)
	reserve := pagesFor(maxSize)
	if pages == 0 {
		return 0
	}
	if log2align > maxLog2Align {
		s.fail(fmt.Errorf("mve: firmware asked for 2^%d alignment", log2align))
		return 0
	}
	if log2align < pageShift {
		log2align = pageShift
	}
	// Ruimte voor het uitlijnen ERBIJ reserveren, niet erin knabbelen. Wie de
	// uitlijning uit de gereserveerde span haalt laat het staartje van dit
	// blok over de volgende heen lopen — en dat is stille corruptie in het
	// referentiegeheugen van een decoder: groene blokken, geen foutmelding.
	slack := (uint32(1)<<log2align - 1) / pageSize
	va, err := s.takeVA(reserve+slack, protected)
	if err != nil {
		s.failRPC("virtual address space", maxSize)
		return 0
	}
	if align := uint32(1)<<log2align - 1; va&align != 0 {
		va = (va + align) &^ align
	}
	pa, err := s.d.arena.Alloc(pages, log2align)
	if err != nil {
		s.failRPC("arena", size)
		return 0
	}
	if err := s.mmu.mapRange(va, pa, pages, accessRW); err != nil {
		s.d.arena.Free(pa, pages)
		s.failRPC("page tables", size)
		return 0
	}
	s.rpcMem = append(s.rpcMem, owned{va: va, span: reserve, pages: pages, parts: []span{{pa, pages}}})
	s.stat.rpcAllocs++
	s.stat.rpcPages += pages
	return va
}

// rpcResize laat een eerder blok groeien. Krimpen negeren we: dat levert
// versnipperde arena op en de firmware vraagt er zelden om. De groei is een
// eigen stuk arena achter het blok, en dat stuk hoort bij hetzelfde blok: bij
// het vrijgeven gaat elk stuk terug met precies zijn eigen pagina's.
func (s *session) rpcResize(va, newSize uint32) uint32 {
	for i := range s.rpcMem {
		o := &s.rpcMem[i]
		if o.va != va {
			continue
		}
		want := pagesFor(newSize)
		if want <= o.pages {
			return va
		}
		if want > o.span {
			s.failRPC("the reserved span", newSize)
			return 0
		}
		extra := want - o.pages
		pa, err := s.d.arena.Alloc(extra, pageShift)
		if err != nil {
			s.failRPC("arena", newSize)
			return 0
		}
		if err := s.mmu.mapRange(va+o.pages*pageSize, pa, extra, accessRW); err != nil {
			s.d.arena.Free(pa, extra)
			s.failRPC("page tables", newSize)
			return 0
		}
		o.parts = append(o.parts, span{pa, extra})
		o.pages = want
		s.stat.rpcPages += extra
		return va
	}
	return 0
}

// rpcRelease geeft een blok terug.
func (s *session) rpcRelease(va uint32) {
	for i := range s.rpcMem {
		o := s.rpcMem[i]
		if o.va != va {
			continue
		}
		s.mmu.unmapRange(o.va, o.pages)
		o.free(s.d.arena)
		s.rpcMem = append(s.rpcMem[:i], s.rpcMem[i+1:]...)
		return
	}
}

// post zet een event klaar voor de aanroeper.
func (s *session) post(e codec.Event) {
	s.events = append(s.events, e)
}

// fail markeert de sessie als verloren. Eén Fault-event, en daarna neemt de
// sessie niets meer aan: doorgaan op een firmware die zijn eigen staat kwijt
// is levert alleen maar stille beeldfouten op.
func (s *session) fail(err error) {
	if s.failed != nil {
		return
	}
	s.failed = err
	s.post(codec.Event{Kind: codec.Fault, Err: err})
}
