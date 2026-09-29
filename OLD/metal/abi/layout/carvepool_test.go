package layout

import "testing"

// CarvePool bepaalt welk DRAM een board aan app-partities uitdeelt. Dat is
// bit-arithmetiek over gemeten banken, dus het hoort op de host bewezen te
// worden — op een bordje kost één ronde een kaartwissel, en de fout is er stil:
// een pool die één regio te ruim is geeft een app-partitie bovenop iets van de
// firmware, en dat merk je pas als de node zijn eigen naam kwijt is.
//
// Deze tests bestaan omdat precies dat mis dreigde te gaan op de Radxa Zero 3E
// (review 05-08): het gat voor de U-Boot-DTB stond op een VASTE bovengrens
// (32MB onder 2GB) terwijl de DTB gemeten op ~0x7ce9d000 lag — 17MB eronder, dus
// gewoon ín de pool. En bootparam.go leest die DTB live bij élke config-vraag.

// overlaps zegt of een regio-lijst een adresbereik raakt.
func overlaps(regs []Region, base, size uint64) bool {
	end := base + size
	for _, r := range regs {
		if base < r.Base+r.Size && r.Base < end {
			return true
		}
	}
	return false
}

func TestCarvePoolRespecteertEenNietUitgelijndGat(t *testing.T) {
	// Eén bank van 2GB minus de eerste 2MB — de gemeten /memory van de Radxa.
	banks := []Region{{Base: 0x200000, Size: 0x80000000 - 0x200000}}

	// Het gat: de gemeten DTB. Niet 2MB-uitgelijnd en klein — precies de vorm
	// waar een naar-buiten-afrondende implementatie op stukloopt.
	const dtb, dtbSize = 0x7CE9D000, 0x10000

	pool := CarvePool(banks, []Region{
		{Base: 0, Size: 0x07000000}, // alles onder poolBase
		{Base: dtb, Size: dtbSize},
	}, 2<<20)

	if len(pool) == 0 {
		t.Fatal("pool is leeg — CarvePool sneed de hele bank weg")
	}
	if overlaps(pool, dtb, dtbSize) {
		t.Errorf("pool raakt de DTB op %#x..%#x: %#v", dtb, dtb+dtbSize, pool)
	}
	// En het gat mag de pool niet in twee halve pools veranderen die samen
	// minder zijn dan de bank: er hoort een stuk vóór en een stuk ná te staan.
	var total uint64
	for _, r := range pool {
		total += r.Size
		if r.Base&(2<<20-1) != 0 || r.Size&(2<<20-1) != 0 {
			t.Errorf("regio %#x+%#x is niet 2MB-uitgelijnd", r.Base, r.Size)
		}
	}
	if total == 0 || total > 0x80000000-0x07000000 {
		t.Errorf("pool-totaal %#x is groter dan de bank boven poolBase", total)
	}
}

func TestCarvePoolHoudtDeStukkenBuitenElkGat(t *testing.T) {
	// Twee banken, drie gaten waarvan één over een bankgrens heen loopt.
	banks := []Region{
		{Base: 0x00000000, Size: 0x40000000},
		{Base: 0x40000000, Size: 0x40000000},
	}
	holes := []Region{
		{Base: 0x00000000, Size: 0x08000000}, // onderkant
		{Base: 0x3FF00000, Size: 0x00200000}, // over de bankgrens
		{Base: 0x7A123456, Size: 0x00001000}, // rommelig uitgelijnd, bovenin
	}
	pool := CarvePool(banks, holes, 2<<20)
	for _, h := range holes {
		if overlaps(pool, h.Base, h.Size) {
			t.Errorf("pool raakt gat %#x+%#x: %#v", h.Base, h.Size, pool)
		}
	}
}

func TestCarvePoolDroptStukkenOnderDeMinimummaat(t *testing.T) {
	// Een bank met een gat dat er twee snippers van 1MB overlaat: die zijn te
	// klein voor een partitie en mogen niet als pool gemeld worden — een
	// slot-manager die er een partitie in probeert te leggen faalt anders pas bij
	// de eerste jobstart.
	banks := []Region{{Base: 0x10000000, Size: 0x00400000}}
	holes := []Region{{Base: 0x10100000, Size: 0x00200000}}
	if pool := CarvePool(banks, holes, 2<<20); len(pool) != 0 {
		t.Errorf("verwachtte een lege pool, kreeg %#v", pool)
	}
}
