package mve

import (
	"bytes"
	"encoding/binary"
	"runtime"
	"testing"
	"unsafe"

	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/codec"
)

// fakeVPU is een VPU van geheugen: een registerblok plus een firmware die de
// ringen bedient. Hij bewijst wat op ijzer het duurst te debuggen is — of de
// page tables kloppen, of de descriptorvelden op de goede plek staan, en of de
// sessie de firmware begrijpt — zonder dat er een bord voor hoeft te booten.
//
// De firmware doet zijn vertalingen met een ECHTE page walk door het geheugen
// (walk hieronder leest de tabellen zoals de hardware dat doet), niet via de
// Go-boekhouding van de driver. Anders zou de test blijven slagen terwijl de
// tabellen die de VPU leest fout staan, en dat is precies de fout die zich op
// ijzer voordoet als beeld vol groene blokken.
type fakeVPU struct {
	t    *testing.T
	base uintptr // registerblok
	rcsu uintptr
	hi   uintptr // bits boven de 40 die het PTE-formaat niet draagt
	mem  []byte  // de hele testwereld: registers, arena en buffers
	next uintptr // bump-toewijzer binnen mem
	stop chan struct{}
}

// world legt één aaneengesloten stuk geheugen neer voor registers, arena en
// buffers. Eén blok, omdat het PTE-formaat maar veertig bits fysiek adres
// draagt: op een ontwikkelmachine liggen heap-adressen hoger, en alleen als
// alles in hetzelfde blok ligt zijn die hoge bits voor de hele test gelijk.
// Op ijzer speelt dit niet — daar ís het fysieke adres wat de VPU leest.
func world(t *testing.T, pages int) *fakeVPU {
	t.Helper()
	mem := make([]byte, (pages+1)*pageSize)
	base := (uintptr(unsafe.Pointer(&mem[0])) + pageSize - 1) &^ uintptr(pageSize-1)
	f := &fakeVPU{t: t, mem: mem, next: base, hi: base &^ (1<<40 - 1)}
	dev.Clear(base, uint64(pages)*pageSize)
	return f
}

// take snijdt pagina's uit de testwereld.
func (f *fakeVPU) takePages(n int) uintptr {
	pa := f.next
	f.next += uintptr(n) * pageSize
	if f.next > uintptr(unsafe.Pointer(&f.mem[0]))+uintptr(len(f.mem)) {
		f.t.Fatal("testwereld is op")
	}
	return pa
}

// hwIDO6NMeasured is wat het HARDWARE_ID-register van de Orion O6N geeft.
const hwIDO6NMeasured = 0x56648002

func newFakeVPU(t *testing.T, ncores, nlsid int) *fakeVPU {
	t.Helper()
	f := world(t, 1024)
	f.initRegs(ncores, nlsid)
	return f
}

func (f *fakeVPU) initRegs(ncores, nlsid int) {
	t := f.t
	base := f.takePages(2)
	f.base, f.rcsu = base, base+pageSize
	f.stop = make(chan struct{})
	// De waarde die de O6N werkelijk teruggeeft (gemeten 22-09): het model in
	// de bovenste helft, een variant eronder. De test rekent met wat het ijzer
	// zegt, niet met wat wij ervan verwachtten.
	dev.Write32(base+regHardwareID, hwIDO6NMeasured)
	dev.Write32(base+regSVNRev, 0x1234)
	dev.Write32(base+regNCores, uint32(ncores))
	dev.Write32(base+regNLSID, uint32(nlsid))

	// Het enige stukje dat echt gelijktijdig moet zijn: het ijzer wist het
	// TERMINATE-bit zelf, en de driver wacht daarop.
	go func() {
		for {
			select {
			case <-f.stop:
				return
			default:
			}
			for id := 0; id < nlsid; id++ {
				off := base + uintptr(lsidBase+id*lsidStride) + lsTerminate
				if dev.Read32(off) != 0 {
					dev.Write32(off, 0)
				}
			}
			runtime.Gosched()
		}
	}()
	t.Cleanup(func() { close(f.stop) })
}

// walk vertaalt een firmware-adres met de tabellen zoals ze in het geheugen
// staan: L1 uit MMU_CTRL van het slot, dan L2, dan de pagina.
func (f *fakeVPU) walk(lsid int, va uint32) (uintptr, bool) {
	// MMU_CTRL draagt een volledige PTE; het adres zit op dezelfde plek als
	// in elke andere entry.
	ctrl := dev.Read32(f.base + uintptr(lsidBase+lsid*lsidStride) + lsMMUCtrl)
	if ctrl == 0 {
		return 0, false
	}
	l1 := uintptr(uint64(ctrl>>ptePAShift&ptePAMask)<<pageShift) | f.hi
	e := dev.Read32(l1 + uintptr(((va>>(pageShift+idxShift))&idxMask)*4))
	if e == 0 {
		return 0, false
	}
	l2 := uintptr(uint64(e>>ptePAShift&ptePAMask)<<pageShift) | f.hi
	p := dev.Read32(l2 + uintptr(((va>>pageShift)&idxMask)*4))
	if p == 0 {
		return 0, false
	}
	return uintptr(uint64(p>>ptePAShift&ptePAMask)<<pageShift) | f.hi + uintptr(va&(pageSize-1)), true
}

// ringAt geeft de twee pagina's van een ringpaar, gevonden via de page walk —
// dus precies zoals de firmware ze zou vinden.
func (f *fakeVPU) ringAt(lsid int, hostVA, mveVA uint32) *fakeFW {
	host, ok1 := f.walk(lsid, hostVA)
	mve, ok2 := f.walk(lsid, mveVA)
	if !ok1 || !ok2 {
		f.t.Fatalf("firmware vindt zijn ringen niet: %#x=%v %#x=%v", hostVA, ok1, mveVA, ok2)
	}
	return &fakeFW{r: &ring{host: host, mve: mve, csum: true}}
}

// testFirmware levert de blobs die deze test gebruikt.
type testFirmware struct{ bin []byte }

func (t testFirmware) Load(string) ([]byte, error) { return t.bin, nil }

// decodeSetup opent een HEVC-decodesessie op een nagebootste VPU.
func decodeSetup(t *testing.T, pixel codec.Pixel) (*Device, *session, *fakeVPU, *Arena) {
	t.Helper()
	f := newFakeVPU(t, 4, 2)
	// Ruim: de firmware vraagt in deze test zelf geheugen op, net als echt.
	arena := NewArena(f.takePages(512), 512*pageSize)
	bin := fwBlob(2*pageSize, 0x4d000, []int{0, 1})
	d, err := Probe(f.base, f.rcsu, arena, testFirmware{bin})
	if err != nil {
		t.Fatal(err)
	}
	ses, err := d.Open(codec.Config{Codec: codec.HEVC, Dir: codec.Decode, Pixel: pixel})
	if err != nil {
		t.Fatal(err)
	}
	return d, ses.(*session), f, arena
}

func TestProbeLeestDeGeometrieEnWeigertVreemdIjzer(t *testing.T) {
	f := newFakeVPU(t, 4, 2)
	arena := NewArena(f.takePages(64), 64*pageSize)
	d, err := Probe(f.base, f.rcsu, arena, testFirmware{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Cores() != 4 || d.Sessions() != 2 {
		t.Errorf("geometrie: %d cores, %d sessies", d.Cores(), d.Sessions())
	}
	if dev.Read32(f.base+regEnable) != 1 {
		t.Error("scheduler staat niet aan na probe")
	}
	if got := dev.Read32(f.base + regJobQueue); got != emptyJobQueue {
		t.Errorf("job queue niet leeggemaakt: %#x", got)
	}

	dev.Write32(f.base+regHardwareID, 0x5650_0000) // een oudere Mali-V500
	if _, err := Probe(f.base, f.rcsu, arena, testFirmware{}); err == nil {
		t.Error("driver accepteerde ijzer dat hij niet kent")
	}
}

func TestOpenZetDeSessieOpHetIjzer(t *testing.T) {
	d, s, f, _ := decodeSetup(t, codec.NV12)
	defer s.Close()

	id := s.lsid
	if got := dev.Read32(f.base + uintptr(lsidBase+id*lsidStride) + lsAlloc); got != allocNonProtected {
		t.Errorf("ALLOC = %d", got)
	}
	if got := dev.Read32(f.base + uintptr(lsidBase+id*lsidStride) + lsSched); got != 1 {
		t.Error("sessie is niet ingepland")
	}
	if got := dev.Read32(f.base + uintptr(lsidBase+id*lsidStride) + lsNProt); got != 1 {
		t.Error("sessie draait niet in de gewone (niet-beveiligde) modus")
	}
	if got := dev.Read32(f.base+uintptr(lsidBase+id*lsidStride)+lsCtrl) >> ctrlMaxCoresShift & 0xf; got != 1 {
		t.Errorf("maxcores = %d, verwacht 1", got)
	}
	// De job moet in een slot staan, met deze sessie en één core.
	q := dev.Read32(f.base + regJobQueue)
	var found bool
	for i := 0; i < jobSlots; i++ {
		if jobSlotLSID(q, i) == uint32(id) {
			if ncores := (q >> (i*8 + 4)) & 0xf; ncores != 1 {
				t.Errorf("job vraagt %d cores", ncores)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("sessie staat niet in de job queue: %#x", q)
	}

	// En de firmware moet zijn startsein en zijn eigen code vinden.
	msg := f.ringAt(id, vaMsgInQ, vaMsgOutQ)
	if code, _ := msg.take(t); code != reqGo {
		t.Errorf("eerste bericht is %d, verwacht GO (%d)", code, reqGo)
	}
	text, ok := f.walk(id, fwTextBase)
	if !ok {
		t.Fatal("firmware staat niet op zijn laadadres")
	}
	var head [4]byte
	dev.CopyOut(head[:], text)
	if binary.LittleEndian.Uint32(head[:]) != 0xeb5e {
		t.Errorf("op 0x1000 staat %x, niet het begin van de firmware", head)
	}

	// Twee sessies passen op dit ijzer, een derde niet.
	s2, err := d.Open(codec.Config{Codec: codec.H264, Dir: codec.Decode, Pixel: codec.NV12})
	if err != nil {
		t.Fatalf("tweede sessie: %v", err)
	}
	defer s2.Close()
	if _, err := d.Open(codec.Config{Codec: codec.VP9, Dir: codec.Decode, Pixel: codec.NV12}); err != codec.ErrBusy {
		t.Errorf("derde sessie gaf %v, verwacht ErrBusy", err)
	}
}

func TestDecodeLooptVanBitstreamNaarFrame(t *testing.T) {
	_, s, f, _ := decodeSetup(t, codec.NV12)
	defer s.Close()
	id := s.lsid
	msg := f.ringAt(id, vaMsgInQ, vaMsgOutQ)
	bin := f.ringAt(id, vaBufInQ, vaBufInRQ)
	bout := f.ringAt(id, vaBufOutQ, vaBufOutRQ)
	msg.take(t) // het GO van Open

	// De firmware heeft de headers gelezen en meldt wat het wordt: 4K, 8 bit,
	// 4:2:0, en hij wil er zes tegelijk kunnen vasthouden.
	seq := make([]byte, 8)
	seq[1], seq[2], seq[3], seq[4] = chromaYUV420, 8, 8, 6
	msg.give(respSeqParams, seq)
	alloc := make([]byte, 20)
	binary.LittleEndian.PutUint16(alloc[0:], 3840)
	binary.LittleEndian.PutUint16(alloc[2:], 2176) // afgerond op macroblokken
	binary.LittleEndian.PutUint16(alloc[16:], 0)
	binary.LittleEndian.PutUint16(alloc[18:], 16) // 2176 - 16 = 2160 zichtbaar
	msg.give(respFrameAllocParm, alloc)

	ev, ok := s.Poll()
	if !ok || ev.Kind != codec.Format {
		t.Fatalf("geen formaat-event: %+v ok=%v", ev, ok)
	}
	// Vóór het eerste frame is de gealloceerde maat wat we weten: de
	// firmware meldt hier hoe groot een buffer moet zijn, niet wat er
	// zichtbaar van is. Dat laatste komt per beeld mee (zie onderaan).
	l := ev.Layout
	if l.Width != 3840 || l.Height != 2176 {
		t.Errorf("layout meldt %dx%d, verwacht de gealloceerde maat", l.Width, l.Height)
	}
	if l.AllocWidth != 3840 || l.AllocHeight != 2176 {
		t.Errorf("alloc %dx%d", l.AllocWidth, l.AllocHeight)
	}
	if want := uint64(3840 * 2176 * 3 / 2); l.FrameSize != want {
		t.Errorf("framegrootte %d, verwacht %d", l.FrameSize, want)
	}
	if l.MinBuffers != 6 {
		t.Errorf("minbuffers %d", l.MinBuffers)
	}
	if l.Planes[0].Stride != 3840 || l.Planes[1].Off != 3840*2176 {
		t.Errorf("vlakken: %+v", l.Planes)
	}

	// Voer een stukje bitstream in.
	inBuf, inMem := f.buffer(2)
	copy(inMem, []byte{0, 0, 1, 0x26, 0x01, 0x02})
	if err := s.Feed(inBuf, 6, 0, 0xCAFE); err != nil {
		t.Fatal(err)
	}
	code, desc := bin.take(t)
	if code != bufBitstream {
		t.Fatalf("invoer kwam aan als code %d", code)
	}
	le := binary.LittleEndian
	if got := le.Uint64(desc[bsUserTag:]); got != 0xCAFE {
		t.Errorf("tag = %#x", got)
	}
	if got := le.Uint32(desc[bsFilledLen:]); got != 6 {
		t.Errorf("gevuld = %d", got)
	}
	// De firmware leest de bytes op het adres uit de descriptor: dat is de
	// echte proef op de MMU-mapping van een buffer van de aanroeper.
	src, ok := f.walk(id, le.Uint32(desc[bsBufAddr:]))
	if !ok {
		t.Fatal("firmware kan de invoerbuffer niet vinden")
	}
	got := make([]byte, 6)
	dev.CopyOut(got, src)
	if !bytes.Equal(got, inMem[:6]) {
		t.Errorf("firmware leest %x, de app schreef %x", got, inMem[:6])
	}

	// De firmware heeft na de streamparameters zijn uitvoerpoort stilgezet en
	// wacht op een output-flush. Die moet de driver uit zichzelf gestuurd
	// hebben; zonder dat antwoord blijft elke aangeboden buffer liggen.
	var flushed bool
	for !msg.empty() {
		if code, _ := msg.take(t); code == reqOutputFlush {
			flushed = true
		}
	}
	if !flushed {
		t.Fatal("driver stuurde geen output-flush na de streamparameters")
	}

	// Bied een framebuffer aan; een klein frame, want de test hoeft geen 12MB
	// te verzetten om het pad te bewijzen.
	outBuf, outMem := f.buffer(4)
	if err := s.Offer(outBuf); err != nil {
		t.Fatal(err)
	}
	if bout.empty() {
		// Goed zo: de poort staat nog stil.
	} else {
		t.Fatal("driver bood een buffer aan terwijl de uitvoerpoort stilstond")
	}
	msg.give(respOutputFlushed, nil)
	s.Poll()

	code, fdesc := bout.take(t)
	if code != bufFrame {
		t.Fatalf("uitvoer kwam aan als code %d", code)
	}
	if got := le.Uint16(fdesc[bfFormat:]); got != fmtNV12 {
		t.Errorf("formaat %#x, verwacht NV12 %#x", got, fmtNV12)
	}

	// De firmware "decodeert": hij schrijft zijn pixels op het luma-adres uit
	// de descriptor en geeft de buffer terug met de zichtbare maat erin.
	dst, ok := f.walk(id, le.Uint32(fdesc[bfPlaneTop:]))
	if !ok {
		t.Fatal("firmware kan de framebuffer niet vinden")
	}
	dev.Copy(dst, []byte{0x10, 0x20, 0x30, 0x40})
	le.PutUint16(fdesc[bfVisibleWidth:], 3840)
	le.PutUint16(fdesc[bfVisibleHeight:], 2160)
	le.PutUint64(fdesc[bfUserTag:], 0xCAFE)
	bout.give(bufFrame, fdesc)
	bin.give(bufBitstream, desc)

	// En dat moet de aanroeper terugzien: zijn invoer vrij, zijn frame gevuld.
	var consumed, produced bool
	for i := 0; i < 8; i++ {
		ev, ok := s.Poll()
		if !ok {
			break
		}
		switch ev.Kind {
		case codec.Consumed:
			consumed = true
			if ev.Buf != inBuf || ev.Tag != 0xCAFE {
				t.Errorf("verkeerde invoer terug: %+v", ev)
			}
		case codec.Produced:
			produced = true
			if ev.Tag != 0xCAFE {
				t.Errorf("input timestamp lost: %x", ev.Tag)
			}
			if ev.Buf != outBuf {
				t.Errorf("verkeerd frame terug: %+v", ev)
			}
			if ev.Layout.Width != 3840 || ev.Layout.Height != 2160 {
				t.Errorf("frame meldt %dx%d", ev.Layout.Width, ev.Layout.Height)
			}
		case codec.Fault:
			t.Fatalf("sessie viel om: %v", ev.Err)
		}
	}
	if !consumed || !produced {
		t.Errorf("consumed=%v produced=%v", consumed, produced)
	}
	if !bytes.Equal(outMem[:4], []byte{0x10, 0x20, 0x30, 0x40}) {
		t.Errorf("de pixels landden niet in de buffer van de app: %x", outMem[:4])
	}
}

func TestFirmwareVraagtGeheugenEnKrijgtHet(t *testing.T) {
	_, s, f, arena := decodeSetup(t, codec.NV12)
	defer s.Close()
	_, vrijVoor := arena.Pages()

	// Zo vraagt een decoder zijn referentieframes: een blok nu, ruimte om te
	// groeien, uitgelijnd op 64KB.
	rpc, ok := f.walk(s.lsid, vaRPC)
	if !ok {
		t.Fatal("firmware vindt zijn RPC-pagina niet")
	}
	dev.Write32(rpc+rpcCallID, rpcAlloc)
	dev.Write32(rpc+rpcParams, 8*pageSize)
	dev.Write32(rpc+rpcParams+4, 32*pageSize)
	dev.Write32(rpc+rpcParams+8, uint32(rpcRegionFrameBuf)|16<<8)
	dev.Write32(rpc+rpcState, rpcStateParam)

	s.Poll()

	if got := dev.Read32(rpc + rpcState); got != rpcStateReturn {
		t.Fatalf("RPC-staat %d, verwacht RETURN", got)
	}
	va := dev.Read32(rpc + rpcParams)
	if va == 0 {
		t.Fatal("firmware kreeg geen geheugen")
	}
	if va&0xFFFF != 0 {
		t.Errorf("blok op %#x is niet op 64KB uitgelijnd", va)
	}
	if va < vaSplitV3 || va >= vaFrameEndV3 {
		t.Errorf("blok op %#x ligt buiten de framebuffer-regio", va)
	}
	pa, ok := f.walk(s.lsid, va)
	if !ok {
		t.Fatal("het toegewezen blok staat niet in de page tables")
	}
	dev.Write32(pa, 0x5A5A5A5A) // de firmware moet erin kunnen schrijven

	// Groeien binnen de gereserveerde ruimte moet hetzelfde adres opleveren,
	// want de firmware houdt de oude inhoud vast.
	dev.Write32(rpc+rpcCallID, rpcResize)
	dev.Write32(rpc+rpcParams, va)
	dev.Write32(rpc+rpcParams+4, 20*pageSize)
	dev.Write32(rpc+rpcState, rpcStateParam)
	s.Poll()
	if got := dev.Read32(rpc + rpcParams); got != va {
		t.Errorf("na resize adres %#x, was %#x", got, va)
	}
	if _, ok := f.walk(s.lsid, va+19*pageSize); !ok {
		t.Error("de bijgekomen pagina's staan niet in de tabel")
	}
	if dev.Read32(pa) != 0x5A5A5A5A {
		t.Error("resize gooide de oude inhoud weg")
	}

	// En bij het sluiten moet ALLES terug in de arena: de page tables, de
	// firmware, de ringen en het geheugen dat de firmware zelf vroeg.
	s.Close()
	total, vrijNa := arena.Pages()
	if vrijNa != total {
		t.Errorf("na sluiten %d van %d vrij (vóór de RPC-aanvraag: %d) — de sessie lekt",
			vrijNa, total, vrijVoor)
	}
}

func TestSessieWeigertVerderNaEenFirmwareFout(t *testing.T) {
	_, s, f, _ := decodeSetup(t, codec.NV12)
	defer s.Close()
	msg := f.ringAt(s.lsid, vaMsgInQ, vaMsgOutQ)
	msg.take(t)

	body := make([]byte, 4+16)
	binary.LittleEndian.PutUint32(body, 9) // de watchdog van de firmware
	copy(body[4:], "watchdog")
	msg.give(respError, body)

	ev, ok := s.Poll()
	if !ok || ev.Kind != codec.Fault {
		t.Fatalf("fout kwam niet door: %+v", ev)
	}
	buf, _ := f.buffer(1)
	if err := s.Feed(buf, 1, 0, 0); err == nil {
		t.Error("sessie nam na een fout nog werk aan")
	}
}

// buffer geeft een paginagelijnde buffer uit de testwereld plus het geheugen
// erachter, zodat een test kan zien wat de "firmware" erin schreef.
func (f *fakeVPU) buffer(pages int) (codec.Buffer, []byte) {
	pa := f.takePages(pages)
	off := int(pa - uintptr(unsafe.Pointer(&f.mem[0])))
	return codec.Buffer{PA: pa, Size: uint64(pages) * pageSize}, f.mem[off : off+pages*pageSize]
}

// TestBuffersLandenInDeJuisteFirmwareRegio legt de tweedeling vast die de
// stille uitvoerring veroorzaakte: bitstream hoort in de protected-regio,
// pixels in de framebuffer-regio, en die grens ligt bij host-interface v3 op
// 0x70000000. Eén regio voor allebei lijkt te werken — de firmware leest de
// invoer gewoon — maar hij pakt dan nooit een uitvoerbuffer op.
func TestBuffersLandenInDeJuisteFirmwareRegio(t *testing.T) {
	_, s, f, _ := decodeSetup(t, codec.NV12)
	defer s.Close()
	bin := f.ringAt(s.lsid, vaBufInQ, vaBufInRQ)
	bout := f.ringAt(s.lsid, vaBufOutQ, vaBufOutRQ)
	le := binary.LittleEndian

	inBuf, _ := f.buffer(2)
	if err := s.Feed(inBuf, 6, 0, 1); err != nil {
		t.Fatal(err)
	}
	_, desc := bin.take(t)
	if va := le.Uint32(desc[bsBufAddr:]); va < vaProtectedBeg || va >= vaSplitV3 {
		t.Errorf("bitstream op %#x ligt buiten de protected-regio", va)
	}

	outBuf, _ := f.buffer(4)
	if err := s.Offer(outBuf); err != nil {
		t.Fatal(err)
	}
	_, fdesc := bout.take(t)
	if va := le.Uint32(fdesc[bfPlaneTop:]); va < vaSplitV3 || va >= vaFrameEndV3 {
		t.Errorf("framebuffer op %#x ligt buiten de framebuffer-regio", va)
	}
}

// TestOudeFirmwareKrijgtDeOudeGrens: dezelfde driver moet een v2-blob nog
// kunnen bedienen, en daar ligt de knip op 0x50000000.
func TestOudeFirmwareKrijgtDeOudeGrens(t *testing.T) {
	v2, v3 := regionsFor(2), regionsFor(3)
	if v2.protEnd != vaSplitV2 || v2.frameBeg != vaSplitV2 || v2.frameEnd != vaFrameEndV2 {
		t.Errorf("v2-indeling: %+v", v2)
	}
	if v3.protEnd != vaSplitV3 || v3.frameBeg != vaSplitV3 || v3.frameEnd != vaFrameEndV3 {
		t.Errorf("v3-indeling: %+v", v3)
	}
	if v2.protBeg != vaProtectedBeg || v3.protBeg != vaProtectedBeg {
		t.Error("de protected-regio begint in beide versies op 0x20000000")
	}
}

// TestUitvoerpoortWachtOpDeFlush legt de handshake vast die in
// mve_protocol_def.h staat: na SEQUENCE_PARAMETERS doet de firmware niets meer
// tot de host een output-flush stuurt, geeft dan al zijn uitvoerbuffers terug,
// en pas ná OUTPUT_FLUSHED mag de host opnieuw aanbieden. Die teruggave is
// GEEN gedecodeerd beeld — wie hem als frame doorgeeft levert de aanroeper een
// lege buffer als eerste beeld.
func TestUitvoerpoortWachtOpDeFlush(t *testing.T) {
	_, s, f, _ := decodeSetup(t, codec.NV12)
	defer s.Close()
	msg := f.ringAt(s.lsid, vaMsgInQ, vaMsgOutQ)
	bout := f.ringAt(s.lsid, vaBufOutQ, vaBufOutRQ)
	for !msg.empty() {
		msg.take(t) // GO en de eerste job
	}

	// Eén buffer die al bij de firmware ligt als de streamparameters komen.
	outBuf, _ := f.buffer(4)
	if err := s.Offer(outBuf); err != nil {
		t.Fatal(err)
	}
	code, first := bout.take(t)
	if code != bufFrame {
		t.Fatalf("uitvoer kwam aan als code %d", code)
	}

	seq := make([]byte, 8)
	seq[1], seq[2], seq[3], seq[4] = chromaYUV420, 8, 8, 3
	msg.give(respSeqParams, seq)
	s.Poll()

	var flushed bool
	for !msg.empty() {
		if c, _ := msg.take(t); c == reqOutputFlush {
			flushed = true
		}
	}
	if !flushed {
		t.Fatal("driver stuurde geen output-flush")
	}

	// Aanbieden mag nu niet doorgaan naar het ijzer.
	second, _ := f.buffer(4)
	if err := s.Offer(second); err != nil {
		t.Fatal(err)
	}
	if !bout.empty() {
		t.Error("driver bood aan terwijl de poort stilstond")
	}

	// De firmware geeft de buffer terug en meldt daarna pas dat hij leeg is.
	bout.give(bufFrame, first)
	msg.give(respOutputFlushed, nil)
	if ev, ok := s.Poll(); ok {
		t.Errorf("teruggegeven flush-buffer kwam naar buiten als %v", ev.Kind)
	}

	// En nu moeten ze er allebei alsnog in staan.
	var seen int
	for !bout.empty() {
		if c, _ := bout.take(t); c == bufFrame {
			seen++
		}
	}
	if seen != 2 {
		t.Errorf("%d buffers aangeboden na de flush, verwacht 2", seen)
	}
}

// TestLaatsteBeeldGaatNietVerlorenOpEOS: de firmware zet de EOS-vlag ÓP het
// laatste beeld. Wie daar alleen een Done van maakt levert elke stream één
// beeld te weinig — en dat is bij archiveren precies het beeld dat opvalt.
func TestLaatsteBeeldGaatNietVerlorenOpEOS(t *testing.T) {
	_, s, f, _ := decodeSetup(t, codec.NV12)
	defer s.Close()
	msg := f.ringAt(s.lsid, vaMsgInQ, vaMsgOutQ)
	bout := f.ringAt(s.lsid, vaBufOutQ, vaBufOutRQ)
	for !msg.empty() {
		msg.take(t)
	}

	// Streamparameters, flush afhandelen, dan één buffer aanbieden.
	seq := make([]byte, 8)
	seq[1], seq[2], seq[3], seq[4] = chromaYUV420, 8, 8, 1
	msg.give(respSeqParams, seq)
	alloc := make([]byte, 20)
	binary.LittleEndian.PutUint16(alloc[0:], 64)
	binary.LittleEndian.PutUint16(alloc[2:], 64)
	msg.give(respFrameAllocParm, alloc)
	s.Poll()
	for !msg.empty() {
		msg.take(t)
	}
	msg.give(respOutputFlushed, nil)
	s.Poll()

	outBuf, _ := f.buffer(4)
	if err := s.Offer(outBuf); err != nil {
		t.Fatal(err)
	}
	code, desc := bout.take(t)
	if code != bufFrame {
		t.Fatalf("uitvoer kwam aan als code %d", code)
	}

	// De firmware geeft hem terug MET beeld én de EOS-vlag erop.
	le := binary.LittleEndian
	le.PutUint32(desc[bfFlags:], frFlagEOS)
	le.PutUint16(desc[bfVisibleWidth:], 64)
	le.PutUint16(desc[bfVisibleHeight:], 64)
	bout.give(bufFrame, desc)

	var gotFrame, gotDone bool
	for i := 0; i < 8; i++ {
		ev, ok := s.Poll()
		if !ok {
			break
		}
		switch ev.Kind {
		case codec.Produced:
			gotFrame = true
			if ev.Buf.PA != outBuf.PA {
				t.Errorf("beeld kwam terug in een andere buffer: %#x", ev.Buf.PA)
			}
		case codec.Done:
			gotDone = true
			if !gotFrame {
				t.Error("Done kwam vóór het laatste beeld")
			}
		}
	}
	if !gotFrame {
		t.Error("het laatste beeld ging verloren op de EOS-vlag")
	}
	if !gotDone {
		t.Error("geen Done na het laatste beeld")
	}
}

func TestCorruptFrameFailsInsteadOfPublishingPixels(t *testing.T) {
	_, s, f, _ := decodeSetup(t, codec.P010)
	defer s.Close()
	msg := f.ringAt(s.lsid, vaMsgInQ, vaMsgOutQ)
	bout := f.ringAt(s.lsid, vaBufOutQ, vaBufOutRQ)
	for !msg.empty() {
		msg.take(t)
	}
	seq := make([]byte, 8)
	seq[1], seq[2], seq[3], seq[4] = chromaYUV420, 10, 10, 1
	msg.give(respSeqParams, seq)
	alloc := make([]byte, 20)
	binary.LittleEndian.PutUint16(alloc, 64)
	binary.LittleEndian.PutUint16(alloc[2:], 64)
	msg.give(respFrameAllocParm, alloc)
	s.Poll()
	for !msg.empty() {
		msg.take(t)
	}
	msg.give(respOutputFlushed, nil)
	s.Poll()
	b, _ := f.buffer(4)
	if err := s.Offer(b); err != nil {
		t.Fatal(err)
	}
	_, desc := bout.take(t)
	binary.LittleEndian.PutUint32(desc[bfFlags:], frFlagCorrupt)
	binary.LittleEndian.PutUint16(desc[bfVisibleWidth:], 64)
	binary.LittleEndian.PutUint16(desc[bfVisibleHeight:], 64)
	bout.give(bufFrame, desc)
	fault := false
	for range 8 {
		ev, ok := s.Poll()
		if !ok {
			break
		}
		if ev.Kind == codec.Produced {
			t.Fatal("corrupt pixels were published")
		}
		if ev.Kind == codec.Fault {
			fault = true
		}
	}
	if !fault {
		t.Fatal("no explicit decoder fault")
	}
}

// TestZelfdeBufferKrijgtZelfdeAdres legt vast wat een MMU ABORT op 4K kostte:
// een toewijzer die bij elke ronde een vers adres uitdeelt is na tachtig
// beelden door zijn regio heen en gaat dan over levende buffers heen. Dezelfde
// fysieke buffer hoort dezelfde plek te houden.
func TestZelfdeBufferKrijgtZelfdeAdres(t *testing.T) {
	_, s, f, _ := decodeSetup(t, codec.NV12)
	defer s.Close()
	msg := f.ringAt(s.lsid, vaMsgInQ, vaMsgOutQ)
	bout := f.ringAt(s.lsid, vaBufOutQ, vaBufOutRQ)
	for !msg.empty() {
		msg.take(t)
	}
	seq := make([]byte, 8)
	seq[1], seq[2], seq[3], seq[4] = chromaYUV420, 8, 8, 1
	msg.give(respSeqParams, seq)
	alloc := make([]byte, 20)
	binary.LittleEndian.PutUint16(alloc[0:], 64)
	binary.LittleEndian.PutUint16(alloc[2:], 64)
	msg.give(respFrameAllocParm, alloc)
	s.Poll()
	for !msg.empty() {
		msg.take(t)
	}
	msg.give(respOutputFlushed, nil)
	s.Poll()

	outBuf, _ := f.buffer(4)
	le := binary.LittleEndian
	var first uint32
	var firstPA uintptr
	for round := 0; round < 40; round++ {
		if err := s.Offer(outBuf); err != nil {
			t.Fatalf("ronde %d: %v", round, err)
		}
		code, desc := bout.take(t)
		if code != bufFrame {
			t.Fatalf("ronde %d: code %d", round, code)
		}
		va := le.Uint32(desc[bfPlaneTop:])
		if round == 0 {
			first = va
		} else if va != first {
			t.Fatalf("ronde %d gaf adres %#x, ronde 0 gaf %#x — de plek moet vast blijven",
				round, va, first)
		}
		pa, ok := f.walk(s.lsid, va)
		if !ok {
			t.Fatalf("ronde %d: %#x staat niet in de page tables", round, va)
		}
		if round == 0 {
			firstPA = pa
		} else if pa != firstPA {
			t.Fatalf("ronde %d: %#x wijst nu naar %#x in plaats van %#x", round, va, pa, firstPA)
		}
		// De firmware geeft hem terug; de mapping hoort te blijven staan.
		le.PutUint16(desc[bfVisibleWidth:], 64)
		le.PutUint16(desc[bfVisibleHeight:], 64)
		bout.give(bufFrame, desc)
		for i := 0; i < 4; i++ {
			if _, ok := s.Poll(); !ok {
				break
			}
		}
	}
}

// TestRuimteKomtVrijVoorEenANDEREBuffer: blijft een plek eeuwig staan, dan
// loopt een aanroeper die telkens verse buffers aanbiedt alsnog vast. Wat niet
// bij het ijzer ligt moet kunnen wijken.
func TestRuimteKomtVrijVoorEenANDEREBuffer(t *testing.T) {
	var r vaRegion
	r = vaRegion{beg: vaSplitV3, end: vaSplitV3 + 8*pageSize, next: vaSplitV3}
	a, ok := r.alloc(4)
	if !ok {
		t.Fatal("eerste toewijzing paste niet")
	}
	b, ok := r.alloc(4)
	if !ok {
		t.Fatal("tweede toewijzing paste niet")
	}
	if _, ok := r.alloc(4); ok {
		t.Fatal("regio gaf ruimte uit die er niet is")
	}
	r.put(a, 4)
	c, ok := r.alloc(4)
	if !ok || c != a {
		t.Errorf("vrijgegeven ruimte kwam niet terug: %#x (verwacht %#x)", c, a)
	}
	if b == a {
		t.Error("twee levende buffers kregen hetzelfde adres")
	}
}

// rpcCall doet één geheugenverzoek zoals de firmware dat doet: parameters in de
// RPC-pagina, staat op PARAM, en dan wachten tot de host RETURN zet.
func rpcCall(t *testing.T, s *session, f *fakeVPU, call uint32, params ...uint32) uint32 {
	t.Helper()
	rpc, ok := f.walk(s.lsid, vaRPC)
	if !ok {
		t.Fatal("firmware vindt zijn RPC-pagina niet")
	}
	dev.Write32(rpc+rpcCallID, call)
	for i, p := range params {
		dev.Write32(rpc+rpcParams+uintptr(i*4), p)
	}
	dev.Write32(rpc+rpcState, rpcStateParam)
	s.Poll()
	if got := dev.Read32(rpc + rpcState); got != rpcStateReturn {
		t.Fatalf("RPC %d: staat %d, verwacht RETURN", call, got)
	}
	return dev.Read32(rpc + rpcParams)
}

// TestTweeResizesEnVrijgevenKloppenTotDePagina: elke resize hangt een eigen
// stuk arena achter het blok. De tweede moet achter de EERSTE verder, niet
// eroverheen, en bij het vrijgeven moet elk stuk precies zijn eigen pagina's
// teruggeven — niet het eerste stuk voor de hele blokmaat.
func TestTweeResizesEnVrijgevenKloppenTotDePagina(t *testing.T) {
	_, s, f, arena := decodeSetup(t, codec.NV12)
	defer s.Close()
	total, _ := arena.Pages()
	fb := uint32(rpcRegionFrameBuf) | pageShift<<8

	// Twee blokken: het ene geeft de firmware zelf terug, het andere ruimt
	// Close op.
	var blocks [2]uint32
	for b := range blocks {
		_, vrijVoor := arena.Pages()
		va := rpcCall(t, s, f, rpcAlloc, 4*pageSize, 32*pageSize, fb)
		if va == 0 {
			t.Fatalf("blok %d: firmware kreeg geen geheugen", b)
		}
		blocks[b] = va
		_, vrijNaAlloc := arena.Pages() // inclusief een eventuele L2-pagina
		mark := func(from, to uint32) {
			for i := from; i < to; i++ {
				pa, ok := f.walk(s.lsid, va+i*pageSize)
				if !ok {
					t.Fatalf("blok %d: pagina %d staat niet in de tabel", b, i)
				}
				dev.Write32(pa, 0xB10C0000|i)
			}
		}
		mark(0, 4)
		for _, size := range []uint32{8, 16} {
			if got := rpcCall(t, s, f, rpcResize, va, size*pageSize); got != va {
				t.Fatalf("blok %d: resize naar %d pagina's gaf %#x", b, size, got)
			}
			mark(size/2, size)
		}
		seenPA := map[uintptr]bool{}
		for i := uint32(0); i < 16; i++ {
			pa, _ := f.walk(s.lsid, va+i*pageSize)
			if seenPA[pa] {
				t.Errorf("blok %d: pagina %d deelt zijn fysieke pagina met een andere", b, i)
			}
			seenPA[pa] = true
			if got := dev.Read32(pa); got != 0xB10C0000|i {
				t.Errorf("blok %d: pagina %d draagt %#x — een resize mapte over levend geheugen", b, i, got)
			}
		}
		if _, vrij := arena.Pages(); vrijNaAlloc-vrij != 12 {
			t.Errorf("blok %d: twee resizes kostten %d pagina's, verwacht 12", b, vrijNaAlloc-vrij)
		}
		if b == 0 {
			rpcCall(t, s, f, rpcFree, va)
			// Alles terug behalve de L2-pagina, en die is van de page table.
			if _, vrij := arena.Pages(); vrij != vrijNaAlloc+4 || vrij > vrijVoor {
				t.Errorf("na rpcFree %d vrij, verwacht %d", vrij, vrijNaAlloc+4)
			}
		}
	}

	s.Close()
	if _, vrij := arena.Pages(); vrij != total {
		t.Errorf("na sluiten %d van %d vrij — een geresized blok lekt", vrij, total)
	}
}

// TestPollNaCloseRaaktNietsMeerAan: na Close zijn de ringen en de RPC-pagina
// terug in de arena, misschien al van een andere sessie. Een Poll die dan nog
// pompt leest en schrijft in vreemd geheugen, en schrijft de deurbel van
// "slot -1" — register 0x1e0 in het globale blok.
func TestPollNaCloseRaaktNietsMeerAan(t *testing.T) {
	_, s, f, arena := decodeSetup(t, codec.NV12)
	rpc, ok := f.walk(s.lsid, vaRPC)
	if !ok {
		t.Fatal("firmware vindt zijn RPC-pagina niet")
	}
	msg := f.ringAt(s.lsid, vaMsgInQ, vaMsgOutQ)
	s.Close()
	_, vrij := arena.Pages()

	dev.Write32(rpc+rpcCallID, rpcAlloc)
	dev.Write32(rpc+rpcParams, 4*pageSize)
	dev.Write32(rpc+rpcState, rpcStateParam)
	msg.give(respStateChange, make([]byte, 4))

	if ev, ok := s.Poll(); ok {
		t.Errorf("gesloten sessie gaf nog een event: %+v", ev)
	}
	if got := dev.Read32(rpc + rpcState); got != rpcStateParam {
		t.Error("gesloten sessie bediende een RPC in vrijgegeven geheugen")
	}
	if _, got := arena.Pages(); got != vrij {
		t.Errorf("gesloten sessie alloceerde nog: %d vrij, was %d", got, vrij)
	}
	if got := dev.Read32(f.base + lsidBase - lsidStride + lsIRQHost); got != 0 {
		t.Errorf("register %#x = %d: deurbel van slot -1", lsidBase-lsidStride+lsIRQHost, got)
	}
}

// TestOnzinnigeMatenWordenGeweigerd: maten en uitlijningen komen van de
// firmware of de aanroeper en lopen in 32 bits makkelijk over. Wat overloopt
// moet geweigerd worden, niet als een paar pagina's toegewezen.
func TestOnzinnigeMatenWordenGeweigerd(t *testing.T) {
	r := vaRegion{beg: vaSplitV3, end: vaFrameEndV3, next: vaSplitV3}
	if va, ok := r.alloc(0x100001); ok {
		t.Errorf("0x100001 pagina's pasten op %#x — 4GB+4KB liep over tot 4KB", va)
	}
	if r.next != vaSplitV3 {
		t.Errorf("geweigerde aanvraag schoof de regio op naar %#x", r.next)
	}

	_, s, f, arena := decodeSetup(t, codec.NV12)
	defer s.Close()

	// Een buffer van net boven 4GB: afgekapt in 32 bits is dat één pagina.
	b, _ := f.buffer(1)
	b.Size = 1<<32 + pageSize
	if err := s.Feed(b, 1, 0, 0); err == nil {
		t.Error("buffer groter dan de adresruimte werd aangenomen")
	}

	_, vrij := arena.Pages()
	fb := uint32(rpcRegionFrameBuf)
	for _, c := range []struct {
		naam                string
		size, maxSize, tail uint32
	}{
		{"uitlijning 2^44", 4 * pageSize, 4 * pageSize, fb | 44<<8},
		{"uitlijning 2^32", 4 * pageSize, 4 * pageSize, fb | 32<<8},
		{"max_size bijna 4GB", pageSize, 0xFFFFFFFF, fb | 16<<8},
		{"size bijna 4GB", 0xFFFFF001, 0, fb | 12<<8},
	} {
		if va := rpcCall(t, s, f, rpcAlloc, c.size, c.maxSize, c.tail); va != 0 {
			t.Errorf("%s: firmware kreeg %#x", c.naam, va)
		}
		if _, got := arena.Pages(); got != vrij {
			t.Errorf("%s: geweigerde aanvraag kostte %d pagina's", c.naam, vrij-got)
		}
	}
}
