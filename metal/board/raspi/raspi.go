// Package raspi is de gedeelde board-laag voor de Raspberry Pi-familie
// (BCM2711 = Pi 4, BCM2712 = Pi 5). Beide boards booten identiek: de
// firmware laadt een raw arm64-Image op 0x200000 (Pi 5-default; op de Pi 4
// zetten we kernel_address=0x200000 in config.txt zodat het geheugenplan
// één-op-één gelijk is) en levert ons op EL2 af met TF-A/armstub op EL3 →
// PSCI via SMC.
//
// Taakverdeling met de dunne board-pakketten (board/rpi4, board/rpi5):
//
//   - hier: PSCI/SMCCC (+ psci.s), MPIDR-read, generic timers + idle, de
//     runtime-hooks RamStackOffset/Hwinit1/Nanotime, het park/scratch-
//     geheugenplan en de park-code-generator;
//   - board: UART (printk + cpuinit.s — die moeten vóór init() al werken,
//     dus geen parametrisatie via variabelen), MPIDR-nummering (A72: aff0,
//     A76: aff1 → target/CoreID), GIC-basis, RNG en de board.Board-glue.
//
// Alleen voor GOOS=tamago GOARCH=arm64.
package raspi

import (
	_ "unsafe"

	"github.com/usbarmory/tamago/arm64"

	"hop-os/metal/idle"
)

// Gedeeld geheugenplan onder de kernel-load (0x200000) en boven het
// firmware/TF-A-gebied (< 0x80000). Geldig op beide boards ómdat beide op
// 0x200000 laden (zie docs/rpi5.md resp. docs/rpi4.md).
const (
	// ParkBase/ParkCount: park-code voor secundaire cores en hun
	// levensteken-tellers (geplant door de probes, zie ParkCode).
	ParkBase  = 0x1F0000
	ParkCount = 0x1F8000

	// BootScratch: cpuinit.s van het board schrijft er het boot-EL (vóór
	// de drop naar EL1); BootEL() leest het. Moet gelijk zijn aan de
	// BOOT_SCRATCH-#define in de cpuinit.s van beide boards.
	BootScratch = 0x1FF000
)

// ARM64 core-instantie (zelfde constructie als board/qemuvirt).
var ARM64 = &arm64.CPU{
	TimerOffset: 1,
}

//go:linkname ramStackOffset runtime/goos.RamStackOffset
var ramStackOffset uint = 0x100

// hwinit1: post-World lagere-laag-init. CNTFRQ is door de firmware/TF-A
// gezet; InitGenericTimers(0, 0) berekent alleen de TimerMultiplier.
//
//go:linkname hwinit1 runtime/goos.Hwinit1
func hwinit1() {
	ARM64.Init()
	ARM64.EnableCache()
	ARM64.InitGenericTimers(0, 0)
	idle.Enable() // ná Init (die zet de default governor)
}

//go:linkname nanotime runtime/goos.Nanotime
func nanotime() int64 {
	return ARM64.GetTime()
}

// mpidr leest MPIDR_EL1 (cpu_arm64.s).
func mpidr() uint64

// MPIDR geeft het rauwe register; de nummering (aff0 op de A72, aff1 op de
// A76) is boardspecifiek — zie CoreID in het board-pakket.
func MPIDR() uint64 { return mpidr() }
