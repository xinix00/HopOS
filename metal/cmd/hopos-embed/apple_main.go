//go:build apple

// apple_main — het acceptatiedraaiboek op de Mac mini M4 (Apple t8132, geboot
// via m1n1): de multikernel op het vierde silicium, en het eerste zonder PSCI
// en met een VHE-only EL2. De secties zelf zijn gedeeld (accept.go) en
// ongewijzigd — dát is het punt: als de kooi-naad houdt, hoort een nieuw board
// niets aan het draaiboek te veranderen. Wat hier wél nieuw bewezen wordt:
// CPUOn als spin-table-release (board/apple/hop), s2tramp/switch met de
// _EL12-encoderingen (cpu/el2/sysreg.h, -D VHE), en stage-2 met VM=1 op
// silicium waar dat tot vandaag alleen via VTCR-schrijfbaarheid was gemeten.
//
// Waarom deze main naast de agent (cmd/hopos): er is nog geen NIC op dit
// board, dus de agent kan geen app ophalen. Deze main draagt de app-image ín
// het binary (go:embed) en bewijst de kooi zonder één pakket over de lijn.
//
// Rapportage via de dockchannel (kis ch-0). Bouwen/laden: EMBED=1
// image/apple-m4.sh, dan image/apple/boot-cycle.sh metal/out/hopos-apple-embed.img.
package main

import (
	_ "embed"
	"fmt"
	"runtime"

	_ "github.com/xinix00/HopOS/metal/board/apple/hop" // registreert het board (init)
	"github.com/xinix00/HopOS/metal/cpu/memlimit"
)

// Dezelfde canonieke app als op QEMU, de Pi's en de Radxa (slot-1-IPA
// 0x50010000): één artifact draait op elk slot, want de stage-2 ís de relocatie.
//
//go:embed app.elf
var app []byte

func main() {
	memlimit.Arm() // geheugenplafond uit het RAM-raam — zie cpu/memlimit
	fmt.Println("")
	fmt.Println("HopOS (apple): bare-metal multikernel on the Mac mini M4 — no Linux, no macOS aboard")
	fmt.Printf("runtime %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	// EL2-invariant + firmware/DRAM/slots-rapport (gedeeld, accept.go).
	preamble("APPLE")

	// De vijf multikernel-acceptatiesecties, ongewijzigd gedeeld met de Pi's en
	// de Radxa. Core-naam: de E-cores dragen de slots 1..6 ("sawtooth").
	acceptance("APPLE", "M4", app)
}
