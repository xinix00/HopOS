// Package rpi5 is het HopOS-board-pakket voor de Raspberry Pi 5 (BCM2712,
// 4× Cortex-A76) — fase P: het eerste echte silicium en een blijvend
// productie-target (edge). Boot zonder UEFI: de EEPROM-bootloader laadt een
// raw kernel_2712.img van de SD-kaart (arm64 Image-header, zie
// image/mkkernel) en levert ons op EL2 af; PSCI komt van de armstub
// (TF-A BL31) op EL3 via SMC.
//
// Het pakket levert de verplichte tamago runtime-hooks (RamStackOffset,
// Hwinit1, Printk, Nanotime, InitRNG, GetRandomData; RamStart/RamSize komen
// uit de image) en registreert zich als board.Board (board.go).
//
// Geverifieerd vs. nog te meten op het board: zie docs/rpi5.md — de
// probe-image (metal/cmd/probe5) rapporteert de aannames via de debug-UART.
//
// Alleen voor GOOS=tamago GOARCH=arm64.
package rpi5

import (
	_ "unsafe"

	"github.com/usbarmory/tamago/arm64"

	"hop-os/metal/idle"
)

// BCM2712-adressen (40-bit MMIO boven 4GB; tamago's identity-map dekt 512GB,
// alles buiten de RAM-declaratie is device-nGnRnE).
const (
	// De dedicated debug-UART (PL011, de 3-pins JST-SH-connector; in Linux
	// ttyAMA10). De firmware initialiseert hem (baud 115200) zodra hij zelf
	// bootlogs schrijft — config.txt: uart_2ndstage=1 — dus printk hoeft
	// alleen DR te vullen; wij programmeren geen clocks.
	UART0Base = 0x107d001000
	uartDR    = UART0Base + 0x00
	uartFR    = UART0Base + 0x18
	frTXFF    = 1 << 5

	// GIC-400 (GICv2 — géén v3: SGI's gaan hier via GICD_SGIR, niet via
	// systeemregisters). Fase P1: hard-kill-SGI's; de probe raakt de GIC niet.
	GICBase  = 0x107fff8000
	GICDBase = GICBase + 0x1000
	GICCBase = GICBase + 0x2000

	// bootScratch: cpuinit.s schrijft er het boot-EL (vóór de drop naar
	// EL1); BootEL() leest het. Ligt onder elke RAM-declaratie (kernel laadt
	// op 0x200000) en buiten het TF-A/firmware-gebied (< 0x80000). De DTB
	// wordt met device_tree_address in config.txt bewust elders gelegd.
	bootScratch = 0x1FF000

	// parkBase/parkCount: scratch voor de probe (park-code voor secundaire
	// cores + hun levensteken-tellers). Zelfde vrije regio als bootScratch.
	ParkCode  = 0x1F0000
	ParkCount = 0x1F8000
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

// MPIDR geeft het rauwe register (de probe rapporteert 'm ter verificatie).
func MPIDR() uint64 { return mpidr() }

// CoreID geeft de eigen core-index. LET OP: de Cortex-A76 nummert cores in
// affiniteit-1 (MT-formaat: aff0 = thread, altijd 0) — anders dan QEMU's
// A53 (aff0). Zie ook target() in psci.go.
func CoreID() int { return int(mpidr() >> 8 & 0xFF) }

// target vertaalt een core-index naar het PSCI/MPIDR-target voor de A76.
func target(core uint64) uint64 { return core << 8 }
