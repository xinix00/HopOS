package slots

import (
	"testing"

	"hop-os/metal/abi/layout"
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
		c, err := PlaceCage(cage, "", 1)
		if err != nil {
			t.Fatalf("kooi %d: %v", cage, err)
		}
		if c < 1 || c > 4 || seen[c] {
			t.Fatalf("kooi %d kreeg core %d (dubbel/buiten bereik)", cage, c)
		}
		seen[c] = true
	}
	// Vijfde dedicated app past niet: 4 cores op.
	if _, err := PlaceCage(5, "", 1); err == nil {
		t.Fatal("5e dedicated kooi moet falen (geen vrije core)")
	}
}

func TestPoolSharegroupBalanceert(t *testing.T) {
	setCores(t, 4)
	// Vier kooien (4..7, dus BOVEN de 4 cores — bewijst kooi≠core) in één
	// sharegroup met een pool van 2 hele cores → 2 cores, 2 kooien elk.
	got := map[int]int{}
	for cage := 4; cage <= 7; cage++ {
		c, err := PlaceCage(cage, "web", 2)
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
	if _, err := PlaceCage(8, "db", 2); err != nil {
		t.Fatalf("tweede sharegroup: %v", err)
	}
	// En dan is het op: geen vrije core meer voor een derde pool.
	if _, err := PlaceCage(9, "cache", 1); err == nil {
		t.Fatal("derde pool moet falen (alle 4 cores vergeven)")
	}
}

func TestPoolReleaseGeeftPoolTerug(t *testing.T) {
	setCores(t, 4)
	for cage := 4; cage <= 7; cage++ {
		if _, err := PlaceCage(cage, "web", 2); err != nil {
			t.Fatalf("kooi %d: %v", cage, err)
		}
	}
	// Drie van de vier weg: pool houdt zijn 2 cores nog (nog 1 lid).
	ReleaseCage(4)
	ReleaseCage(5)
	ReleaseCage(6)
	if _, err := PlaceCage(10, "other", 3); err == nil {
		t.Fatal("pool 'web' zou zijn 2 cores nog moeten vasthouden (1 lid leeft)")
	}
	// Laatste lid weg: nu komen alle 2 pool-cores vrij → 3 zijn er vrij (2+1... nee: 4 totaal, web had 2, 2 vrij + 2 terug = 4).
	ReleaseCage(7)
	if _, err := PlaceCage(11, "other", 4); err != nil {
		t.Fatalf("na leegloop pool moeten alle 4 cores vrij zijn: %v", err)
	}
}

func TestPlaceCageIdempotent(t *testing.T) {
	setCores(t, 4)
	c1, err := PlaceCage(4, "web", 2)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := PlaceCage(4, "web", 2) // fase 2 van dezelfde kooi
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
		c, err := PlaceCage(cage, "", 1)
		if err != nil {
			t.Fatalf("kooi %d: %v", cage, err)
		}
		if c < 2 {
			t.Fatalf("kooi %d kreeg core %d — core 1 is gereserveerd voor HOP", cage, c)
		}
	}
	if _, err := PlaceCage(4, "", 1); err == nil {
		t.Fatal("4e dedicated moet falen: maar 3 app-cores (2..4)")
	}
}
