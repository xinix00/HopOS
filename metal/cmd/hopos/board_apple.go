//go:build apple

// board_apple.go — de Apple-silicon-kant van de agent-main (Mac mini M4,
// t8132, geboot via m1n1): board-registratie, de RAM-declaratie van de HOP-kern
// en het PA-plan. Zelfde vorm als board_rk3566.go, ander silicium en een andere
// boot-route (geen U-Boot, geen DTB: het param-blok van de loader,
// board/apple/params.go).
//
// Bouwen: AGENT=1 image/apple-m4.sh (-tags "apple linkcpuinit highram",
// -asmflags -D VHE). Laden: image/apple/boot-cycle.sh. Zie
// docs/archief/apple-m4.md.
package main

import (
	"fmt"
	_ "unsafe"

	"github.com/xinix00/HopOS/metal/board/apple"
	_ "github.com/xinix00/HopOS/metal/board/apple/hop" // registreert het board (init)
)

// De RAM-declaratie van de HOP-kern: 256MB vanaf apple.RamBase (1TiB + 4GB).
// Ruim — HOP zit op ~20MB — maar dit board heeft 24GB en de TLS-apps en
// per-dial buffers van eerder (memory: OOM op een HOP van 64MB) verdienen lucht.
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = apple.RamBase

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = apple.HopRAMSize

func init() {
	// Eerst het DRAM bereikbaar maken: op dit silicium faultt een 1GB-blok met
	// een adres boven 2^40 (gemeten 28-08), dus élke GB buiten het HOP-venster
	// — de pool, m1n1's spin-table, de NIC-DMA — moet via 2MB-blokken. Vóór
	// het plan, want kern/slots raakt de pool meteen bij zijn init.
	if p, ok := apple.Params(); ok {
		n := apple.MapDRAM(p.DRAMBase, p.DRAMSize)
		fmt.Printf("mmu: %d GB of DRAM remapped to 2MB blocks (Apple: no 1GB blocks above 2^40)\n", n)
	} else {
		fmt.Printf("WARNING HOPOS_NO_PARAMS: no loader param block at %#x — DRAM outside the HOP window stays unreachable\n", apple.ParamBase)
	}

	// Het PA-plan moet vóór alles staan (slots/stage2 lezen het bij hun eerste
	// gebruik).
	apple.SetupPlan()

	// De platform-config (hopos.node/cluster/apikey/...) komt als tekst van de
	// loader (CFG=pad image/apple/load-probe.py → apple.CfgBase): dezelfde
	// `key=waarde`-regels als hopos.cfg op de Pi's, gelezen door fw/bootcfg.
	// Geen bootargs, geen initrd op dit board.
	bootParamAll = apple.BootParamAll

	// Node-identiteit-terugval: het serial dat de loader uit de ADT meegaf
	// (hopos.serial). Twee nodes op één LAN mogen nooit allebei "hopos-1" heten.
	nodeSerial = func() string {
		if s := apple.BootParam("hopos.serial"); len(s) >= 8 {
			return "hopos-" + s[len(s)-8:]
		}
		return ""
	}

	// Geen node-watchdog nog: m1n1 zet de Apple-WDT (0x3882b0000) uit en de
	// reset-scope is niet gemeten. Beleid in watchdog.go; nodeWDT blijft nil.
}
