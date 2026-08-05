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
	ramStart, _ := runtime.MemRegion()
	l1 := (*[512]uint64)(unsafe.Pointer(uintptr(ramStart) + l1TableOffset))

	cur := l1[gb]
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
		l1[gb] = uint64(pa) | descTable
		return tbl, nil
	}
	return nil, fmt.Errorf("memattr: GB %d is niet gemapt (%#x)", gb, cur)
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

// flushTLB publiceert de tabelwijzigingen en gooit de oude vertalingen weg.
func flushTLB()
