// Package raspi is de gedeelde board-laag voor de Raspberry Pi-familie
// (BCM2711 = Pi 4, BCM2712 = Pi 5). Beide boards booten identiek: de
// firmware laadt een raw image op 0x80000 (de Pi 5-EEPROM negeert
// kernel_address — gemeten 2026-07-09; de Pi 4 laadt daar per default) en
// levert ons op EL2 af met TF-A/armstub op EL3 → PSCI via SMC.
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

	"hop-os/metal/dev"
	"hop-os/metal/idle"
)

// Gedeeld geheugenplan ONDER 0x80000 — want (gemeten 2026-07-09): de Pi 5-
// EEPROM-bootloader negeert kernel_address en laadt raw images op 0x80000;
// de Pi 4 laadt op 0x200000 (kernel_address gehonoreerd) dus voor die board
// ligt dit gebied nog ruimer onder de load. Boven het TF-A/armstub-gebied
// (< ~0x20000) blijven.
const (
	// ParkBase/ParkCount: park-code voor secundaire cores en hun
	// levensteken-tellers (geplant door de probes, zie ParkCode).
	ParkBase  = 0x70000
	ParkCount = 0x78000

	// BootScratch: cpuinit.s van het board schrijft er het boot-EL (vóór
	// de drop naar EL1); BootEL() leest het. Moet gelijk zijn aan de
	// BOOT_SCRATCH-#define in de cpuinit.s van beide boards.
	BootScratch = 0x7F000
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
	// 'H'-marker (rauwe DR-poke, geen FIFO-poll): de runtime leeft en is
	// door rt0 + Hwinit0 (tamago's vroege MMU-init) heen — bisect-punt
	// tussen 'R' (cpuinit) en de main-banner. ALLEEN op de primaire core
	// (MPIDR-affiniteit 0 — dekt A72-aff0 én A76-aff1): een app-core draait
	// onder stage-2 zonder mapping voor het UART-adres, dus dezelfde poke zou
	// een app daar bij boot meteen zijn core kosten (fault → CPU_OFF).
	if MPIDR()&0xFFFFFF == 0 {
		dev.Write32(0x107d001000, 'H')
	}

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

// cntfrq/cntpct lezen de generic-timer-registers (cpu_arm64.s).
func cntfrq() uint64
func cntpct() uint64

// CNTFRQ geeft de counterfrequentie die de firmware zette (verwacht 54MHz op
// de Pi; 0 = niet gezet → tamago's timers en time.Sleep zijn dan dood).
func CNTFRQ() uint64 { return cntfrq() }

// CNTPCT geeft de rauwe fysieke counter. Kan trappen als EL1PCTEN uit staat
// (zie cpu_arm64.s) — kondig de read aan vóór je hem doet.
func CNTPCT() uint64 { return cntpct() }

// spin (cpu_arm64.s) draait n afhankelijke SUBS-iteraties.
func spin(n uint64)

// Spin brandt n CPU-cycli (±dual-issue-marge) — met CNTPCT eromheen is dat
// de kloksnelheidsmeting van de probes.
func Spin(n uint64) { spin(n) }
