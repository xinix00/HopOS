//go:build rpi4

// pi4_main — de fase-P1-acceptatie op de Raspberry Pi 4: dezelfde multikernel
// als op de Pi 5, op de oudere Cortex-A72. De code ís gedeeld (metal/cpu/el2,
// stage2, slots, smp) en het draaiboek óók (raspi_main.go: acceptance —
// secties 1-5); dit bestand draagt alleen de rpi4-banner en de app-blob.
// Enige board-verschillen: de A72 nummert cores in aff0 (de Pi 5-A76 in aff1)
// en de stock-firmware levert géén PSCI, dus TF-A bl31.bin is als armstub
// verplicht (image/rpi4-hopos.sh, config.txt armstub=bl31.bin).
//
// Rapportage via de PL011 op GPIO14/15. Bouwen/flashen: image/rpi4-hopos.sh.
package main

import (
	_ "embed"
	"fmt"
	"runtime"

	_ "hop-os/metal/board/rpi4/hop" // registreert het board (init) + basis-hooks
)

// Zelfde canonieke app als op QEMU/Pi 5 (slot-1-IPA), met rpi4-runtime-hooks
// gebouwd (-tags rpi4). Eén artifact draait op elk slot — de stage-2 is de
// relocatie.
//
//go:embed app4.elf
var app []byte

func main() {
	fmt.Println("")
	fmt.Println("HopOS (rpi4): bare-metal multikernel op de Pi 4 — geen Linux aan boord")
	fmt.Printf("runtime %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	// EL2-invariant + PSCI/DRAM/slots-rapport (gedeeld, raspi_main.go).
	preamble("PI4")

	// ── 1-5: het gedeelde acceptatiedraaiboek (raspi_main.go). ──
	acceptance("PI4", "A72", app)

	fmt.Println("HOPOS_PI4_MULTIKERNEL_OK — fase P1: de multikernel draait op de Pi 4")

	select {}
}
