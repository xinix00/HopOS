//go:build media

package slots

import (
	"fmt"
	"sync"

	"github.com/xinix00/HopOS/metal/v2/abi/hopabi"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/codec"
)

// De system-functies waarmee een taak het codec-blok gebruikt. Dit is de naad
// tussen een app en het ijzer, en de vorm ervan wordt bepaald door één getal:
// een 4K-beeld in P010 is 24MB, en bij 24fps is dat 597MB/s. Het slot-LAN
// piekt op 550MB/s. Die beelden KUNNEN dus niet over de system-calls, en ze
// horen er ook niet: ze staan al in het geheugen van de app.
//
// Dus draagt deze laag geen bytes maar aanwijzingen. De app noemt een stuk van
// zijn EIGEN partitie (afstand vanaf RamStart, lengte); HOP controleert dat het
// daarbinnen valt, rekent het om naar fysiek, en hangt het in de page tables
// van de codec. Dat is de grant én de isolatie in één stap: het ijzer kan per
// constructie niets aanraken wat niet van deze taak is.
//
// Wat HOP er wél bij doet is het cache-onderhoud. De VPU van de O6N is niet
// coherent (`_CCA = 0` in de DSDT) terwijl de partitie van een app gewoon
// gecached is. Zonder vegen leest de app een beeld dat half in zijn eigen cache
// staat en half in DRAM.

// codecHandles is de sessietabel van één lifecycle van een slot. Handvatten
// zijn per tabel genummerd en beginnen bij 1: nul is "geen sessie" en dat
// willen we kunnen onderscheiden van een geldig handvat.
//
// De tabel hangt aan de servicer en niet aan het slotnummer, om dezelfde reden
// als de mounts: een system-verbinding van de vorige huurder kan nog een
// verzoek in de lucht hebben terwijl het slot al opnieuw uitgegeven wordt
// (evictServicer wacht op run, niet op serveSystemConn). Zo'n verzoek landt in
// de tabel van zijn eigen servicer, die dan al dicht is — en nooit in die van
// de opvolger, wiens handvat 1 toevallig hetzelfde getal is.
type codecHandles struct {
	mu   sync.Mutex
	next uint32
	live map[uint32]codec.Session
	// held onthoudt per buffer bij welke sessie hij ligt en of hij als INVOER
	// is aangeboden. Bij een Consumed-event hoeft er niets geveegd te worden
	// (de app schreef, het ijzer las); bij een Produced-event juist wel.
	held map[uintptr]heldBuf
	// closed: de lifecycle is voorbij (evictServicer). Een Open die daarna
	// nog klaar komt sluit zijn sessie meteen weer.
	closed bool
}

// heldBuf is één buffer die bij het ijzer ligt.
type heldBuf struct {
	handle uint32
	input  bool
}

// add zet een verse sessie in de tabel. Is de tabel intussen gesloten, dan
// weigert hij: de aanroeper moet de sessie zelf sluiten.
func (t *codecHandles) add(ses codec.Session) (uint32, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return 0, false
	}
	if t.live == nil {
		t.live = map[uint32]codec.Session{}
	}
	t.next++
	t.live[t.next] = ses
	return t.next, true
}

// shut sluit de tabel en alles wat erin stond. Idempotent. Een Open die nu
// nog loopt ziet closed in add; wat vóór dit moment binnenkwam staat in live
// en gaat hier dicht. Daartussen past niets, want beide lopen onder t.mu.
func (t *codecHandles) shut() {
	t.mu.Lock()
	t.closed = true
	live := t.live
	t.live, t.held = nil, nil
	t.mu.Unlock()
	for _, ses := range live {
		CloseCodec(ses)
	}
}

// codecGrant rekent een stuk van de partitie van een slot om naar een fysieke
// buffer die het ijzer mag zien.
//
// Dit is de enige plek waar een adres van buiten HOP binnenkomt, dus hier
// hoort de argwaan. Drie dingen moeten kloppen en alle drie om dezelfde reden:
// wat hier doorheen komt wordt straks door een DMA-motor beschreven, en die
// vraagt niet nog eens of het mocht.
func codecGrant(slot int, off, n uint64) (codec.Buffer, error) {
	base, size, ok := partitionOf(slot)
	if !ok {
		return codec.Buffer{}, fmt.Errorf("slot %d has no partition", slot)
	}
	if n == 0 {
		return codec.Buffer{}, fmt.Errorf("buffer of zero bytes")
	}
	// Optellen mag niet omlopen: met off = 2^64-1 zou off+n weer klein worden
	// en de grenstoets halen.
	if off+n < off || off+n > size {
		return codec.Buffer{}, fmt.Errorf("buffer %#x+%d falls outside the %d MB partition", off, n, size>>20)
	}
	// Het codec-blok werkt met hele pagina's — zijn eigen MMU kent niets
	// fijners. Een buffer die halverwege een pagina begint zou de buren
	// meenemen, en die zijn van dezelfde app maar niet voor dit doel bedoeld.
	if off&(codecPageSize-1) != 0 || n&(codecPageSize-1) != 0 {
		return codec.Buffer{}, fmt.Errorf("buffer %#x+%d is not a whole number of %d-byte pages", off, n, codecPageSize)
	}
	return codec.Buffer{PA: uintptr(base + off), Size: n}, nil
}

// codecPageSize is de paginamaat van de codec-MMU. Vier kilobyte op de Linlon
// V8, en elk ander blok dat we later onder driver/codec hangen zal niet
// grover zijn dan de CPU zelf.
const codecPageSize = 4096

// codecServe bedient één codec-op namens een slot.
func (s *servicer) codecServe(req hopabi.Req) []byte {
	if !HasCodec() {
		return failWith(req, hopabi.StatusError, "this node has no codec hardware")
	}
	t := &s.codec

	switch req.Op {
	case hopabi.OpCodecOpen:
		a, err := hopabi.DecodeOpen(req.Data)
		if err != nil {
			return fail(req, err)
		}
		ses, err := OpenCodec(s.slot, codec.Config{
			Codec:  codec.Codec(a.Codec),
			Dir:    codec.Direction(a.Dir),
			Pixel:  codec.Pixel(a.Pixel),
			Width:  int(a.Width),
			Height: int(a.Height),
		})
		if err != nil {
			return fail(req, err)
		}
		// Een Open laadt firmware en kan dus even duren; is de taak intussen
		// opgeruimd, dan hoort deze sessie bij niemand meer.
		h, live := t.add(ses)
		if !live {
			CloseCodec(ses)
			return failWith(req, hopabi.StatusError, "slot is being released")
		}
		return ok(req, uint64(h), nil)

	case hopabi.OpCodecFeed:
		a, err := hopabi.DecodeFeed(req.Data)
		if err != nil {
			return fail(req, err)
		}
		ses := t.session(a.Handle)
		if ses == nil {
			return failWith(req, hopabi.StatusNoEnt, "no such codec session")
		}
		b, err := codecGrant(s.slot, req.Off, req.N)
		if err != nil {
			return failWith(req, hopabi.StatusDenied, err.Error())
		}
		if a.Filled > b.Size {
			return fail(req, fmt.Errorf("filled %d exceeds the %d-byte buffer", a.Filled, b.Size))
		}
		// De app schreef deze bytes met zijn cache aan; het ijzer leest DRAM.
		// Dus eerst uitschrijven, dan pas aanbieden.
		dev.Clean(b.PA, uintptr(b.Size))
		t.mark(b.PA, a.Handle, true)
		if err := ses.Feed(b, a.Filled, codec.Flag(a.Flags), a.Tag); err != nil {
			t.forget(b.PA)
			return fail(req, err)
		}
		return ok(req, a.Filled, nil)

	case hopabi.OpCodecOffer:
		a, err := hopabi.DecodeBuf(req.Data)
		if err != nil {
			return fail(req, err)
		}
		ses := t.session(a.Handle)
		if ses == nil {
			return failWith(req, hopabi.StatusNoEnt, "no such codec session")
		}
		b, err := codecGrant(s.slot, req.Off, req.N)
		if err != nil {
			return failWith(req, hopabi.StatusDenied, err.Error())
		}
		// Het ijzer gaat hierin SCHRIJVEN. Alles wat de app er nog vuil van in
		// zijn cache heeft staan moet nu weg: zou zo'n regel later uitgezet
		// worden, dan schrijft hij over een beeld heen dat er al lag.
		dev.CleanInv(b.PA, uintptr(b.Size))
		t.mark(b.PA, a.Handle, false)
		if err := ses.Offer(b); err != nil {
			t.forget(b.PA)
			return fail(req, err)
		}
		return ok(req, b.Size, nil)

	case hopabi.OpCodecPoll:
		a, err := hopabi.DecodeBuf(req.Data)
		if err != nil {
			return fail(req, err)
		}
		ses := t.session(a.Handle)
		if ses == nil {
			return failWith(req, hopabi.StatusNoEnt, "no such codec session")
		}
		return t.poll(req, ses, s.slot)

	case hopabi.OpCodecClose:
		a, err := hopabi.DecodeBuf(req.Data)
		if err != nil {
			return fail(req, err)
		}
		ses := t.take(a.Handle)
		if ses == nil {
			return failWith(req, hopabi.StatusNoEnt, "no such codec session")
		}
		CloseCodec(ses)
		return ok(req, 0, nil)
	}
	return failWith(req, hopabi.StatusError, "unknown codec op")
}

// session zoekt een sessie op zijn handvat.
func (t *codecHandles) session(h uint32) codec.Session {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.live[h]
}

// take haalt een sessie uit de tabel (voor Close), met de buffers die nog bij
// hem lagen: na Close zijn die weer van de app en komen ze nooit meer als
// event terug.
func (t *codecHandles) take(h uint32) codec.Session {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.live[h]
	delete(t.live, h)
	for pa, b := range t.held {
		if b.handle == h {
			delete(t.held, pa)
		}
	}
	return s
}

// mark onthoudt van welke kant een buffer kwam, zodat poll weet of er geveegd
// moet worden als hij terugkomt.
func (t *codecHandles) mark(pa uintptr, h uint32, input bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if t.held == nil {
		t.held = map[uintptr]heldBuf{}
	}
	t.held[pa] = heldBuf{handle: h, input: input}
}

func (t *codecHandles) forget(pa uintptr) {
	t.mu.Lock()
	delete(t.held, pa)
	t.mu.Unlock()
}

// wasInput meldt of deze buffer als invoer werd aangeboden, en haalt hem uit
// de boekhouding.
func (t *codecHandles) wasInput(pa uintptr) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	in := t.held[pa].input
	delete(t.held, pa)
	return in
}

// poll haalt op wat er klaarstaat en vertaalt het naar de draad. Eén call
// levert alles op wat er ligt: een app die per event een RPC zou doen betaalt
// bij 4K een round-trip per beeld, en dat is precies de kosten die deze naad
// moest vermijden.
func (t *codecHandles) poll(req hopabi.Req, ses codec.Session, slot int) []byte {
	base, _, have := partitionOf(slot)
	if !have {
		return fail(req, fmt.Errorf("slot %d has no partition", slot))
	}
	const maxEvents = 32
	out := make([]byte, 0, maxEvents*hopabi.EventLen)
	rec := make([]byte, hopabi.EventLen)
	n := 0
	for n < maxEvents {
		ev, more := ses.Poll()
		if !more {
			break
		}
		w, err := wireEvent(ev, base)
		if err != nil {
			// Het event is al uit de sessie gehaald en kan niet terug. Een fout
			// voor de hele poll zou het en alles hiervóór laten verdwijnen —
			// buffers die de app dan nooit terugziet. Dus gaat het als Fault
			// mee: een driver die zoiets oplevert is stuk, en precies dat zegt
			// een Fault.
			fmt.Printf("slot %d: %v\n", slot, err)
			w = hopabi.Event{Kind: hopabi.EventFault, Tag: ev.Tag}
			t.forget(ev.Buf.PA)
		} else if ev.Kind == codec.Produced && ev.Buf.Size > 0 && !t.wasInput(ev.Buf.PA) {
			// Een gevuld resultaat komt uit DRAM; de app leest het gecached.
			// Zijn regels moeten dus weg vóór hij kijkt, anders leest hij wat
			// er stond voordat het ijzer begon.
			dev.CleanInv(ev.Buf.PA, uintptr(ev.Buf.Size))
		} else if ev.Buf.PA != 0 {
			t.forget(ev.Buf.PA)
		}
		hopabi.EncodeEvent(rec, w)
		out = append(out, rec...)
		n++
	}
	return ok(req, uint64(n), out)
}

// wireEvent vertaalt één driver-event naar het wire-formaat.
func wireEvent(ev codec.Event, base uint64) (hopabi.Event, error) {
	w := hopabi.Event{Tag: ev.Tag, Bytes: ev.Bytes, Key: ev.Key}
	switch ev.Kind {
	case codec.Consumed:
		w.Kind = hopabi.EventConsumed
	case codec.Produced:
		w.Kind = hopabi.EventProduced
	case codec.Format:
		w.Kind = hopabi.EventFormat
	case codec.Done:
		w.Kind = hopabi.EventDone
	case codec.Fault:
		w.Kind = hopabi.EventFault
	default:
		return w, fmt.Errorf("codec: unknown event kind %d", ev.Kind)
	}
	if ev.Buf.PA != 0 {
		if uint64(ev.Buf.PA) < base {
			return w, fmt.Errorf("codec: buffer %#x lies below the partition", ev.Buf.PA)
		}
		w.Off = uint64(ev.Buf.PA) - base
		w.Size = ev.Buf.Size
	}
	l := ev.Layout
	w.Width, w.Height = uint16(l.Width), uint16(l.Height)
	w.Pixel = uint8(l.Pixel)
	for i, p := range l.Planes {
		w.Stride[i] = p.Stride
		w.Plane[i] = p.Off
	}
	// Bij een Format-event is er geen buffer; dan draagt Size de maat die een
	// buffer moet hebben en Bytes het aantal dat het ijzer tegelijk wil.
	if ev.Kind == codec.Format {
		w.Size = l.FrameSize
		w.Bytes = uint64(l.MinBuffers)
	}
	return w, nil
}
