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
	"fmt"

	"hop-os/metal/abi/layout"
	"hop-os/metal/board/raspi"
	"hop-os/metal/fw/fdt"
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
	// Board-specifiek RNG200-basisadres bekendmaken aan de gedeelde raspi-RNG
	// (board/raspi/rng.go leest RNG200Base voor crypto/rand). Vóór de MPIDR-guard:
	// Alleen de HOP-core (MPIDR-aff 0) berekent en zet het plan: het leest de
	// DTB fysiek (0x7F008 + de blob), en dat adres bestaat niet in de kooi van
	// een app-core (die draait onder stage-2). Een app-core heeft het plan ook
	// niet nodig — HOP bezit het en gebruikt de *PA-accessors; de app kent
	// alleen de IPA-constanten. Zonder deze guard faultt elke app bij zijn eigen
	// board-init (gemeten 2026-07-10: far=0x7f008).
	if raspi.MPIDR()&0xFFFFFF != 0 {
		return
	}
	// RNG200-basis bekendmaken aan de gedeelde raspi-RNG (crypto/rand) — ACHTER
	// de guard: appspike linkt dit board, dus een app draait deze init ook. Zou
	// RNG200Base vóór de guard gezet worden, dan wees getRandomData in de app
	// naar RNG200-MMIO dat in zijn stage-2-kooi niet gemapt is → fault bij de
	// eerste crypto/rand. Achter de guard blijft de RNG200Base van een app 0, en
	// dan valt getRandomData terug op de PRNG. Alleen HOP mapt en gebruikt dit MMIO.
	raspi.RNG200Base = RNG200Base
	raspi.WatchdogBase = 0x10_7d20_0000 // PM-blok (bcm2712.dtsi watchdog@7d200000)
	p := layout.Plan{
		CtrlPA:        0x10000000,
		RingPA:        0x11000000,
		Stage2PA:      0x12000000,
		RevokeVecPA:   revokeVecAsm,
		BootScratchPA: raspi.BootScratch, // 0x7F000, cpuinit-vast
		NetDMAPA:      0x14000000,        // GEM-ringen/buffers (buiten RAM-decl → ongecachet)
	}
	// De pool = het volledige DRAM (DTB /memory, ook boven 4GB) minus de vaste
	// regio's. Faalt de DTB-lezing, val terug op een conservatieve vaste pool
	// (512MB, past in élke Pi 5) — nooit fantoom-RAM uitdelen. Die terugval is
	// LUID: op een Pi met geldige DTB hoort dit pad nooit te lopen, dus als het
	// wél loopt (kromme/afwezige blob) mag het niet stil degraderen.
	dtb := raspi.DTB()
	p.Pool = raspi.DTBPool(dtb, p)
	if len(p.Pool) == 0 {
		fmt.Printf("WAARSCHUWING HOPOS_POOL_FALLBACK: geen bruikbare DTB /memory (dtb=%#x, geldig=%v) — partitie-pool valt terug op de vaste 512MB [0x20000000,0x40000000); de RAM-sanity draait dan op het layout, niet op gemeten RAM\n",
			dtb, fdt.Valid(dtb))
		p.Pool = []layout.Region{{Base: 0x20000000, Size: 0x20000000}}
	}
	layout.UsePlan(p)
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

	// AVS-monitor (thermiek): brcm,bcm2711-thermal in de BCM2712-DTB —
	// temperatuur = slope×raw + offset uit de thermal-zone (zie probe5).
	AVSMonBase = 0x107d542000

	// Externe PCIe-controller (pciex1, de FFC waar NVMe/AI-HAT's wonen;
	// brcm,bcm2712-pcie). RP1 hangt op z'n broer pcie@1000120000.
	PCIeX1Base = 0x1000110000

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
