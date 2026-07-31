package conlog

import (
	"strings"
	"testing"
)

// reset maakt de ring leeg. Alleen voor de tests: op een node is deze staat
// zo lang als de boot.
func reset() {
	pos = 0
	buf = [Size]byte{}
}

func put(s string) {
	for i := range len(s) {
		Put(s[i])
	}
}

func TestSnapshotGeeftAllesTerugZolangHetPast(t *testing.T) {
	reset()
	put("slot 1: image placed in 57ms\n")
	if got, want := string(Snapshot()), "slot 1: image placed in 57ms\n"; got != want {
		t.Fatalf("Snapshot = %q, want %q", got, want)
	}
	if d := Dropped(); d != 0 {
		t.Fatalf("Dropped = %d, want 0 zolang de hele geschiedenis past", d)
	}
}

func TestRingHoudtDeLAATSTEBytes(t *testing.T) {
	reset()
	// Eerst de ring helemaal vol met iets herkenbaars, dan één regel erover.
	put(strings.Repeat("x", Size))
	put("de laatste regel\n")

	got := string(Snapshot())
	if len(got) != Size {
		t.Fatalf("snapshot is %d bytes, want %d (de ring hoort vol te blijven)", len(got), Size)
	}
	if !strings.HasSuffix(got, "de laatste regel\n") {
		t.Fatal("de nieuwste bytes horen achteraan te staan")
	}
	if strings.HasPrefix(got, "x") == false {
		t.Fatal("de oudste bewaarde bytes horen vooraan te staan")
	}
	// Precies zoveel gedropt als er te veel in ging.
	if d, want := Dropped(), uint64(len("de laatste regel\n")); d != want {
		t.Fatalf("Dropped = %d, want %d", d, want)
	}
}

// Een lezer mag de ring niet leegmaken: twee keer opvragen geeft twee keer
// hetzelfde. Anders zou de eerste die kijkt de reden voor de tweede weggooien.
func TestSnapshotIsHerhaalbaar(t *testing.T) {
	reset()
	put("HOPOS_AGENT_UP\n")
	if a, b := string(Snapshot()), string(Snapshot()); a != b {
		t.Fatalf("twee snapshots verschillen: %q vs %q", a, b)
	}
}

// Since is wat een meelezer op de console-poort gebruikt: hij houdt zijn eigen
// positie bij en krijgt precies wat er sinds toen bij kwam.
func TestSinceVolgtDeConsole(t *testing.T) {
	reset()
	put("eerste\n")
	d1, next := Since(0)
	if string(d1) != "eerste\n" {
		t.Fatalf("Since(0) = %q", d1)
	}
	// Niets nieuws: geen bytes, positie blijft staan.
	if d, n := Since(next); len(d) != 0 || n != next {
		t.Fatalf("Since op dezelfde positie moet leeg zijn, kreeg %q/%d", d, n)
	}
	put("tweede\n")
	d2, _ := Since(next)
	if string(d2) != "tweede\n" {
		t.Fatalf("Since(next) = %q, want alleen het nieuwe stuk", d2)
	}
}

// Een lezer die te lang niet keek terwijl de ring rondliep, begint bij het
// oudste dat er nog is — hij mist bytes, maar krijgt nooit onzin.
func TestSinceSlaatOverWatDeRingKwijtIs(t *testing.T) {
	reset()
	put(strings.Repeat("a", Size+100))
	d, next := Since(0)
	if uint64(len(d)) != uint64(Size) {
		t.Fatalf("Since(0) gaf %d bytes, want %d (de hele ring)", len(d), Size)
	}
	if next != pos {
		t.Fatalf("next = %d, want %d", next, pos)
	}
}
