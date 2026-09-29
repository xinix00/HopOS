// Package rpi5 is het HopOS-board-pakket voor de Raspberry Pi 5 (BCM2712,
// 4× Cortex-A76) — fase P: het eerste echte silicium en een blijvend
// productie-target (edge). Boot zonder UEFI: de EEPROM-bootloader laadt een
// raw kernel_2712.img van de SD-kaart (arm64 Image-header, zie
// image/mkkernel) en levert ons op EL2 af; PSCI komt van de armstub
// (TF-A BL31) op EL3 via SMC.
//
// Alles wat de Pi 4 en Pi 5 delen (PSCI/SMCCC, MPIDR-read, timers/idle,
// de runtime-hooks Hwinit1/Nanotime/RamStackOffset, het park/scratch-plan)
// zit in board/raspi; hier staat alleen het BCM2712-eigene: UART-adres
// (printk + cpuinit.s), GIC-basis, MPIDR-nummering (A76: aff1) en de RNG.
// board.go registreert het geheel als board.Board.
//
// Geverifieerd vs. nog te meten op het board: zie docs/archief/rpi5.md — de
// probe-image (metal/cmd/probe5) rapporteert de aannames via de debug-UART.
//
// Alleen voor GOOS=tamago GOARCH=arm64.
package rpi5

import (
	"github.com/xinix00/HopOS/metal/v2/board/raspi"
)

// Het PA-plan van de Pi 5 (fase P1): wáár control-pages, ringen en
// stage-2-tabellen fysiek liggen, en welk DRAM de partitie-pool is. Alles in
// laag DRAM, ruim vrij van: TF-A/armstub (< ~0x20000), park/scratch
// (0x70000-0x7F008), de HOP-kern (load 0x80000 + 128MB = tot 0x8080000) en
// de DTB (0x0F000000, device_tree_address in config.txt).
//
// De pool is voor de bring-up bewust conservatief — 512MB..2GB, gegarandeerd
// binnen de eerste /memory-range van elke 4/8GB-Pi. De volle 8GB benutten
// (regio's uit de DTB-/memory-ranges + /memreserve/) is de vervolgstap zodra
// de main die ranges op het board heeft geprint (verifieer eerst); de
// pool-vorm ([]Region) en VTCR PS=40-bit kunnen het al aan.
func init() {
	// Het PA-plan + RNG/watchdog-bases: gedeeld met de Pi 4 (raspi.SetupPlan —
	// zelfde plan, zelfde MPIDR-guard, zelfde DTB-pool-terugval); dit board
	// levert alleen zijn eigen MMIO-bases (bcm2712.dtsi watchdog@7d200000).
	raspi.SetupPlan(RNG200Base, 0x10_7d20_0000)
}

// BCM2712-adressen (40-bit MMIO boven 4GB; tamago's identity-map dekt 512GB,
// alles buiten de RAM-declaratie is device-nGnRnE).
const (
	// De dedicated debug-UART (PL011, de 3-pins JST-SH-connector; in Linux
	// ttyAMA10). De firmware initialiseert hem (baud 115200) zodra hij zelf
	// bootlogs schrijft — config.txt: uart_2ndstage=1 — dus printk hoeft
	// alleen DR te vullen; wij programmeren geen clocks.
	UART0Base = 0x107d001000 // PL011-poke via metal/driver/pl011 (offsets/bit gedeeld)

	// GIC-400 (GICv2 — géén v3: SGI's gaan hier via GICD_SGIR, niet via
	// systeemregisters). Fase P1: hard-kill-SGI's; de probe raakt de GIC niet.
	GICBase  = 0x107fff8000
	GICDBase = GICBase + 0x1000
	GICCBase = GICBase + 0x2000

	// DTBPtr: cpuinit.s legt hier (primary, MMU uit) de DTB-pointer die de
	// firmware in x0 meegaf — laag DRAM onder de kernel, zelfde plek als de
	// boot-EL-scratch (+8). board.MemTotal parset 'm met metal/fw/fdt.
	DTBPtr = 0x7F008

	// RNG200: het Broadcom iproc-rng200-blok (BCM2712) — hetzelfde blok als de
	// Pi 4 (daar op 0xFE104000), hier op 40-bit MMIO. De gedeelde driver zit in
	// board/raspi/rng.go; init() geeft dit adres door via raspi.RNG200Base.
	RNG200Base = 0x107d208000

	// VideoCore-firmware-mailbox (brcm,bcm2835-mbox; DT mailbox@7c013880,
	// soc-ranges 0x7c000000 → 0x10_7c000000) — metal/driver/vcmail: temperatuur,
	// ARM-klok, board-MAC.
	VCMailBase = 0x10_7C01_3880
)

// CoreID geeft de eigen core-index. LET OP: de Cortex-A76 nummert cores in
// affiniteit-1 (MT-formaat: aff0 = thread, altijd 0) — anders dan QEMU's
// A53 en de Pi 4's A72 (aff0). Zie ook target() hieronder (PSCI, board.go).
func CoreID() int { return int(raspi.MPIDR() >> 8 & 0xFF) }

// Target vertaalt een core-index naar het PSCI/MPIDR-target voor de A76
// (exported voor de PSCI-forwards in board/rpi5/hop).
func Target(core uint64) uint64 { return core << 8 }
