package place

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"os"
	"testing"
)

// memSink is een partitie als []byte — de host-tegenhanger van dev.Copy c.s.
type memSink struct{ b []byte }

func (m *memSink) Write(off uint64, p []byte) { copy(m.b[off:], p) }
func (m *memSink) Read(p []byte, off uint64)  { copy(p, m.b[off:]) }
func (m *memSink) Zero(off, n uint64) {
	for i := uint64(0); i < n; i++ {
		m.b[off+i] = 0
	}
}

// De partitiegeometrie van de tests. linkBase is een canoniek slotadres.
const (
	tLinkBase = 0x8000_0000
	tAppRAM   = 96 << 20
	tLoOff    = 0
	tSlot     = 3
)

// buildRef plaatst image via het BESTAANDE pad: Build over het hele bestand,
// daarna segmenten kopiëren, BSS nullen, patches schrijven. Dit is de
// referentie waar de streamende plaatsing byte voor byte gelijk aan moet zijn.
func buildRef(t *testing.T, img []byte, abi uint64) (*memSink, *Plan) {
	t.Helper()
	plan, err := Build(bytes.NewReader(img), int64(len(img)), tLinkBase, tAppRAM, tLoOff, tAppRAM, tSlot, abi)
	if err != nil {
		t.Fatalf("Build (referentie): %v", err)
	}
	m := &memSink{b: make([]byte, tAppRAM)}
	for _, s := range plan.Segs {
		copy(m.b[s.Dst-tLinkBase:], img[s.Off:s.Off+s.Filesz])
		m.Zero(s.Dst-tLinkBase+s.Filesz, s.Memsz-s.Filesz)
	}
	for _, p := range plan.Patches {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], p.Val)
		m.Write(p.Addr-tLinkBase, b[:])
	}
	return m, plan
}

// streamIn voert image in stukken van chunk bytes door de streamende plaatser.
func streamIn(t *testing.T, img []byte, abi uint64, chunk int, appRAM uint64) (*memSink, *Plan, error) {
	t.Helper()
	m := &memSink{b: make([]byte, appRAM)}
	st := NewStream(m, int64(len(img)), tLinkBase, appRAM, tLoOff, tSlot, abi)
	for off := 0; off < len(img); off += chunk {
		end := off + chunk
		if end > len(img) {
			end = len(img)
		}
		if _, err := st.Write(img[off:end]); err != nil {
			return m, nil, err
		}
	}
	plan, err := st.Finish()
	return m, plan, err
}

// abiOf leest de ABI-versie uit het image zelf, zodat de tests niet aan een
// vast getal vastzitten (Build eist gelijkheid).
func abiOf(t *testing.T, img []byte) uint64 {
	t.Helper()
	for _, v := range []uint64{0, 1, 2, 3, 4, 5, 6, 7, 8} {
		if _, err := Build(bytes.NewReader(img), int64(len(img)), tLinkBase, tAppRAM, tLoOff, tAppRAM, tSlot, v); err == nil {
			return v
		}
	}
	t.Skip("testimage spreekt geen ABI die Build accepteert")
	return 0
}

// realImage geeft een echte tamago-app-image, of slaat de test over. Het pad
// komt uit PLACE_TEST_ELF; tools/test.sh zet 'm op een vers gebouwde apploader.
func realImage(t *testing.T) []byte {
	t.Helper()
	p := os.Getenv("PLACE_TEST_ELF")
	if p == "" {
		t.Skip("PLACE_TEST_ELF niet gezet — geen echte image om tegen te toetsen")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Skipf("PLACE_TEST_ELF=%s: %v", p, err)
	}
	return b
}

// segGeometry geeft (hoogste segment-top − linkBase, staartmaat) van een image:
// de twee getallen waar de exact passende schrapruimte om draait.
func segGeometry(t *testing.T, img []byte) (segTop, tail uint64) {
	t.Helper()
	f, err := elf.NewFile(bytes.NewReader(img))
	if err != nil {
		t.Fatal(err)
	}
	var fileEnd uint64
	for _, ph := range f.Progs {
		if ph.Type != elf.PT_LOAD {
			continue
		}
		if ph.Paddr+ph.Memsz-tLinkBase > segTop {
			segTop = ph.Paddr + ph.Memsz - tLinkBase
		}
		if ph.Off+ph.Filesz > fileEnd {
			fileEnd = ph.Off + ph.Filesz
		}
	}
	return segTop, uint64(len(img)) - fileEnd
}

// TestStreamPlaatstIdentiekAanStaging is de kern: dezelfde bytes op dezelfde
// plek als het staging-pad, zonder de image ooit compleet ergens neer te zetten.
// Meerdere blokgroottes, want de scheiding tussen segmenten en staart mag nooit
// van de toevallige knip van een TCP-segment afhangen.
func TestStreamPlaatstIdentiekAanStaging(t *testing.T) {
	img := realImage(t)
	abi := abiOf(t, img)
	ref, refPlan := buildRef(t, img, abi)

	for _, chunk := range []int{1 << 16, 4096, 8191, 1, 1 << 20} {
		if chunk == 1 && len(img) > 1<<20 {
			continue // byte-voor-byte alleen op kleine images: O(n·segs)
		}
		got, plan, err := streamIn(t, img, abi, chunk, tAppRAM)
		if err != nil {
			t.Fatalf("chunk %d: %v", chunk, err)
		}
		if plan.Entry != refPlan.Entry {
			t.Errorf("chunk %d: entry %#x, want %#x", chunk, plan.Entry, refPlan.Entry)
		}
		if len(plan.Patches) != len(refPlan.Patches) {
			t.Errorf("chunk %d: %d patches, want %d", chunk, len(plan.Patches), len(refPlan.Patches))
		}
		if !bytes.Equal(got.b, ref.b) {
			t.Fatalf("chunk %d: geplaatst geheugen wijkt af van het staging-pad (eerste verschil op %#x)",
				chunk, firstDiff(got.b, ref.b))
		}
	}
}

// TestStreamLaatGeenTweedeKopieAchter legt de winst vast: boven de hoogste
// segment-top is het app-RAM na afloop leeg — de staart is gewist, nergens
// staat nog een (deel)kopie van de image.
func TestStreamLaatGeenTweedeKopieAchter(t *testing.T) {
	img := realImage(t)
	abi := abiOf(t, img)
	segTop, _ := segGeometry(t, img)
	got, _, err := streamIn(t, img, abi, 1<<16, tAppRAM)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range got.b[segTop:] {
		if b != 0 {
			t.Fatalf("app-RAM niet leeg boven de segmenten, op %#x (%#x)", segTop+uint64(i), b)
		}
	}
}

// TestStreamWeigertEenTerugsprong bewaakt de enige aanname van het ontwerp.
// Zonder deze check zou een image met omgekeerde PT_LOAD-volgorde stil verkeerd
// geplaatst worden, en dat is precies de klasse fouten die dagen kost.
func TestStreamWeigertEenTerugsprong(t *testing.T) {
	img := append([]byte(nil), realImage(t)...)
	phoff := binary.LittleEndian.Uint64(img[0x20:])
	phentsize := uint64(binary.LittleEndian.Uint16(img[0x36:]))
	phnum := uint64(binary.LittleEndian.Uint16(img[0x38:]))

	var loads []uint64
	for i := uint64(0); i < phnum; i++ {
		if binary.LittleEndian.Uint32(img[phoff+i*phentsize:]) == 1 {
			loads = append(loads, phoff+i*phentsize)
		}
	}
	if len(loads) < 2 {
		t.Skip("testimage heeft maar één PT_LOAD")
	}
	// De offset van het tweede PT_LOAD vóór die van het eerste zetten.
	binary.LittleEndian.PutUint64(img[loads[1]+0x08:], 0)

	m := &memSink{b: make([]byte, tAppRAM)}
	st := NewStream(m, int64(len(img)), tLinkBase, tAppRAM, tLoOff, tSlot, 0)
	_, err := st.Write(img[:1<<16])
	if err == nil {
		_, err = st.Finish()
	}
	if err == nil {
		t.Fatal("een image met terugspringende PT_LOAD's werd geaccepteerd")
	}
	t.Logf("geweigerd met: %v", err)
}

// TestStreamWeigertEenAfgekapteStroom: een download die halverwege stopt mag
// nooit als geldige plaatsing eindigen.
func TestStreamWeigertEenAfgekapteStroom(t *testing.T) {
	img := realImage(t)
	abi := abiOf(t, img)
	m := &memSink{b: make([]byte, tAppRAM)}
	st := NewStream(m, int64(len(img)), tLinkBase, tAppRAM, tLoOff, tSlot, abi)
	if _, err := st.Write(img[:len(img)/2]); err != nil {
		t.Fatalf("halve stroom: %v", err)
	}
	if _, err := st.Finish(); err == nil {
		t.Fatal("een halve image werd als geldige plaatsing geaccepteerd")
	}
}

// TestStreamWeigertMeerDanAangekondigd: een server die doorpraat voorbij zijn
// Content-Length schrijft nooit voorbij de schrapruimte.
func TestStreamWeigertMeerDanAangekondigd(t *testing.T) {
	img := realImage(t)
	abi := abiOf(t, img)
	m := &memSink{b: make([]byte, tAppRAM)}
	st := NewStream(m, int64(len(img)), tLinkBase, tAppRAM, tLoOff, tSlot, abi)
	if _, err := st.Write(img); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write([]byte{0}); err == nil {
		t.Fatal("een byte voorbij de aangekondigde maat werd geaccepteerd")
	}
}

// TestStreamWeigertEenTeKrappePartitie: passen segmenten en staart niet samen,
// dan is dat een nette startfout — geen schrijven buiten het raam.
func TestStreamWeigertEenTeKrappePartitie(t *testing.T) {
	img := realImage(t)
	abi := abiOf(t, img)
	segTop, tail := segGeometry(t, img)
	krap := segTop + tail/2 // te klein voor segmenten + staart samen
	m := &memSink{b: make([]byte, krap)}
	st := NewStream(m, int64(len(img)), tLinkBase, krap, tLoOff, tSlot, abi)
	var err error
	for off := 0; off < len(img) && err == nil; off += 1 << 16 {
		end := off + 1<<16
		if end > len(img) {
			end = len(img)
		}
		_, err = st.Write(img[off:end])
	}
	if err == nil {
		_, err = st.Finish()
	}
	if err == nil {
		t.Fatal("segmenten + staart die niet samen passen werden geaccepteerd")
	}
	t.Logf("geweigerd met: %v", err)
}

func firstDiff(a, b []byte) int {
	for i := range a {
		if a[i] != b[i] {
			return i
		}
	}
	return -1
}
