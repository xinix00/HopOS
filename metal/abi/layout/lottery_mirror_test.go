package layout

// De loterij-spiegels: hop/cpuinit_riscv64.s kan geen Go-consts lezen en draagt
// dus letterlijke waarden. Dit klinkt beide kanten aan elkaar vast — tekst op
// tekst, want het licheerv-pakket zelf bouwt alleen onder tamago.

import (
	"os"
	"strings"
	"testing"
)

func TestLotterijSpiegels(t *testing.T) {
	asm, err := os.ReadFile("../../board/licheerv/hop/cpuinit_riscv64.s")
	if err != nil {
		t.Fatal(err)
	}
	goot, err := os.ReadFile("../../board/licheerv/lottery.go")
	if err != nil {
		t.Fatal(err)
	}
	a, g := string(asm), string(goot)
	for _, w := range []struct{ naam, inAsm, inGo string }{
		{"voortgang", "64(X10)", "LotteryProgress uintptr = 64"},
		{"adoptie-PC", "72(X10)", "LotteryAdoptPC  uintptr = 72"},
		{"levensteken", "88(X10)", "LotteryHopAlive uintptr = 88"},
		{"adoptie-arg", "96(X10)", "LotteryParkArg uintptr = 96"},
		{"scratch", "SCRATCH   0x8FE00000", "0x8FE00000"},
	} {
		if !strings.Contains(a, w.inAsm) {
			t.Errorf("spiegel %s: %q niet in hop/cpuinit_riscv64.s", w.naam, w.inAsm)
		}
		if w.naam != "scratch" && !strings.Contains(g, w.inGo) {
			t.Errorf("spiegel %s: %q niet in lottery.go", w.naam, w.inGo)
		}
	}
	if !strings.Contains(g, "= layout.HopCore") {
		t.Error("HopHart hoort layout.HopCore te lezen — de ene knop")
	}
}
