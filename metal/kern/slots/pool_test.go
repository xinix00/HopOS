package slots

import (
	"errors"
	"github.com/xinix00/HopOS/metal/v2/board"
	"testing"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
)

// setCores stelt de fysieke app-core-grens in en veegt de allocator schoon.
func setCores(t *testing.T, n int) {
	t.Helper()
	layout.SetAppCores(n)
	SetHopCores(1) // hopReserved=0: app-cores 1..n
	resetPools()
}

func TestPoolDedicatedEigenCore(t *testing.T) {
	setCores(t, 4)
	seen := map[int]bool{}
	for cage := 1; cage <= 4; cage++ {
		c, err := PlaceCage(cage, "", 1, 1, "")
		if err != nil {
			t.Fatalf("kooi %d: %v", cage, err)
		}
		if c < 1 || c > 4 || seen[c] {
			t.Fatalf("kooi %d kreeg core %d (dubbel/buiten bereik)", cage, c)
		}
		seen[c] = true
	}
	// Vijfde dedicated app past niet: 4 cores op. Géén stille terugval naar
	// delen — delen is een keuze die de aanroeper maakt met een sharegroup, want
	// het is een vertrouwensbeslissing (medebewoners zien elkaars timing).
	if _, err := PlaceCage(5, "", 1, 1, ""); err == nil {
		t.Fatal("5e dedicated kooi moet falen: delen vraagt een sharegroup")
	}
}

func TestPoolSharegroupBalanceert(t *testing.T) {
	setCores(t, 4)
	// Vier kooien (4..7, dus BOVEN de 4 cores — bewijst kooi≠core) in één
	// sharegroup met een pool van 2 hele cores → 2 cores, 2 kooien elk.
	got := map[int]int{}
	for cage := 4; cage <= 7; cage++ {
		c, err := PlaceCage(cage, "web", 2, 1, "")
		if err != nil {
			t.Fatalf("kooi %d: %v", cage, err)
		}
		got[c]++
	}
	if len(got) != 2 {
		t.Fatalf("pool=2 gebruikte %d cores, wil 2: %v", len(got), got)
	}
	for c, n := range got {
		if n != 2 {
			t.Fatalf("core %d draagt %d kooien, wil 2 (niet gebalanceerd): %v", c, n, got)
		}
	}
	// Een andere sharegroup pakt de resterende 2 cores.
	if _, err := PlaceCage(8, "db", 2, 1, ""); err != nil {
		t.Fatalf("tweede sharegroup: %v", err)
	}
	// En dan is het op: geen vrije core meer voor een derde pool.
	if _, err := PlaceCage(9, "cache", 1, 1, ""); err == nil {
		t.Fatal("derde pool moet falen (alle 4 cores vergeven)")
	}
}

func TestPoolReleaseGeeftPoolTerug(t *testing.T) {
	setCores(t, 4)
	for cage := 4; cage <= 7; cage++ {
		if _, err := PlaceCage(cage, "web", 2, 1, ""); err != nil {
			t.Fatalf("kooi %d: %v", cage, err)
		}
	}
	// Drie van de vier weg: pool houdt zijn 2 cores nog (nog 1 lid).
	ReleaseCage(4)
	ReleaseCage(5)
	ReleaseCage(6)
	if _, err := PlaceCage(10, "other", 3, 1, ""); err == nil {
		t.Fatal("pool 'web' zou zijn 2 cores nog moeten vasthouden (1 lid leeft)")
	}
	// Laatste lid weg: nu komen alle 2 pool-cores vrij → 3 zijn er vrij (2+1... nee: 4 totaal, web had 2, 2 vrij + 2 terug = 4).
	ReleaseCage(7)
	if _, err := PlaceCage(11, "other", 4, 1, ""); err != nil {
		t.Fatalf("na leegloop pool moeten alle 4 cores vrij zijn: %v", err)
	}
}

// Twee jobs in dezelfde sharegroup die het over de poolgrootte oneens zijn is een
// FOUT en geen detail: het werd stil "first wins", dus de tweede job kreeg minder
// hart dan zijn spec zei zonder dat iemand het merkte.
func TestPoolGrootteMismatchWordtGeweigerd(t *testing.T) {
	setCores(t, 6)
	if _, err := PlaceCage(4, "web", 2, 1, ""); err != nil {
		t.Fatalf("eerste kooi: %v", err)
	}
	if _, err := PlaceCage(5, "web", 4, 1, ""); err == nil {
		t.Fatal("een tweede spec met 4 cores in een pool van 2 moet falen, niet stil 2 krijgen")
	}
	// Dezelfde grootte blijft natuurlijk gewoon werken — en een default-loze
	// aanvraag (0 → 1) tegen een pool van 1 óók.
	if _, err := PlaceCage(6, "web", 2, 1, ""); err != nil {
		t.Fatalf("gelijke poolgrootte moet gewoon slagen: %v", err)
	}
	if _, err := PlaceCage(7, "solo", 0, 1, ""); err != nil {
		t.Fatalf("eerste kooi van 'solo': %v", err)
	}
	if _, err := PlaceCage(8, "solo", 1, 1, ""); err != nil {
		t.Fatalf("poolCores 0 en 1 zijn hetzelfde (0 wordt 1): %v", err)
	}
}

func TestPlaceCageIdempotent(t *testing.T) {
	setCores(t, 4)
	c1, err := PlaceCage(4, "web", 2, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := PlaceCage(4, "web", 2, 1, "") // fase 2 van dezelfde kooi
	if err != nil {
		t.Fatal(err)
	}
	if c1 != c2 {
		t.Fatalf("twee-fase-start kreeg verschillende cores: %d vs %d", c1, c2)
	}
}

func TestPoolRespecteertHopReserved(t *testing.T) {
	layout.SetAppCores(4)
	SetHopCores(2) // hopReserved=1: core 1 is HOP-runtime, app-cores 2..4
	resetPools()
	defer SetHopCores(1)
	for cage := 1; cage <= 3; cage++ {
		c, err := PlaceCage(cage, "", 1, 1, "")
		if err != nil {
			t.Fatalf("kooi %d: %v", cage, err)
		}
		if c < 2 {
			t.Fatalf("kooi %d kreeg core %d — core 1 is gereserveerd voor HOP", cage, c)
		}
	}
	if _, err := PlaceCage(4, "", 1, 1, ""); err == nil {
		t.Fatal("4e dedicated moet falen: maar 3 app-cores (2..4)")
	}
}

// Een SMP-kooi houdt zijn hele core-run bezet, niet alleen de primaire. Zonder
// dat landt de volgende app stil op de tweede core van de SMP-app: geen fout,
// alleen een buurman die 137x trager is (gemeten 05-09 op de M4).
func TestPoolSMPReserveertZijnHeleRun(t *testing.T) {
	setCores(t, 4)
	c, err := PlaceCage(2, "", 1, 2, "") // kooi 2 met twee cores → 2 en 3
	if err != nil {
		t.Fatalf("SMP-kooi 2: %v", err)
	}
	if c != 2 {
		t.Fatalf("SMP-kooi 2 kreeg core %d, wil zijn eigen core 2", c)
	}
	next, err := PlaceCage(4, "", 1, 1, "")
	if err != nil {
		t.Fatalf("kooi 4: %v", err)
	}
	if next == 3 {
		t.Fatal("kooi 4 landde op core 3 — de tweede core van de SMP-app")
	}
	if next != 4 {
		t.Fatalf("kooi 4 kreeg core %d, wil 4", next)
	}

	// En terug: de hele run komt vrij, niet alleen de primaire core.
	ReleaseCage(2)
	for _, core := range []int{2, 3} {
		if !coreFree(core) {
			t.Fatalf("core %d is na ReleaseCage nog bezet", core)
		}
	}
}

// De primaire core van een SMP-app MOET zijn eigen kooinummer zijn (smp.go
// dispatcht kooi+1..kooi+cores-1, validateSMPPlacement weigert de rest). Past
// die run niet, dan is dat een duidelijke fout en geen stille andere plaatsing.
func TestPoolSMPWeigertEenBezetteRun(t *testing.T) {
	setCores(t, 4)
	if _, err := PlaceCage(3, "", 1, 1, ""); err != nil { // core 3 bezet
		t.Fatalf("kooi 3: %v", err)
	}
	if _, err := PlaceCage(2, "", 1, 2, ""); err == nil {
		t.Fatal("SMP-kooi 2 moet falen: core 3 is bezet")
	}
	// Buiten het core-bereik telt net zo goed: kooi 4 + 1 = core 5 bestaat niet.
	if _, err := PlaceCage(4, "", 1, 2, ""); err == nil {
		t.Fatal("SMP-kooi 4 moet falen: core 5 bestaat niet op een 4-core node")
	}
}

// Een kooi in een sharegroup draait op één core — de pool ís het deelmechanisme.
// Twee cores vragen én delen is een tegenstrijdige spec, geen capaciteitsfout.
func TestPoolSharegroupMetSMPWordtGeweigerd(t *testing.T) {
	setCores(t, 4)
	err := func() error { _, err := PlaceCage(1, "web", 2, 2, ""); return err }()
	if err == nil {
		t.Fatal("sharegroup + 2 app-cores moet falen")
	}
	if !errors.Is(err, ErrPoolSize) {
		t.Fatalf("fout moet ErrPoolSize dragen (geen pending-capaciteit): %v", err)
	}
}

type classPoolBoard struct {
	board.Board
	classes map[int]string
}

func (b classPoolBoard) CoreClass(c int) string { return b.classes[c] }
func useClassPool(t *testing.T, n int, classes map[int]string) {
	t.Helper()
	setCores(t, n)
	old := board.Current()
	board.Use(classPoolBoard{classes: classes})
	t.Cleanup(func() { board.Use(old) })
}

func TestPoolClassSharingAndSMP(t *testing.T) {
	useClassPool(t, 9, map[int]string{7: "big", 8: "big", 9: "big"})
	for _, cage := range []int{10, 11} {
		if core, err := PlaceCage(cage, "trusted", 1, 1, "big"); err != nil || core != 7 {
			t.Fatalf("shared cage %d: core=%d err=%v", cage, core, err)
		}
	}
	if CanPlaceDedicated(7, 2) || !CanPlaceDedicated(8, 2) {
		t.Fatal("SMP free-run query ignores physical pool")
	}
	if core, err := PlaceCage(8, "", 1, 2, "big"); err != nil || core != 8 {
		t.Fatalf("SMP core=%d err=%v", core, err)
	}
	ReleaseCage(10)
	if CanPlaceDedicated(7, 1) {
		t.Fatal("live neighbor lost pool")
	}
	ReleaseCage(11)
	if !CanPlaceDedicated(7, 1) {
		t.Fatal("last neighbor did not release pool")
	}
}
func TestPoolOneCoreClassSharing(t *testing.T) {
	useClassPool(t, 1, map[int]string{1: "big"})
	for _, cage := range []int{2, 3} {
		if core, err := PlaceCage(cage, "trusted", 1, 1, "big"); err != nil || core != 1 {
			t.Fatalf("cage=%d core=%d err=%v", cage, core, err)
		}
	}
}
func TestPoolClassMismatchAndDedicatedFallback(t *testing.T) {
	useClassPool(t, 3, map[int]string{1: "small", 2: "big", 3: "big"})
	if _, err := PlaceCage(4, "mixed", 1, 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := PlaceCage(5, "mixed", 1, 1, "big"); !errors.Is(err, ErrPoolSize) {
		t.Fatalf("class mismatch accepted: %v", err)
	}
	ReleaseCage(4)
	if core, err := PlaceCage(4, "", 1, 1, "big"); err != nil || core != 2 {
		t.Fatalf("fallback violated class: core=%d err=%v", core, err)
	}
	if core, err := PlaceCage(5, "", 1, 1, "big"); err != nil || core != 3 {
		t.Fatalf("fallback violated class: core=%d err=%v", core, err)
	}
	if _, err := PlaceCage(6, "", 1, 1, "big"); err == nil {
		t.Fatal("fell back to small core")
	}
}
