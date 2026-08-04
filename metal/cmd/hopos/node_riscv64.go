//go:build riscv64

package main

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/board"
)

// De RISC-V64-helft van wat de agent-main over zijn eigen ijzer moet weten.
// Zie node_arm64.go voor het contract; hier de RISC-V-vorm.

// bootRefusal: HopOS eist M-mode. Alleen daar kun je PMP programmeren en harts
// resetten — en zonder dat is er geen kooi, dus geen HopOS. Zelfde weigering
// als BootEL<2 op ARM.
func bootRefusal() (string, bool) {
	if m := board.Current().BootMode(); m != 3 {
		return fmt.Sprintf("booted in mode %d: HopOS requires M-mode (3) for the PMP cage", m), true
	}
	return "", false
}

// firmwareLine: er is geen PSCI. Wij ZIJN de laag die op ARM de firmware is —
// ons image vervangt OpenSBI in het MONITOR-slot, dus we melden wat we van het
// board weten in plaats van wat een firmware ons vertelt.
func firmwareLine() string {
	return fmt.Sprintf("boot: M-mode monitor (no SBI), app harts %v", board.Current().AppHarts())
}

// nodeDispatch: niets te dispatchen. HOP heeft op dit board één hart (de C906);
// de C906L is het app-slot, niet een node-core. smp.ConfigureNode is hier een
// no-op, dus dit wordt nooit aangeroepen — maar het contract vullen we netjes.
func nodeDispatch(core int, entry, ctx uint64) {}

// nodeCoreState: het reset-blok is de enige bron van waarheid over een hart.
func nodeCoreState(core int) string {
	return fmt.Sprintf("hart-state=%s", board.Current().HartState(core))
}
