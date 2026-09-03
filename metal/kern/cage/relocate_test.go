package cage

import (
	"encoding/binary"
	"testing"
)

// paOf haalt het fysieke adres uit een entry: de PPN is 44 bits (53:10), en
// daarboven wonen de uitgebreide attributen — zonder masker trek je die mee.
func paOf(e uint64) uint64 { return ((e >> 10) & (1<<44 - 1)) << 12 }

// entryAt leest één tabel-entry uit de uitgeschreven bytes.
func entryAt(b []byte, page, idx int) uint64 {
	var v uint64
	o := page*PageSize + idx*8
	for i := range 8 {
		v |= uint64(b[o+i]) << (8 * i)
	}
	return v
}

// Het echte geval: een slot ziet zichzelf op het canonieke linkadres, terwijl
// zijn partitie ergens anders staat. Dat ís waarvoor verplaatsen bestaat.
func TestRelocateLegtEenPartitieOpHetLinkadres(t *testing.T) {
	const (
		link  = 0x88000000 // waar élk app-image gelinkt is
		phys  = 0x8A000000 // de partitie die dít slot kreeg
		size  = 64 << 20
		table = 0x8BFF0000
	)
	r, err := Relocate(MapPlan{
		TableBase: table,
		Windows:   []MapWindow{{Link: link, Phys: phys, Size: size, R: true, W: true, X: true}},
	})
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}

	// Twee pagina's: de wortel plus één niveau (link en phys zitten in dezelfde GB).
	if len(r.Bytes) != 2*PageSize {
		t.Fatalf("tabel is %d bytes, want %d (wortel + één niveau)", len(r.Bytes), 2*PageSize)
	}
	if want := uint64(mapMode | table>>12); r.Root != want {
		t.Fatalf("Root = %#x, want %#x", r.Root, want)
	}

	// De wortel wijst naar het niveau eronder, en die entry is een pure
	// verwijzing: alleen Valid.
	gi := (link >> 30) & (mapEntries - 1)
	ptr := entryAt(r.Bytes, 0, int(gi))
	if ptr&(entRead|entWrite|entExec) != 0 {
		t.Fatalf("wortel-entry %#x heeft rechten — dan is het een blad, geen verwijzing", ptr)
	}
	if got, want := paOf(ptr), uint64(table+PageSize); got != want {
		t.Fatalf("wortel wijst naar %#x, want %#x", got, want)
	}

	// Elk blok van het venster staat op zijn plek, met de juiste rechten.
	for off := uint64(0); off < size; off += BlockSize {
		bi := ((link + off) >> 21) & (mapEntries - 1)
		e := entryAt(r.Bytes, 1, int(bi))
		if e&entValid == 0 {
			t.Fatalf("blok op link %#x is niet valid", link+off)
		}
		if got, want := paOf(e), uint64(phys+off); got != want {
			t.Fatalf("link %#x wijst naar %#x, want %#x", link+off, got, want)
		}
		if e&(entRead|entWrite|entExec) != entRead|entWrite|entExec {
			t.Fatalf("blok op link %#x mist rechten: %#x", link+off, e)
		}
		if e&entTouched != entTouched {
			t.Fatal("Seen/Dirty horen vooraf te staan — anders faultt de C906 op eigen geheugen")
		}
		if e&entUserCheck != 0 {
			t.Fatal("de user-bit staat aan — de onderste laag mag hier NOOIT bij")
		}
	}
}

// Twee slots, hetzelfde linkadres, verschillende partities: dat is de hele
// belofte van één artifact per architectuur.
func TestTweeSlotsDelenHetLinkadres(t *testing.T) {
	const link, size = 0x88000000, 32 << 20
	a, err := Relocate(MapPlan{TableBase: 0x8BFF0000,
		Windows: []MapWindow{{Link: link, Phys: 0x88000000, Size: size, R: true, W: true, X: true}}})
	if err != nil {
		t.Fatalf("slot 1: %v", err)
	}
	b, err := Relocate(MapPlan{TableBase: 0x8DFF0000,
		Windows: []MapWindow{{Link: link, Phys: 0x8A000000, Size: size, R: true, W: true, X: true}}})
	if err != nil {
		t.Fatalf("slot 2: %v", err)
	}
	bi := (link >> 21) & (mapEntries - 1)
	pa1 := paOf(entryAt(a.Bytes, 1, int(bi)))
	pa2 := paOf(entryAt(b.Bytes, 1, int(bi)))
	if pa1 == pa2 {
		t.Fatal("beide slots wijzen naar dezelfde partitie")
	}
	if pa1 != 0x88000000 || pa2 != 0x8A000000 {
		t.Fatalf("verkeerde partities: %#x en %#x", pa1, pa2)
	}
	if a.Root == b.Root {
		t.Fatal("beide slots hebben dezelfde map-wortel")
	}
}

func TestRelocateWeigertKrommePlannen(t *testing.T) {
	ok := MapWindow{Link: 0x88000000, Phys: 0x88000000, Size: BlockSize, R: true, W: true}
	cases := []struct {
		naam string
		plan MapPlan
	}{
		{"tabel niet uitgelijnd", MapPlan{TableBase: 0x8BFF0001, Windows: []MapWindow{ok}}},
		{"leeg plan", MapPlan{TableBase: 0x8BFF0000}},
		{"lengte nul", MapPlan{TableBase: 0x8BFF0000,
			Windows: []MapWindow{{Link: 0x88000000, Phys: 0x88000000, R: true}}}},
		{"link niet op blokgrens", MapPlan{TableBase: 0x8BFF0000,
			Windows: []MapWindow{{Link: 0x88001000, Phys: 0x88000000, Size: BlockSize, R: true}}}},
		{"phys niet op blokgrens", MapPlan{TableBase: 0x8BFF0000,
			Windows: []MapWindow{{Link: 0x88000000, Phys: 0x88001000, Size: BlockSize, R: true}}}},
		{"lengte niet op blokgrens", MapPlan{TableBase: 0x8BFF0000,
			Windows: []MapWindow{{Link: 0x88000000, Phys: 0x88000000, Size: PageSize, R: true}}}},
		{"geen rechten", MapPlan{TableBase: 0x8BFF0000,
			Windows: []MapWindow{{Link: 0x88000000, Phys: 0x88000000, Size: BlockSize}}}},
		{"schrijven zonder lezen", MapPlan{TableBase: 0x8BFF0000,
			Windows: []MapWindow{{Link: 0x88000000, Phys: 0x88000000, Size: BlockSize, W: true}}}},
		{"overlappende vensters", MapPlan{TableBase: 0x8BFF0000, Windows: []MapWindow{ok, ok}}},
	}
	for _, c := range cases {
		if _, err := Relocate(c.plan); err == nil {
			t.Errorf("%s: Relocate accepteerde een krom plan", c.naam)
		}
	}
}

// Een venster over een gigabyte-grens krijgt een tweede niveau — en de wortel
// moet dan naar twee verschillende tabellen wijzen.
func TestRelocateOverEenGigabyteGrens(t *testing.T) {
	const link = 0x3FE00000 // laatste 2MB van de eerste GB
	r, err := Relocate(MapPlan{TableBase: 0x88000000,
		Windows: []MapWindow{{Link: link, Phys: 0x88000000, Size: 4 << 20, R: true, X: true}}})
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	if len(r.Bytes) != 3*PageSize {
		t.Fatalf("tabel is %d bytes, want %d (wortel + twee niveaus)", len(r.Bytes), 3*PageSize)
	}
	g0 := entryAt(r.Bytes, 0, int((link>>30)&(mapEntries-1)))
	g1 := entryAt(r.Bytes, 0, int(((link+(2<<20))>>30)&(mapEntries-1)))
	if g0 == 0 || g1 == 0 || g0 == g1 {
		t.Fatalf("de twee gigabytes horen naar verschillende niveaus te wijzen: %#x en %#x", g0, g1)
	}
}

// Normaal RAM moet bufferable+cacheable zijn en MMIO juist niet: met MAEE aan is
// een pagina zonder die bits device-achtig, en dáár faultt een atomic op.
func TestRAMIsCacheableEnMMIONiet(t *testing.T) {
	r, err := Relocate(MapPlan{TableBase: 0x8BFF0000, Windows: []MapWindow{
		{Link: 0x88000000, Phys: 0x88000000, Size: BlockSize, R: true, W: true, X: true},
		{Link: 0x8A000000, Phys: 0x8A000000, Size: BlockSize, R: true, W: true, Device: true},
	}})
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	ram := entryAt(r.Bytes, 1, int((0x88000000>>21)&(mapEntries-1)))
	if ram&(entBuf|entCache) != entBuf|entCache {
		t.Fatalf("RAM-entry %#x mist bufferable/cacheable — een atomic faultt daar", ram)
	}
	mmio := entryAt(r.Bytes, 1, int((0x8A000000>>21)&(mapEntries-1)))
	if mmio&(entBuf|entCache) != 0 {
		t.Fatalf("MMIO-entry %#x is cacheable — daar wil je device-semantiek", mmio)
	}
}

// Geen enkele slot-PTE mag de G-vlag dragen. G belooft de hardware dat de
// mapping in élke adresruimte hetzelfde is, en elk slot mapt juist hetzelfde
// linkadres naar een ándere partitie — de spec noemt dat expliciet een
// softwarefout, en het silicium mag na een wissel de oude vertaling houden.
// GEMETEN 31-07: dat deed het ook (zie relocate.go). Deze test is de wacht
// ertegen, want de vlag ziet er onschuldig uit en "global" klinkt goedkoop.
func TestGeenGlobalOpSlotPTEs(t *testing.T) {
	m, err := Relocate(MapPlan{TableBase: 0x8FE00000, Windows: []MapWindow{
		{Link: 0x88000000, Phys: 0x8C000000, Size: 4 * BlockSize, R: true, W: true, X: true},
		{Link: 0x88800000, Phys: 0x8CC00000, Size: BlockSize, R: true, W: true, Device: true},
	}})
	if err != nil {
		t.Fatalf("Relocate: %v", err)
	}
	for off := 0; off+8 <= len(m.Bytes); off += 8 {
		e := binary.LittleEndian.Uint64(m.Bytes[off:])
		if e&entValid == 0 {
			continue
		}
		if e&entGlobalCheck != 0 {
			t.Fatalf("entry op +%#x heeft de G-vlag: %#x", off, e)
		}
	}
}

// De twee vlaggen die op géén enkele slot-PTE mogen staan, met opzet alléén hier
// gedefinieerd: in relocate.go bestaan ze niet, precies zodat niemand ze per
// ongeluk in een flagset kan meenemen. Deze tests zijn de wacht.
const (
	entUserCheck   = 1 << 4
	entGlobalCheck = 1 << 5
)
