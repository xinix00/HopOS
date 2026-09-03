package slots

import (
	"testing"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
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
	bufferArena = region{}
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

// Een MISLUKTE allocatie mag de reservering van dat slot niet opruimen: doet hij
// dat wel, dan ligt het geheugen van een draaiende bewoner vrij in de pool en
// geeft de volgende plaatsing het aan iemand anders. Stille corruptie, en op het
// pad dat een onplaatsbare job elke vijf seconden raakt (Derek, 19-08).
func TestPartAllocFailKeepsTheReservation(t *testing.T) {
	poolReset(t, []layout.Region{{Base: 0x80000000, Size: 64 << 20}})

	base, size, err := partAlloc(1, 40<<20)
	if err != nil {
		t.Fatalf("eerste allocatie: %v", err)
	}
	// Slot 1 draait nu in [base, base+size). Een re-place die NIET past mag daar
	// niets aan veranderen.
	if _, _, err := partAlloc(1, 200<<20); err == nil {
		t.Fatal("een partitie van 200MB paste in een pool van 64MB")
	}
	gotBase, gotSize, ok := partitionOf(1)
	if !ok || gotBase != base || gotSize != size {
		t.Fatalf("na de misser: partitionOf(1) = %#x/%d (ok=%v), wil %#x/%d",
			gotBase, gotSize, ok, base, size)
	}
	// En de pool mag die bytes niet aan een ander slot uitdelen.
	other, otherSize, err := partAlloc(2, 24<<20)
	if err != nil {
		t.Fatalf("tweede slot: %v", err)
	}
	if other < base+size && other+otherSize > base {
		t.Fatalf("slot 2 kreeg [%#x,%#x) en dat overlapt slot 1 [%#x,%#x)",
			other, other+otherSize, base, base+size)
	}
}

// Een re-place van hetzelfde slot moet zijn EIGEN regio wel kunnen hergebruiken:
// het terugdraaien mag dat niet blokkeren.
func TestPartAllocReplaceReusesItsOwnRegion(t *testing.T) {
	poolReset(t, []layout.Region{{Base: 0x80000000, Size: 32 << 20}})

	if _, _, err := partAlloc(1, 30<<20); err != nil {
		t.Fatalf("eerste allocatie: %v", err)
	}
	// Zonder hergebruik van zijn eigen 30MB is er nergens 30MB vrij.
	if _, _, err := partAlloc(1, 30<<20); err != nil {
		t.Fatalf("re-place van hetzelfde slot: %v", err)
	}
}

// PoolLargest is wat de toelating nodig heeft: het grootste gat, niet de som.
// Zonder dat verschil reserveert HOP voor een job die nergens past.
func TestPoolLargestIsTheHoleNotTheSum(t *testing.T) {
	poolReset(t, []layout.Region{
		{Base: 0x88000000, Size: 126 << 20},
		{Base: 0x80000000, Size: 64 << 20},
		{Base: 0x86000000, Size: 32 << 20},
	})
	if got := PoolLargest(); got != 126<<20 {
		t.Errorf("op een verse pool = %d MB, wil 126", got>>20)
	}
	// De stand van de LicheeRV die de flapper opleverde: bundel 126, stulp 36.
	if _, _, err := partAlloc(1, 126<<20); err != nil {
		t.Fatal(err)
	}
	if _, _, err := partAlloc(2, 36<<20); err != nil {
		t.Fatal(err)
	}
	// De som van de pool die deze test opzette; PoolBytes() zelf leest
	// layout.Pool() en dat vraagt een board-plan dat hier niet bestaat.
	const pool = (126 + 64 + 32) << 20
	free := uint64(pool) - (126 << 20) - (36 << 20)
	largest := PoolLargest()
	if free != 60<<20 {
		t.Fatalf("vrij volgens de som = %d MB, wil 60", free>>20)
	}
	if largest != 32<<20 {
		t.Fatalf("grootste gat = %d MB, wil 32", largest>>20)
	}
	// Dít is het verschil dat de node liet flapperen: de som zegt ja tegen 36MB,
	// het gat zegt nee — en de plaatsing volgt het gat.
	if _, _, err := partAlloc(3, 36<<20); err == nil {
		t.Error("36MB werd geplaatst terwijl het grootste gat 32MB is")
	}
}

// Dereks twee klachten, letterlijk (19-08): "ik kon geen 200 plaatsen omdat ik
// alleen 128 pool had, en geen 128 meer plaatsen omdat de 128 was opgegeten door
// een 32." Beide zijn gevolgen van een pool in stukken, en beide verdwijnen als
// het DRAM van dit board één regio is (HopBase naar de onderkant, 19-08).
//
// De test staat er in de OUDE en de NIEUWE vorm naast elkaar, want dat is het
// hele punt: dezelfde allocator, ander plan.
func TestLicheeRVOneRegionPlacesWhatThreeCouldNot(t *testing.T) {
	oud := []layout.Region{
		{Base: 0x88000000, Size: 126 << 20}, // pool A, boven SlotBase
		{Base: 0x80000000, Size: 64 << 20},  // pool B, onder HOP
		{Base: 0x86000000, Size: 32 << 20},  // pool C, de hersnit-winst
	}
	nieuw := []layout.Region{
		{Base: 0x82400000, Size: 218 << 20}, // één regio boven HOP
	}

	t.Run("200MB op een lege node", func(t *testing.T) {
		poolReset(t, oud)
		if _, _, err := partAlloc(1, 200<<20); err == nil {
			t.Error("oude vorm plaatste 200MB — dat kon juist niet")
		}
		poolReset(t, nieuw)
		if _, _, err := partAlloc(1, 200<<20); err != nil {
			t.Errorf("nieuwe vorm: 200MB past niet: %v", err)
		}
	})

	t.Run("vrij maar niet plaatsbaar", func(t *testing.T) {
		// Dit is de klacht precies: drie apps die elk in een ANDERE regio
		// landen laten overal een restje achter. De som zegt dan 100MB vrij en
		// er past geen 96 — terwijl één regio met dezelfde apps 96MB aaneen
		// overhoudt en hem gewoon plaatst.
		poolReset(t, oud)
		for i, mb := range []uint64{60, 30, 32} { // → B, C, en dan A
			if _, _, err := partAlloc(i+1, mb<<20); err != nil {
				t.Fatalf("oude vorm: %dMB paste niet: %v", mb, err)
			}
		}
		vrij := uint64(222-60-30-32) << 20
		if got := PoolLargest(); got >= vrij {
			t.Fatalf("oude vorm: grootste gat %d MB is niet kleiner dan de %d MB vrij — dan bewijst deze test niets",
				got>>20, vrij>>20)
		}
		if _, _, err := partAlloc(4, 96<<20); err == nil {
			t.Error("oude vorm plaatste 96MB — verwacht: nee, want het ligt in stukken")
		}

		poolReset(t, nieuw)
		for i, mb := range []uint64{60, 30, 32} {
			if _, _, err := partAlloc(i+1, mb<<20); err != nil {
				t.Fatalf("nieuwe vorm: %dMB paste niet: %v", mb, err)
			}
		}
		if _, _, err := partAlloc(4, 96<<20); err != nil {
			t.Errorf("nieuwe vorm: 96MB paste niet terwijl er 96 vrij was: %v", err)
		}
	})

	t.Run("vrij IS plaatsbaar zolang er alleen bijkomt", func(t *testing.T) {
		// De eigenschap waar het om gaat, en de reden dat één regio de klacht
		// wegneemt: zolang er alleen apps bijkomen is het vrije deel één stuk,
		// dus is het grootste gat exact het vrije geheugen. "Er is X vrij" en
		// "X is plaatsbaar" zijn dan hetzelfde antwoord.
		poolReset(t, nieuw)
		gebruikt := uint64(0)
		for i, mb := range []uint64{32, 128, 20, 14} {
			if _, _, err := partAlloc(i+1, mb<<20); err != nil {
				t.Fatalf("%dMB paste niet: %v", mb, err)
			}
			gebruikt += mb << 20
			if got, wil := PoolLargest(), (218<<20)-gebruikt; got != wil {
				t.Fatalf("na %d apps: grootste gat %d MB, vrij %d MB — die horen gelijk te zijn",
					i+1, got>>20, wil>>20)
			}
		}
	})

	t.Run("een stop maakt ook in één regio een gat", func(t *testing.T) {
		// Eerlijk houden wat dit NIET oplost: geeft een app in het midden zijn
		// partitie terug, dan valt het vrije deel alsnog in twee stukken. Op
		// ARM kan de stage-2 die stukken aan elkaar plakken (scatter), op de
		// C906 niet — daar is het PMP-budget van acht entries de grens.
		poolReset(t, nieuw)
		for i, mb := range []uint64{60, 60, 60} {
			if _, _, err := partAlloc(i+1, mb<<20); err != nil {
				t.Fatal(err)
			}
		}
		partRelease(2) // de middelste
		vrij := uint64(218-120) << 20
		if got := PoolLargest(); got >= vrij {
			t.Errorf("grootste gat %d MB, vrij %d MB — na een stop in het midden hoort dat te verschillen",
				got>>20, vrij>>20)
		}
	})
}

func TestBufferGeometryHoudtPayloadInEenGezamenlijkePool(t *testing.T) {
	old := layout.MaxSlots
	defer layout.SetMaxSlots(old)

	layout.SetMaxSlots(128)
	reserved, metadata, payload, err := networkGeometry(50 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if reserved != 50<<20 || metadata != 17<<20 || payload != 33<<20 {
		t.Fatalf("128 slots: reserved=%d MiB metadata=%d MiB payload=%d MiB",
			reserved>>20, metadata>>20, payload>>20)
	}
	if _, _, _, err := networkGeometry(16 << 20); err == nil {
		t.Fatal("16 MiB geaccepteerd; descriptor-metadata alleen vraagt al 17 MiB")
	}

	layout.SetMaxSlots(16)
	reserved, metadata, payload, err = networkGeometry(4 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if reserved != 4<<20 || metadata != 2176<<10 || payload != 1920<<10 {
		t.Fatalf("16 slots compact: reserved=%d KiB metadata=%d KiB payload=%d KiB",
			reserved>>10, metadata>>10, payload>>10)
	}

	reserved, metadata, payload, err = networkGeometry(50 << 20)
	if err != nil {
		t.Fatal(err)
	}
	if reserved != 50<<20 || payload != reserved-metadata {
		t.Fatalf("16 slots: reserved=%d metadata=%d payload=%d", reserved, metadata, payload)
	}
}

func TestSlotBuffersZijnCompactEnGescheiden(t *testing.T) {
	old := layout.MaxSlots
	layout.SetMaxSlots(128)
	defer layout.SetMaxSlots(old)
	poolReset(t, []layout.Region{{Base: 0x80000000, Size: 128 << 20}})
	if err := ConfigureNetworkBuffer(50 << 20); err != nil {
		t.Fatal(err)
	}
	c1, tx1, rx1, err := slotBuffers(1)
	if err != nil {
		t.Fatal(err)
	}
	c2, tx2, rx2, err := slotBuffers(2)
	if err != nil {
		t.Fatal(err)
	}
	if tx1-c1 != uintptr(layout.SlotControlStride) || rx1-tx1 != 4<<10 {
		t.Fatalf("slot 1: ctrl=%#x tx=%#x rx=%#x", c1, tx1, rx1)
	}
	if c2-rx1 != 4<<10 || tx2-c2 != uintptr(layout.SlotControlStride) || rx2-tx2 != 4<<10 {
		t.Fatalf("slot 2 sluit niet compact aan: ctrl=%#x tx=%#x rx=%#x", c2, tx2, rx2)
	}
}
