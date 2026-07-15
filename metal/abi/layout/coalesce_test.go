package layout

import "testing"

// Het Altra-scenario (14-07): aaneengesloten RAM dat de firmware als
// duizenden losse descriptors administreert. Zonder Coalesce is dat één
// grote schijn-fragmentatie; mét moet er weer één grote regio uitkomen.
func TestCoalesceGestreepteMap(t *testing.T) {
	var regs []Region
	// 1000 aangrenzende snippers van 1MB (Conventional/BSData om en om in
	// het echt — hier allemaal al door het usableRAM-filter).
	base := uint64(0x80000000)
	for i := uint64(0); i < 1000; i++ {
		regs = append(regs, Region{Base: base + i*(1 << 20), Size: 1 << 20})
	}
	out := Coalesce(regs)
	if len(out) != 1 {
		t.Fatalf("1000 aangrenzende snippers → %d regio's, verwacht 1", len(out))
	}
	if out[0].Base != base || out[0].Size != 1000<<20 {
		t.Fatalf("merge = %#x+%#x, verwacht %#x+%#x", out[0].Base, out[0].Size, base, uint64(1000<<20))
	}
}

func TestCoalesceGatenBlijvenGaten(t *testing.T) {
	out := Coalesce([]Region{
		{Base: 0x100000, Size: 0x100000}, // [1MB, 2MB)
		{Base: 0x300000, Size: 0x100000}, // [3MB, 4MB) — gat op [2MB, 3MB)
	})
	if len(out) != 2 {
		t.Fatalf("gat weggemerged: %d regio's, verwacht 2", len(out))
	}
}

func TestCoalesceOverlapEnVolgorde(t *testing.T) {
	// Ongesorteerd + overlappend: [4,6) ∪ [5,8) ∪ [8,9) → [4,9).
	out := Coalesce([]Region{
		{Base: 8 << 20, Size: 1 << 20},
		{Base: 4 << 20, Size: 2 << 20},
		{Base: 5 << 20, Size: 3 << 20},
	})
	if len(out) != 1 || out[0].Base != 4<<20 || out[0].Size != 5<<20 {
		t.Fatalf("overlap-merge fout: %+v", out)
	}
	// Deelverzameling (kleinere regio binnen een grotere) verdwijnt.
	out = Coalesce([]Region{
		{Base: 0x1000000, Size: 0x1000000},
		{Base: 0x1400000, Size: 0x100000},
	})
	if len(out) != 1 || out[0].Size != 0x1000000 {
		t.Fatalf("subset-merge fout: %+v", out)
	}
}
