//go:build rk3566

// rk3566_main — het fase-P1-acceptatiedraaiboek op de Radxa Zero 3E (Rockchip
// RK3566, 4× Cortex-A55): de multikernel op het derde silicium. De secties
// zelf zijn gedeeld (accept.go) en ongewijzigd overgenomen van de Pi's — dát is
// het punt: als de kooi-naad houdt, hoort een nieuw board niets aan het
// draaiboek te veranderen.
//
// Waarom deze main naast de agent (cmd/hopos): de agent heeft een netwerk nodig
// om een app te starten (de apploader haalt zijn image op). Dat netwerk is er
// sinds 06-08 (gigabit + DHCP-lease), dus dit is niet langer de énige route —
// maar wél de route die van niets afhangt. Deze main draagt de app-image ín het
// binary (go:embed) en bewijst de kooi — stage-2-isolatie, hard-kill, relocatie,
// SMP — zonder DHCP-server, zonder internet, zonder één pakket over de lijn.
// Precies wat je wil als je een kooi-regressie zoekt en niet eerst je netwerk
// wil verdenken.
//
// Rapportage via de debug-UART (40-pins header, 1500000 8N1).
// Bouwen/flashen: EMBED=1 image/radxa-zero3.sh.
package main

import (
	_ "embed"
	"fmt"
	"runtime"

	_ "github.com/xinix00/HopOS/metal/v2/board/rk3566/hop" // registreert het board (init)
	"github.com/xinix00/HopOS/metal/v2/cpu/memlimit"
)

// Dezelfde canonieke app als op QEMU en de Pi's (slot-1-IPA 0x50010000): één
// artifact draait op elk slot, want de stage-2 ís de relocatie. Alleen met
// rk3566-runtime-hooks gebouwd.
//
//go:embed app.elf
var app []byte

func main() {
	memlimit.Arm() // geheugenplafond uit het RAM-raam — zie cpu/memlimit
	fmt.Println("")
	fmt.Println("HopOS (rk3566): bare-metal multikernel op de Radxa Zero 3E — geen Linux aan boord")
	fmt.Printf("runtime %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	// EL2-invariant + PSCI/DRAM/slots-rapport (gedeeld, accept.go).
	preamble("RK3566")

	// De vijf multikernel-acceptatiesecties, ongewijzigd gedeeld met de Pi's.
	acceptance("RK3566", "A55", app)
}
