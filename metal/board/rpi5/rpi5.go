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
// Geverifieerd vs. nog te meten op het board: zie docs/rpi5.md — de
// probe-image (metal/cmd/probe5) rapporteert de aannames via de debug-UART.
//
// Alleen voor GOOS=tamago GOARCH=arm64.
package rpi5

import (
	"hop-os/metal/board/raspi"
	"hop-os/metal/layout"
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
// revokeVecAsm = de EL2-vectortabel (faultdump2, 0x8B000) die cpuinit.s al
// voor de boot-diagnostiek installeert en waar VBAR_EL2 van core 0 op staat.
// De revoke-HVC-handler wordt daar door stage2.InitVectors ingeplugd (offset
// 0x400 — sync vanuit lager EL); de andere 15 vectoren blijven de Y-dump.
const revokeVecAsm = 0x8B000

func init() {
	layout.UsePlan(layout.Plan{
		CtrlPA:        0x10000000,
		RingPA:        0x11000000,
		Stage2PA:      0x12000000,
		RevokeVecPA:   revokeVecAsm,
		NetRingPA:     0x13000000,
		BootScratchPA: raspi.BootScratch, // 0x7F000, cpuinit-vast
		Pool: []layout.Region{
			{Base: 0x20000000, Size: 0x60000000}, // 512MB..2GB
		},
	})
}

// BCM2712-adressen (40-bit MMIO boven 4GB; tamago's identity-map dekt 512GB,
// alles buiten de RAM-declaratie is device-nGnRnE).
const (
	// De dedicated debug-UART (PL011, de 3-pins JST-SH-connector; in Linux
	// ttyAMA10). De firmware initialiseert hem (baud 115200) zodra hij zelf
	// bootlogs schrijft — config.txt: uart_2ndstage=1 — dus printk hoeft
	// alleen DR te vullen; wij programmeren geen clocks.
	UART0Base = 0x107d001000 // PL011-poke via metal/pl011 (offsets/bit gedeeld)

	// GIC-400 (GICv2 — géén v3: SGI's gaan hier via GICD_SGIR, niet via
	// systeemregisters). Fase P1: hard-kill-SGI's; de probe raakt de GIC niet.
	GICBase  = 0x107fff8000
	GICDBase = GICBase + 0x1000
	GICCBase = GICBase + 0x2000

	// DTBPtr: cpuinit.s legt hier (primary, MMU uit) de DTB-pointer die de
	// firmware in x0 meegaf — laag DRAM onder de kernel, zelfde plek als de
	// boot-EL-scratch (+8). board.MemTotal parset 'm met metal/fdt.
	DTBPtr = 0x7F008

	// AVS-monitor (thermiek): brcm,bcm2711-thermal in de BCM2712-DTB —
	// temperatuur = slope×raw + offset uit de thermal-zone (zie probe5).
	AVSMonBase = 0x107d542000

	// Externe PCIe-controller (pciex1, de FFC waar NVMe/AI-HAT's wonen;
	// brcm,bcm2712-pcie). RP1 hangt op z'n broer pcie@1000120000.
	PCIeX1Base = 0x1000110000
)

// CoreID geeft de eigen core-index. LET OP: de Cortex-A76 nummert cores in
// affiniteit-1 (MT-formaat: aff0 = thread, altijd 0) — anders dan QEMU's
// A53 en de Pi 4's A72 (aff0). Zie ook target() hieronder (PSCI, board.go).
func CoreID() int { return int(raspi.MPIDR() >> 8 & 0xFF) }

// target vertaalt een core-index naar het PSCI/MPIDR-target voor de A76.
func target(core uint64) uint64 { return core << 8 }
