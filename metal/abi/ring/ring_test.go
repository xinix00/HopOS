// Host-tests (go test, zonder tamago-tag) voor het SPSC-ringprotocol: de
// backing store is hier gewoon heap-geheugen — dev.Read64/Write64 zijn plain
// derefs en dev.MB is een no-op. Dit bewijst de record-, wrap- en
// verdedigingslogica; de barrière-plaatsing bewijst het board.
package ring

import (
	"bytes"
	"testing"
	"unsafe"

	"hop-os/metal/dev"
)

// testBufs houdt de backing-slices levend voor de hele testrun: een Ring kent
// alleen een uintptr, en zonder echte referentie mag de GC het geheugen weg
// geven terwijl de ring er nog op werkt.
var testBufs [][]byte

func newRing(size uint64) *Ring {
	buf := make([]byte, dataOff+int(size)+8)
	testBufs = append(testBufs, buf)
	base := uintptr(unsafe.Pointer(&buf[0]))
	base = (base + 7) &^ 7
	Init(base, size)
	return Open(base)
}

func TestRoundtrip(t *testing.T) {
	r := newRing(512)
	buf := make([]byte, 512)
	for _, n := range []int{0, 1, 7, 8, 9, 15, 16, 63} {
		p := bytes.Repeat([]byte{byte(n)}, n)
		if !r.Write(TypeLog, p) {
			t.Fatalf("Write(%d bytes) geweigerd op niet-volle ring", n)
		}
		typ, got, ok := r.ReadInto(buf)
		if !ok || typ != TypeLog || got != n || !bytes.Equal(buf[:got], p) {
			t.Fatalf("roundtrip %d bytes: typ=%d n=%d ok=%v", n, typ, got, ok)
		}
	}
	if _, _, ok := r.ReadInto(buf); ok {
		t.Fatal("lege ring leverde een record")
	}
}

func TestFIFOEnTypes(t *testing.T) {
	r := newRing(512)
	types := []uint32{TypeLog, TypeRPCReq, TypeRPCResp, TypeFrame}
	for i := range 8 {
		p := bytes.Repeat([]byte{byte(i)}, i+1)
		if !r.Write(types[i%len(types)], p) {
			t.Fatalf("Write %d geweigerd", i)
		}
	}
	buf := make([]byte, 512)
	for i := range 8 {
		typ, n, ok := r.ReadInto(buf)
		if !ok || typ != types[i%len(types)] || n != i+1 || buf[0] != byte(i) {
			t.Fatalf("record %d: typ=%d n=%d ok=%v buf[0]=%d", i, typ, n, ok, buf[0])
		}
	}
}

// De head hoort per record met recHdr+align8(len) op te schuiven — de
// 8-uitlijning is de ABI (een record wrapt nooit binnen een woord).
func TestHeadUitlijning(t *testing.T) {
	r := newRing(512)
	for _, n := range []int{0, 1, 7, 8, 9} {
		before := r.head()
		r.Write(TypeLog, make([]byte, n))
		want := before + recHdr + align8(uint64(n))
		if got := r.head(); got != want {
			t.Fatalf("head na Write(%d): %d, verwacht %d", n, got, want)
		}
	}
}

func TestVolEnDrain(t *testing.T) {
	r := newRing(128)
	p := make([]byte, 24) // need = 32
	for i := range 4 {
		if !r.Write(TypeLog, p) {
			t.Fatalf("Write %d geweigerd; ring hoort 4×32 te dragen", i)
		}
	}
	if r.Write(TypeLog, p) {
		t.Fatal("Write op volle ring gelukt")
	}
	buf := make([]byte, 128)
	if _, _, ok := r.ReadInto(buf); !ok {
		t.Fatal("volle ring leverde niets")
	}
	if !r.Write(TypeLog, p) {
		t.Fatal("Write geweigerd terwijl één record was vrijgelezen")
	}
}

func TestFitsEnTeGroot(t *testing.T) {
	r := newRing(128) // grens: recHdr+align8(n) ≤ 64 → n ≤ 56
	if !r.Fits(56) {
		t.Fatal("Fits(56) hoort te passen (need 64 = size/2)")
	}
	if r.Fits(57) {
		t.Fatal("Fits(57) hoort te weigeren (need 72 > size/2)")
	}
	if r.Write(TypeLog, make([]byte, 57)) {
		t.Fatal("te groot record geaccepteerd op lege ring")
	}
}

// Een record dat niet meer aaneengesloten tot de rand past krijgt een
// PAD-record over de staart en begint vooraan; de lezer ziet daar niets van.
func TestWrapMetPad(t *testing.T) {
	r := newRing(128)
	buf := make([]byte, 128)
	p24 := bytes.Repeat([]byte{0xAA}, 24)
	for range 3 { // head en tail naar 96
		r.Write(TypeLog, p24)
		r.ReadInto(buf)
	}
	p40 := bytes.Repeat([]byte{0xBB}, 40) // need 48 > contig 32 → PAD + wrap
	if !r.Write(TypeFrame, p40) {
		t.Fatal("wrappend record geweigerd terwijl er ruimte is")
	}
	typ, n, ok := r.ReadInto(buf)
	if !ok || typ != TypeFrame || n != 40 || !bytes.Equal(buf[:n], p40) {
		t.Fatalf("na wrap: typ=%d n=%d ok=%v", typ, n, ok)
	}
	if head, tail := r.head(), r.tail(); head != tail || head != 96+32+48 {
		t.Fatalf("indexen na wrap: head=%d tail=%d, verwacht beide %d", head, tail, 96+32+48)
	}
}

// Past het record exact tot de rand (need == contig), dan hoort er géén PAD
// tussen te zitten.
func TestWrapExactPassend(t *testing.T) {
	r := newRing(128)
	buf := make([]byte, 128)
	p24 := make([]byte, 24)
	for range 3 {
		r.Write(TypeLog, p24)
		r.ReadInto(buf)
	}
	if !r.Write(TypeLog, p24) { // need 32 == contig 32
		t.Fatal("exact passend record geweigerd")
	}
	if head := r.head(); head != 128 {
		t.Fatalf("head=%d, verwacht 128 (geen PAD)", head)
	}
	if _, n, ok := r.ReadInto(buf); !ok || n != 24 {
		t.Fatalf("exact passend record niet terug: n=%d ok=%v", n, ok)
	}
}

// Header claimt meer bytes dan de producer publiceerde → corrupt, en de ring
// blijft definitief dood (ook voor een later wél geldig record).
func TestCorruptHeaderBuitenPublicatie(t *testing.T) {
	r := newRing(128)
	dev.Write64(r.base+dataOff, 100|uint64(TypeLog)<<32) // len 100, need 112
	r.setHead(16)                                        // maar slechts 16 gepubliceerd
	buf := make([]byte, 128)
	if _, _, ok := r.ReadInto(buf); ok {
		t.Fatal("corrupt record geleverd")
	}
	if !r.Corrupt() {
		t.Fatal("ring niet corrupt gemarkeerd")
	}
	r.setHead(r.tail()) // "herstel" door de producer
	r.Write(TypeLog, []byte{1})
	if _, _, ok := r.ReadInto(buf); ok {
		t.Fatal("corrupte ring kwam weer tot leven")
	}
}

// Header die over de bufferrand heen claimt → corrupt.
func TestCorruptHeaderOverDeRand(t *testing.T) {
	r := newRing(128)
	buf := make([]byte, 128)
	p24 := make([]byte, 24)
	for range 3 { // tail naar 96; contig = 32
		r.Write(TypeLog, p24)
		r.ReadInto(buf)
	}
	dev.Write64(r.base+dataOff+96, 40|uint64(TypeLog)<<32) // need 48 > 32
	r.setHead(96 + 48)
	if _, _, ok := r.ReadInto(buf); ok || !r.Corrupt() {
		t.Fatalf("randoverschrijdende header: ok=%v corrupt=%v", ok, r.Corrupt())
	}
}

// Record groter dan buf van de lezer → corrupt (nooit een reuzenkopie).
func TestCorruptGroterDanBuf(t *testing.T) {
	r := newRing(512)
	r.Write(TypeLog, make([]byte, 64))
	if _, _, ok := r.ReadInto(make([]byte, 16)); ok || !r.Corrupt() {
		t.Fatalf("record > buf: ok=%v corrupt=%v", ok, r.Corrupt())
	}
}

// Een verzonnen, reusachtige head boven louter PAD-records (nullen) mag de
// consument niet in een miljardenlange skip-lus trekken: head-tail > size is
// per definitie corrupt. Zonder die guard hangt deze test.
func TestCorruptWeggelopenHead(t *testing.T) {
	r := newRing(128)
	r.setHead(1 << 62)
	if _, _, ok := r.ReadInto(make([]byte, 128)); ok || !r.Corrupt() {
		t.Fatalf("weggelopen head: ok=%v corrupt=%v", ok, r.Corrupt())
	}
}

// Spiegelbeeld voor de producer: een malafide consument die tail onzinnig
// zet mag Write niet in de war brengen — geen schrijven, geen paniek.
func TestWriteBijOnzinnigeTail(t *testing.T) {
	r := newRing(128)
	r.setTail(1 << 63)
	if r.Write(TypeLog, []byte{1}) {
		t.Fatal("Write accepteerde onmogelijke indexen")
	}
}

// Fuzz: willekeurige ringinhoud + willekeurige head mag ReadInto nooit laten
// panieken, hangen of meer records leveren dan er ooit in de buffer passen.
func FuzzReadInto(f *testing.F) {
	const size = 128
	f.Add([]byte{}, uint64(0))
	f.Add(bytes.Repeat([]byte{0}, size), uint64(1)<<62)
	f.Add([]byte{100, 0, 0, 0, 1, 0, 0, 0}, uint64(16))
	f.Fuzz(func(t *testing.T, data []byte, head uint64) {
		r := newRing(size)
		if len(data) > size {
			data = data[:size]
		}
		dev.Copy(r.base+dataOff, data)
		r.setHead(head)
		buf := make([]byte, size)
		for range 4 * size { // ruim boven de max. size/recHdr records
			if _, _, ok := r.ReadInto(buf); !ok {
				return
			}
		}
		t.Fatalf("ReadInto blijft leveren (head=%#x)", head)
	})
}
