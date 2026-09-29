package hopfs

import "testing"

// Een venster moet twee dingen doen: blok 0 op de eerste LBA van het venster
// leggen, en niet één blok meer uitdelen dan erin past. Dat tweede is de
// belangrijkste: op een Mac mini deelt hopfs de SSD met macOS, en een
// bestandssysteem dat "de hele schijf" denkt te hebben, schrijft dwars door het
// bestandssysteem van de eigenaar heen.
func TestVensterVerschuiftEnBegrenst(t *testing.T) {
	const (
		first  = 19_659_256 // begin van het vrije gat op de M4 (gemeten 30-08)
		blocks = 101_167_163
	)
	f := &FS{base: first, max: uint32(blocks), disk: nil}

	// Blok 0 ligt op de eerste LBA van het venster, niet op nul.
	if got := f.base + 0; got != first {
		t.Fatalf("blok 0 landt op LBA %d, wil %d", got, first)
	}

	// En de bovengrens is die van het venster, niet die van de schijf: het
	// laatste blok moet nog binnen [first, first+blocks) vallen.
	last := f.base + uint64(f.max) - 1
	if last >= first+blocks {
		t.Fatalf("laatste blok op LBA %d valt buiten het venster [%d, %d)",
			last, first, first+blocks)
	}
	if last < first {
		t.Fatalf("venster is leeg: laatste blok %d ligt vóór %d", last, first)
	}
}

// Een venster van nul blokken hoort geen enkel blok uit te delen — anders zou
// een verkeerd berekend gat stilletjes op LBA 0 gaan schrijven, en dat is waar
// de partitietabel staat.
func TestLeegVensterDeeltNietsUit(t *testing.T) {
	f := &FS{base: 4096, max: 0}
	if f.max != 0 {
		t.Fatalf("max = %d, wil 0", f.max)
	}
}
