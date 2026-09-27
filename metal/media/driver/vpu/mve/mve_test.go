package mve

import (
	"bytes"
	"encoding/binary"
	"testing"
	"unsafe"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// testArena geeft een arena over echt (host-)geheugen, paginagelijnd. De
// driver werkt overal met fysieke adressen; op de ontwikkelmachine is dat
// gewoon een stuk heap, en dat is precies genoeg om de tabellen, de indexen en
// het ringprotocol te bewijzen. Wat een test hier NIET bewijst is de
// barrière-plaatsing en het cachegedrag — dat blijft het bord.
func testArena(t *testing.T, pages int) (*Arena, []byte) {
	t.Helper()
	buf := make([]byte, (pages+1)*pageSize)
	base := (uintptr(unsafe.Pointer(&buf[0])) + pageSize - 1) &^ uintptr(pageSize-1)
	return NewArena(base, uint64(pages)*pageSize), buf
}

func TestArenaGeeftUitgelijndeAaneengeslotenPaginas(t *testing.T) {
	a, _ := testArena(t, 64)
	if total, free := a.Pages(); total != 64 || free != 64 {
		t.Fatalf("verse arena: %d/%d", free, total)
	}

	// Een 64KB-blok moet op een 64KB-grens landen, ook als er al iets vóór ligt.
	if _, err := a.Alloc(1, pageShift); err != nil {
		t.Fatal(err)
	}
	pa, err := a.Alloc(16, 16)
	if err != nil {
		t.Fatal(err)
	}
	if pa&0xFFFF != 0 {
		t.Errorf("64KB-aanvraag landde op %#x", pa)
	}
	if _, free := a.Pages(); free != 64-17 {
		t.Errorf("vrij = %d, verwacht %d", free, 64-17)
	}

	// Vrijgeven moet de ruimte echt teruggeven, ook midden in de bitmap.
	a.Free(pa, 16)
	if _, free := a.Pages(); free != 64-1 {
		t.Errorf("na free: vrij = %d", free)
	}
}

func TestArenaWeigertWatNietPast(t *testing.T) {
	a, _ := testArena(t, 8)
	if _, err := a.Alloc(9, pageShift); err == nil {
		t.Fatal("negen pagina's uit een arena van acht")
	}
	// Twee blokken van vier passen; drie niet meer.
	for i := 0; i < 2; i++ {
		if _, err := a.Alloc(4, pageShift); err != nil {
			t.Fatalf("blok %d: %v", i, err)
		}
	}
	if _, err := a.Alloc(1, pageShift); err == nil {
		t.Fatal("arena gaf meer uit dan hij heeft")
	}
}

func TestArenaWistWatHijUitgeeft(t *testing.T) {
	a, buf := testArena(t, 4)
	for i := range buf {
		buf[i] = 0xA5
	}
	pa, err := a.Alloc(2, pageShift)
	if err != nil {
		t.Fatal(err)
	}
	var got [8]byte
	dev.CopyOut(got[:], pa)
	if got != [8]byte{} {
		t.Errorf("uitgegeven pagina draagt oude bytes: %x", got)
	}
}

func TestArenaDubbeleFreeBlaastDeTellerNietOp(t *testing.T) {
	a, _ := testArena(t, 8)
	pa, err := a.Alloc(2, pageShift)
	if err != nil {
		t.Fatal(err)
	}
	a.Free(pa, 2)
	a.Free(pa, 2)
	// Meer vrijgeven dan er uitgegeven was: de twee pagina's erachter waren
	// nooit van iemand.
	a.Free(pa, 4)
	if total, free := a.Pages(); free != total {
		t.Errorf("vrij = %d van %d — een dubbele free telde dubbel", free, total)
	}
	// En de ruimte moet nog steeds precies één keer uit te geven zijn.
	for i := 0; i < 8; i++ {
		if _, err := a.Alloc(1, pageShift); err != nil {
			t.Fatalf("pagina %d: %v", i, err)
		}
	}
	if _, err := a.Alloc(1, pageShift); err == nil {
		t.Error("arena gaf een negende pagina uit")
	}
}

func TestArenaWeigertOnzinnigeUitlijning(t *testing.T) {
	// De uitlijning komt van de firmware (een u8). Vanaf 2^44 werd de stap
	// nul en liep Alloc eeuwig rond; vanaf 2^32 bestaat de grens niet voor
	// een VPU met 32-bit adressen.
	a, _ := testArena(t, 8)
	for _, align := range []uint8{32, 44, 255} {
		if _, err := a.Alloc(1, align); err == nil {
			t.Errorf("uitlijning 2^%d werd geaccepteerd", align)
		}
	}
	if _, free := a.Pages(); free != 8 {
		t.Errorf("geweigerde aanvragen kostten toch %d pagina's", 8-free)
	}
}

// seen geeft een adres zoals de VPU het uit een PTE terugleest: veertig bits.
func seen(pa uintptr) uintptr { return pa & (1<<40 - 1) }

func TestMMUVertaaltTweeNiveausDiep(t *testing.T) {
	a, _ := testArena(t, 64)
	m, err := newMMU(a)
	if err != nil {
		t.Fatal(err)
	}
	// Twee adressen die in verschillende L1-takken vallen: 0x1000 (tak 0) en
	// 0x70000000 (de framebuffer-regio van v3).
	pa1, err := m.alloc(fwTextBase, 2, accessExec)
	if err != nil {
		t.Fatal(err)
	}
	pa2, err := m.alloc(vaSplitV3, 1, accessRW)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		va   uint32
		want uintptr
	}{
		{fwTextBase, seen(pa1)},
		{fwTextBase + pageSize, seen(pa1 + pageSize)},
		{fwTextBase + 0x123, seen(pa1) + 0x123},
		{vaSplitV3, seen(pa2)},
	} {
		got, ok := m.lookup(c.va)
		if !ok || got != c.want {
			t.Errorf("va %#x → %#x (%v), verwacht %#x", c.va, got, ok, c.want)
		}
	}
	if _, ok := m.lookup(vaSplitV3 + pageSize); ok {
		t.Error("niet-gemapt adres gaf een vertaling")
	}
}

func TestMMUPTEDraagtAttribuutAdresEnRecht(t *testing.T) {
	// Het formaat is attr<<30 | pa[39:12]<<2 | ap. Een adres boven de 4GB moet
	// er heel doorheen komen: de VPU adresseert 40 bits fysiek, ook al is zijn
	// eigen adresruimte 32 bits.
	const pa = uintptr(0x1_2345_6000)
	got := pte(attrPrivate, pa, accessRW)
	if want := uint32(0x1_2345_6>>0)<<2 | accessRW; got != want {
		t.Errorf("pte = %#x, verwacht %#x", got, want)
	}
	if back := uintptr(uint64(got>>ptePAShift&ptePAMask) << pageShift); back != pa {
		t.Errorf("adres kwam terug als %#x", back)
	}
	if hi := pte(attrSharedRW, pa, accessRO); hi>>pteAttrShift != attrSharedRW {
		t.Errorf("attribuut ging verloren: %#x", hi)
	}
}

func TestMMUGeeftAllesTerugBijSluiten(t *testing.T) {
	a, _ := testArena(t, 64)
	m, err := newMMU(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.alloc(vaSplitV3, 8, accessRW); err != nil {
		t.Fatal(err)
	}
	if _, free := a.Pages(); free == 64 {
		t.Fatal("sessie hield niets vast")
	}
	m.destroy()
	if _, free := a.Pages(); free != 64 {
		t.Errorf("na destroy nog %d van 64 vrij — een gesloten sessie lekt", free)
	}
}

// fwBlob bouwt een geldige firmware-binary met een bss-bitmap.
func fwBlob(textLen int, bssStart uint32, bits []int) []byte {
	b := make([]byte, textLen)
	le := binary.LittleEndian
	le.PutUint32(b[0:], 0xeb5e)
	b[4], b[5] = 5, 3 // minor 5, major 3 — zoals de O6N-blobs
	copy(b[8:], "56648002TEST Decoder")
	copy(b[64:], "TESTDEC")
	copy(b[80:], "r0p0-sum0005")
	le.PutUint32(b[96:], uint32(textLen))
	le.PutUint32(b[100:], bssStart)
	var max int
	for _, i := range bits {
		b[108+i/8] |= 1 << (i % 8)
		if i+1 > max {
			max = i + 1
		}
	}
	le.PutUint32(b[104:], uint32(max))
	return b
}

func TestFirmwareKopKomtOvereenMetDeBlob(t *testing.T) {
	bin := fwBlob(3*pageSize+1, 0x4d000, []int{0, 3, 40})
	h, err := parseFW(bin)
	if err != nil {
		t.Fatal(err)
	}
	if h.ProtocolMajor != 3 || h.ProtocolMinor != 5 {
		t.Errorf("protocol v%d.%d", h.ProtocolMajor, h.ProtocolMinor)
	}
	if h.PartNumber != "TESTDEC" {
		t.Errorf("part number %q", h.PartNumber)
	}
	if h.textPages() != 4 {
		t.Errorf("textPages = %d, verwacht 4 (3 pagina's plus één byte)", h.textPages())
	}
	for _, c := range []struct {
		i    uint32
		want bool
	}{{0, true}, {1, false}, {3, true}, {40, true}, {41, false}, {9999, false}} {
		if got := h.bssPage(c.i); got != c.want {
			t.Errorf("bssPage(%d) = %v", c.i, got)
		}
	}
}

func TestFirmwareWeigertOnzin(t *testing.T) {
	for naam, maak := range map[string]func() []byte{
		"te kort": func() []byte { return make([]byte, 10) },
		"geen magic": func() []byte {
			b := fwBlob(pageSize, 0x1000, nil)
			b[0] = 0
			return b
		},
		"text groter dan blob": func() []byte {
			b := fwBlob(pageSize, 0x1000, nil)
			binary.LittleEndian.PutUint32(b[96:], pageSize*4)
			return b
		},
		"bss niet uitgelijnd": func() []byte {
			b := fwBlob(pageSize, 0x1000, nil)
			binary.LittleEndian.PutUint32(b[100:], 0x4d123)
			return b
		},
		"onbekend protocol": func() []byte {
			b := fwBlob(pageSize, 0x1000, nil)
			b[5] = 9
			return b
		},
	} {
		if _, err := parseFW(maak()); err == nil {
			t.Errorf("%s werd geaccepteerd", naam)
		}
	}
}

func TestFirmwareLandtOpDeJuisteAdressen(t *testing.T) {
	a, _ := testArena(t, 64)
	m, err := newMMU(a)
	if err != nil {
		t.Fatal(err)
	}
	bin := fwBlob(2*pageSize, 0x4d000, []int{0, 2})
	for i := fwHeaderLen; i < len(bin); i++ {
		bin[i] = byte(i)
	}
	h, err := parseFW(bin)
	if err != nil {
		t.Fatal(err)
	}
	pa, err := loadFW(m, bin, h)
	if err != nil {
		t.Fatal(err)
	}
	// De code staat op 0x1000 en de eerste bytes moeten kloppen.
	if got, ok := m.lookup(fwTextBase); !ok || got != seen(pa) {
		t.Fatalf("text staat op %#x (%v), verwacht %#x", got, ok, seen(pa))
	}
	got := make([]byte, 8)
	dev.CopyOut(got, pa)
	if !bytes.Equal(got, bin[:8]) {
		t.Errorf("text begint met %x, verwacht %x", got, bin[:8])
	}
	// De bss-pagina's uit de bitmap staan er, de niet-gezette niet.
	if _, ok := m.lookup(0x4d000); !ok {
		t.Error("bss-pagina 0 ontbreekt")
	}
	if _, ok := m.lookup(0x4d000 + pageSize); ok {
		t.Error("bss-pagina 1 stond niet in de bitmap maar is toch gemapt")
	}
	if _, ok := m.lookup(0x4d000 + 2*pageSize); !ok {
		t.Error("bss-pagina 2 ontbreekt")
	}
}
