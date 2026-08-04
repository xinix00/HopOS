package slots

import (
	"testing"

	"github.com/xinix00/HopOS/metal/abi/layout"
)

// Best-fit, en waarom het een test verdient: hoog-eerst koos niet tussen twee
// regio's die allebei passen, en dan snijdt een kleine job zijn partitie uit de
// énige regio die nog een grote kan dragen. Dit is de LicheeRV-pool exact zoals
// hij op het bordje ligt — 127MB naast 64MB — met de maten waarmee het 31-07
// misging: een 64MB-partitie hoort in de 64MB-regio te landen, zodat de 124MB
// erna nog past.
func TestPartAllocBestFitHoudtGroteRegioHeel(t *testing.T) {
	poolReset(t, []layout.Region{
		{Base: 0x88000000, Size: 0x7F00000}, // 127MB (pool A)
		{Base: 0x80000000, Size: 0x4000000}, // 64MB  (pool B)
	})

	small, _, err := partAlloc(1, 64<<20)
	if err != nil {
		t.Fatalf("64MB: %v", err)
	}
	if small < 0x80000000 || small >= 0x84000000 {
		t.Errorf("64MB landde op %#x — hoort in de 64MB-regio (0x80000000), niet in de 127MB", small)
	}
	big, _, err := partAlloc(2, 124<<20)
	if err != nil {
		t.Fatalf("124MB ná de 64MB: %v — precies de fout van 31-07 (regio gefragmenteerd)", err)
	}
	if big < 0x88000000 || big+124<<20 > 0x8FF00000 {
		t.Errorf("124MB landde op %#x, buiten de 127MB-regio", big)
	}
}

// En de omgekeerde orde moet óók werken: de grote eerst, de kleine erna.
func TestPartAllocGroteEerstDanKleine(t *testing.T) {
	poolReset(t, []layout.Region{
		{Base: 0x88000000, Size: 0x7F00000},
		{Base: 0x80000000, Size: 0x4000000},
	})
	if _, _, err := partAlloc(1, 124<<20); err != nil {
		t.Fatalf("124MB: %v", err)
	}
	if _, _, err := partAlloc(2, 64<<20); err != nil {
		t.Fatalf("64MB ná de 124MB: %v", err)
	}
}

// poolReset zet de allocator op een verse pool. Rechtstreeks op de interne staat,
// want poolInit leest het board-plan en dat is hier niet wat we willen toetsen.
func poolReset(t *testing.T, regs []layout.Region) {
	t.Helper()
	partMu.Lock()
	defer partMu.Unlock()
	partOnce.Do(func() {})
	partFree = nil
	for _, r := range regs {
		partFree = append(partFree, region{r.Base, r.Size})
	}
	partOf = make([]region, layout.SlotCap+1)
}

// De maat die partAlloc teruggeeft ÍS de partitie, ook als de aanvraag geen
// veelvoud van de korrel was. Dat de aanroeper zijn eigen getal hield was de bug
// van 31-07: de kooi kreeg dan een andere maat te zien dan de partitie had.
func TestPartAllocGeeftDeEchteMaat(t *testing.T) {
	poolReset(t, []layout.Region{{Base: 0x88000000, Size: 0x4000000}})
	base, grown, err := partAlloc(1, 23<<20) // geen veelvoud van 2MB
	if err != nil {
		t.Fatalf("23MB: %v", err)
	}
	if grown < 23<<20 || grown%part2M != 0 {
		t.Errorf("maat %#x is niet naar de korrel opgerond", grown)
	}
	b, sz, ok := partitionOf(1)
	if !ok || b != base || sz != grown {
		t.Errorf("partitionOf geeft %#x+%#x, partAlloc gaf %#x+%#x", b, sz, base, grown)
	}
}
