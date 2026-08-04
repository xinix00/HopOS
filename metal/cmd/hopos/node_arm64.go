//go:build arm64

package main

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/board"
)

// De ARM64-helft van wat de agent-main over zijn eigen ijzer moet weten. Drie
// dingen zijn hier arch-eigen: het privilege-niveau waarin we móeten booten,
// hoe de firmware zich meldt, en hoe HOP zijn éigen extra cores opbrengt. De
// riscv64-helft staat in node_riscv64.go; alles daartussen is gedeeld.

// bootRefusal meldt waarom deze node niet mag starten, of ok=false als er niets
// aan de hand is. Op ARM: HopOS eist een EL2-boot — de stage-2-kooi is een
// invariant, geen optie. Dit moet vóór de eerste PSCI-call (SMC) gebeuren.
func bootRefusal() (string, bool) {
	if el := board.Current().BootEL(); el < 2 {
		return fmt.Sprintf("booted at EL%d: HopOS requires EL2 (QEMU: virtualization=on)", el), true
	}
	return "", false
}

// firmwareLine is de console-regel die vertelt wát ons booten heeft en waar we
// staan.
func firmwareLine() string {
	major, minor := board.Current().PSCIVersion()
	return fmt.Sprintf("psci: v%d.%d (boot EL%d, SMC conduit)", major, minor, board.Current().BootEL())
}

// nodeDispatch is hoe HOP een éigen extra core opbrengt: direct via PSCI (hij
// ís HOP), naar de gedeelde EL2-trampoline die smp.ConfigureNode meegeeft.
func nodeDispatch(core int, entry, ctx uint64) {
	board.Current().CPUOn(uint64(core), entry, ctx)
}

// nodeCoreState is de console-weergave van een opgekomen node-core.
func nodeCoreState(core int) string {
	return fmt.Sprintf("PSCI-state=%d", board.Current().AffinityInfo(uint64(core)))
}
