// Package rpi4 is het HopOS-board-pakket voor de Raspberry Pi 4 (BCM2711,
// 4× Cortex-A72) — het tweede edge-target naast de Pi 5, toegevoegd
// 2026-07-07. Boot zonder UEFI: de EEPROM-bootloader laadt start4.elf, die
// kernel8.img van de SD-kaart laadt (arm64 Image-header, zie image/mkkernel).
// Wij zetten kernel_address=0x200000 in config.txt (default is hier 0x80000)
// zodat het geheugenplan één-op-één gelijk is aan de Pi 5 — daardoor is de
// hele gedeelde laag (board/raspi) ongewijzigd bruikbaar.
//
// PSCI: de stock armstub8 parkeert secundaire cores in een spin-table en
// heeft GÉÉN PSCI — een SMC verdwijnt dan in een lege EL3-vector (hang).
// Een zelfgebouwde upstream-TF-A bl31.bin als armstub (armstub=bl31.bin in
// config.txt) is op dit board dus verplicht vanaf de eerste boot; die levert
// ons op EL2 af met PSCI via SMC, precies als op de Pi 5. Zie docs/rpi4.md.
//
// Hier staat alleen het BCM2711-eigene: UART-adres (printk + cpuinit.s),
// GIC-basis, MPIDR-nummering (A72: aff0) en de RNG; de rest komt uit
// board/raspi. board.go registreert het geheel als board.Board.
//
// Alleen voor GOOS=tamago GOARCH=arm64.
package rpi4

import "hop-os/metal/board/raspi"

// BCM2711-adressen ("low peripheral mode", de default: MMIO onder 4GB).
const (
	// PL011 UART0 op GPIO14/15 (header-pin 8/10) — de Pi 4 heeft geen
	// aparte debug-connector. De bootloader configureert hem (115200)
	// zodra hij zelf logt — config.txt: uart_2ndstage=1 — dus printk hoeft
	// alleen DR te vullen; dtoverlay=disable-bt houdt de PL011 bij de
	// header (anders claimt Bluetooth hem).
	UART0Base = 0xFE201000
	uartDR    = UART0Base + 0x00
	uartFR    = UART0Base + 0x18
	frTXFF    = 1 << 5

	// GIC-400 (GICv2, zelfde blok en layout als de Pi 5, andere basis).
	// Fase P1: hard-kill-SGI's via GICD_SGIR; de probe raakt de GIC niet.
	GICBase  = 0xFF840000
	GICDBase = GICBase + 0x1000
	GICCBase = GICBase + 0x2000

	// GENET v5: de geïntegreerde NIC (géén RP1/PCIe zoals de Pi 5 —
	// metal/gem geldt hier niet; fase P2 wordt een eigen GENET-driver).
	// De probe leest alleen SYS_REV_CTRL (+0x0) en de UMAC-MAC-registers.
	GENETBase = 0xFD580000

	// RNG200: zelfde hardware-RNG-blok als de Pi 5 (daar op 0x107d208000);
	// de echte driver wordt t.z.t. één gedeeld stuk in board/raspi (P2).
	RNG200Base = 0xFE104000
)

// CoreID geeft de eigen core-index. De Cortex-A72 nummert cores in
// affiniteit-0 (géén MT-formaat) — anders dan de A76 op de Pi 5 (aff1)!
func CoreID() int { return int(raspi.MPIDR() & 0xFF) }

// target vertaalt een core-index naar het PSCI/MPIDR-target voor de A72.
func target(core uint64) uint64 { return core }
