//go:build tamago && arm64

package memattr

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

// De tabel-layout van tamago's identity-map (arm64/mmu.go): L1 op
// ramStart+0x4000, 512 entries van 1GB. Entry 0 wijst al naar een L2-tabel (die
// de nullpointer-pagina afvangt), de rest zijn 1GB-blokken. Wij lezen die
// layout, we bouwen hem niet — vandaar deze constante en niet een eigen map.
const (
	l1TableOffset = 0x4000
	block2M       = 2 << 20

	descBlock = 0x1
	descTable = 0x3
	// Uitvoer-adresveld van een block/page-descriptor (bits 47:12). Alles
	// daarbuiten is attribuut (laag: type/AttrIndx/AP/SH/AF, hoog: XN/PXN).
	addrMask = 0x0000_FFFF_FFFF_F000

	// MAIR-index 2 wordt de onze: 0x44 = Normal, Inner/Outer Non-cacheable.
	// tamago vult zelf alleen index 0 (Device-nGnRnE) en 1 (Normal WB), dus
	// index 2 is vrij en het aanpassen van MAIR raakt geen bestaande mapping.
	ncIndex = 2
	ncAttr  = 0x44

	// Het 2MB-blok zoals wij het willen: geldig, toegankelijk, inner-shareable,
	// EL1-RW, attribuut-index 2 (Normal-NC) en niet-uitvoerbaar. Een
	// framebuffer hoort nooit code te zijn.
	ncBlock = descBlock | 1<<10 | 0x3<<8 | 0x0<<6 | ncIndex<<2 | 0x3<<53
)

var (
	mu     sync.Mutex
	keep   []*[1024]uint64 // eigen L2-tabellen vasthouden: de GC mag ze nooit opruimen
	mairOK bool
)

// NormalNC zet [va, va+size) in de eigen stage-1-map op Normal-NC. Het venster
// wordt naar buiten afgerond op 2MB-blokgrens (de MMU kan niet fijner dan de
// bloklaag waarop we werken), dus roep dit alleen aan voor een venster waarvan
// de buurbytes óók gewoon DRAM zijn — een framebuffer dus, geen registerblok
// met MMIO ernaast.
//
// Idempotent en veilig bij herhaling: een tweede aanroep op hetzelfde venster
// schrijft dezelfde entries.
func NormalNC(va, size uintptr) error {
	if size == 0 {
		return nil
	}
	lo := va &^ (block2M - 1)
	hi := (va + size + block2M - 1) &^ (block2M - 1)
	if lo>>30 != (hi-1)>>30 {
		return fmt.Errorf("memattr: venster %#x..%#x kruist een GB-grens", va, va+size)
	}

	// AFRONDING MAG NIET BUITEN HET VENSTER VALLEN. De MMU kan hier niet
	// fijner dan 2MB, dus een venster dat niet op die grens ligt, zou de buren
	// meenemen — en die zijn niet altijd van ons.
	//
	// Dat is geen theorie gebleven. De framebuffer die iBoot achterlaat op de
	// Mac mini begint op 0x105e5304000: ruim een megabyte ná een 2MB-grens.
	// Hem hier doorlaten betekende ruim een megabyte FIRMWARE-geheugen van
	// Device naar Normal-NC zetten terwijl die firmware het gebruikte, en de
	// machine herstartte daarna om de paar minuten — zonder paniek, zonder
	// spoor (gemeten 30-08). Een attribuutwissel op geheugen van iemand anders
	// is geen optimalisatie maar een bug; liever een tragere buffer.
	if lo != va || hi != va+size {
		return fmt.Errorf("memattr: venster %#x+%#x ligt niet op 2MB-grenzen (%#x..%#x) — de buren zijn niet van ons",
			va, size, lo, hi)
	}

	mu.Lock()
	defer mu.Unlock()

	// Eerst MAIR, dán de entries: een entry die naar index 2 wijst terwijl die
	// index nog 0x00 (Device) is, zou het venster stil device laten.
	if !mairOK {
		m := readMAIR()
		if (m>>(8*ncIndex))&0xff != ncAttr {
			writeMAIR(m&^(0xff<<(8*ncIndex)) | ncAttr<<(8*ncIndex))
		}
		mairOK = true
	}

	l2, err := l2ForGB(lo >> 30)
	if err != nil {
		return err
	}
	gbBase := (lo >> 30) << 30
	for a := lo; a < hi; a += block2M {
		l2[(a-gbBase)>>21] = uint64(a) | ncBlock
	}
	flushTLB()
	return nil
}

// l2ForGB geeft de L2-tabel die dit GB beschrijft. Wijst de L1-entry al naar een
// tabel (bij tamago zo voor GB0 — de nullpointer-val), dan gebruiken we die.
// Staat er een 1GB-blok, dan splitsen we hem: een verse L2 met 512 identieke
// 2MB-blokken die exact dezelfde attributen dragen als het blok dat we
// vervangen, zodat het splitsen zélf niets verandert.
func l2ForGB(gb uintptr) (*[512]uint64, error) {
	l1, err := l1For(gb)
	if err != nil {
		return nil, err
	}
	cur := l1[gb&0x1FF]
	switch cur & 0x3 {
	case descTable:
		return (*[512]uint64)(unsafe.Pointer(uintptr(cur & addrMask))), nil
	case descBlock:
		pa, tbl := newTable()
		attrs := cur &^ uint64(addrMask) // type + AttrIndx/AP/SH/AF + XN
		gbBase := uint64(gb) << 30
		for i := range tbl {
			tbl[i] = gbBase + uint64(i)*block2M | attrs
		}
		// De walker leest deze tabel cacheable en wij schreven hem cacheable
		// (Go-heap), dus een DSB volstaat — dat doet flushTLB.
		l1[gb&0x1FF] = uint64(pa) | descTable
		return tbl, nil
	}
	return nil, fmt.Errorf("memattr: GB %d is niet gemapt (%#x)", gb, cur)
}

// l1For geeft de L1-tabel die dit GB beschrijft — en dat is niet altijd DE
// L1-tabel.
//
// tamago's vlakke map heeft er precies één, op ramStart+0x4000, en die dekt
// 512GB: `l1[gb]` mag dan met gb tot 511. Op silicium met RAM boven die grens
// (Apple: DRAM op 1TiB) bouwt de fork een L0 erboven, en dan is gb tot 1023 of
// hoger. Hier stond geen bereikcontrole, en dat is niet theoretisch gebleven:
// een framebuffer op 0x105e5304000 geeft gb = 1047, las 535 entries voorbij de
// tabel en nam de node mee (gemeten 30-08 op de M4 — de crash zat in fb.Init,
// dus in de agent net zo goed als in de probe).
//
// De weg omhoog is de TTBR0-wortel zelf: staat T0SZ op 16, dan is dat een
// L0-tabel en wijst entry gb>>9 naar de L1 die we zoeken.
func l1For(gb uintptr) (*[512]uint64, error) {
	if t0sz := readTCR() & 0x3F; t0sz > 24 {
		// Vlakke 39-bit-map: één L1, en alles daarbuiten bestaat niet.
		if gb > 511 {
			return nil, fmt.Errorf("memattr: GB %d ligt buiten de vlakke 512GB-map", gb)
		}
		ramStart, _ := runtime.MemRegion()
		return (*[512]uint64)(unsafe.Pointer(uintptr(ramStart) + l1TableOffset)), nil
	}
	l0 := (*[512]uint64)(unsafe.Pointer(readTTBR0() & addrMask))
	e := l0[(gb>>9)&0x1FF]
	if e&0x3 != descTable {
		return nil, fmt.Errorf("memattr: L0-entry %d is geen tabel (%#x)", (gb>>9)&0x1FF, e)
	}
	return (*[512]uint64)(unsafe.Pointer(uintptr(e & addrMask))), nil
}

// newTable levert een 4KB-gealigneerde L2-tabel uit de Go-heap. Identity-map,
// dus het virtuele adres ís het fysieke dat in de L1-entry moet staan.
func newTable() (uintptr, *[512]uint64) {
	b := new([1024]uint64) // 8KB: genoeg om 4KB-alignment binnen te vinden
	keep = append(keep, b)
	p := (uintptr(unsafe.Pointer(b)) + 0xFFF) &^ 0xFFF
	return p, (*[512]uint64)(unsafe.Pointer(p))
}

func readMAIR() uint64
func writeMAIR(v uint64)

// readTCR/readTTBR0: de vorm van de eigen map. Met VHE (E2H=1) lezen deze op
// EL2 vanzelf de _EL2-registers — dezelfde map, andere naam.
func readTCR() uint64
func readTTBR0() uintptr

// flushTLB publiceert de tabelwijzigingen en gooit de oude vertalingen weg.
func flushTLB()
