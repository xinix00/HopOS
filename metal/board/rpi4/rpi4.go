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

import (
	"hop-os/metal/board/raspi"
	"hop-os/metal/dev"
	"hop-os/metal/layout"
)

// Het PA-plan van de Pi 4 (fase P1) — zelfde recept als de Pi 5, op adressen
// die in élke Pi 4-RAM-variant passen (1/2/4/8GB). Alles ruim vrij van:
// TF-A/armstub (< ~0x20000), park/scratch (0x70000-0x7F008), de HOP-kern
// (load 0x80000 + 128MB) en de DTB (0x0f000000). De pool is bewust
// conservatief (512MB) zodat 'm ook een 1GB-Pi 4 dekt; de volle RAM benutten
// (DTB /memory + /memreserve/) is de vervolgstap, net als op de Pi 5.
const revokeVecAsm = 0x8B000 // = faultdump2-tabel in cpuinit.s (VBAR_EL2 core 0)

func init() {
	// Alleen de HOP-core (MPIDR-aff 0) zet het plan — het leest de DTB fysiek,
	// wat een app-core onder stage-2 niet kan (en niet nodig heeft: HOP bezit
	// het plan). Zie de uitgebreide toelichting in board/rpi5/rpi5.go.
	if raspi.MPIDR()&0xFFFFFF != 0 {
		return
	}
	p := layout.Plan{
		CtrlPA:        0x10000000,
		RingPA:        0x11000000,
		Stage2PA:      0x12000000,
		RevokeVecPA:   revokeVecAsm,
		NetRingPA:     0x13000000,
		BootScratchPA: raspi.BootScratch, // 0x7F000, cpuinit-vast
		NetDMAPA:      0x14000000,        // NIC-DMA (GENET, fase P2 — zelfde plek als de Pi 5)
	}
	// Pool = het volledige DRAM (DTB /memory) minus de vaste regio's; terugval
	// op een conservatieve 512MB (past in élke Pi 4-variant) als de DTB faalt.
	p.Pool = raspi.DTBPool(uintptr(dev.Read64(DTBPtr)), p)
	if len(p.Pool) == 0 {
		p.Pool = []layout.Region{{Base: 0x20000000, Size: 0x20000000}}
	}
	layout.UsePlan(p)
}

// BCM2711-adressen ("low peripheral mode", de default: MMIO onder 4GB).
const (
	// PL011 UART0 op GPIO14/15 (header-pin 8/10) — de Pi 4 heeft geen
	// aparte debug-connector. De bootloader configureert hem (115200)
	// zodra hij zelf logt — config.txt: uart_2ndstage=1 — dus printk hoeft
	// alleen DR te vullen; dtoverlay=disable-bt houdt de PL011 bij de
	// header (anders claimt Bluetooth hem).
	UART0Base = 0xFE201000 // PL011-poke via metal/pl011 (offsets/bit gedeeld)

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

	// DTBPtr: cpuinit.s legt hier (primary, MMU uit) de DTB-pointer die de
	// firmware in x0 meegaf — +8 na de boot-EL-scratch (raspi.BootScratch =
	// 0x7F000, gelijk aan het DTB_PTR-#define in cpuinit.s). board.MemTotal
	// parset 'm met metal/fdt.
	DTBPtr = 0x7F008
)

// CoreID geeft de eigen core-index. De Cortex-A72 nummert cores in
// affiniteit-0 (géén MT-formaat) — anders dan de A76 op de Pi 5 (aff1)!
func CoreID() int { return int(raspi.MPIDR() & 0xFF) }

// target vertaalt een core-index naar het PSCI/MPIDR-target voor de A72.
func target(core uint64) uint64 { return core }
