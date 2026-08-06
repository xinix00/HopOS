// Package rk3566 is de board-basis voor de Radxa Zero 3E (Rockchip RK3566:
// 4× Cortex-A55, GIC-600, DW-APB-UART, DWMAC-4.20a GMAC, VOP2+HDMI) — Dereks
// échte doelbord ("main device", 05-08).
//
// Wat hier woont is alles wat GEEN driver is maar wél silicium-kennis: de
// runtime/goos-hooks (console, timers, RNG-seed, cpuinit), het PA-plan, en de
// SoC-glue onder de drivers — pinmux, GRF, CRU, power-domeinen, watchdog,
// temperatuursensor, TRNG, en de VOP2/HDMI-scanout. De board.Board-helft staat
// in board/rk3566/hop (alleen HOP-binaries linken die), de IP-cores in
// driver/nic/dwmac4 en driver/nic/mdio.
//
// Dat het zoveel is heeft één oorzaak, en die is gemeten: U-Boot raakt op dit
// bord noch het ethernet noch de video aan ("No ethernet found", en
// `Out: serial@fe660000` zonder vidconsole). Alles onder de MAC en alles onder
// de connector is dus van ons. Beide zijn op 06-08 op ijzer bewezen —
// gigabit-link met DHCP-lease, en 1920x1080p60 op HDMI.
//
// Boot-route (het verschil met de Pi's): geen firmware die een raw image op
// een vast adres legt, maar U-Boot. BootROM → TPL/SPL (DDR) → TF-A (bl31,
// EL3) → U-Boot (EL2) → `booti` met ons arm64-Image (mkkernel zónder -raw)
// → entry op EL2 met x0 = DTB. Dat is exact de qemuvirt/Pi-conventie, dus
// cpuinit.s is een kloon met andere scratch-adressen. hopos.cfg reist als
// extlinux-INITRD (U-Boot zet /chosen/linux,initrd-* — fw/bootcfg leest dat
// al), bootargs als APPEND-regel.
//
// De adressen hieronder komen uit het RK3566-TRM en rk3566.dtsi; wat de probe
// op 05-08 op ijzer bewees staat als GEMETEN gemarkeerd (zie
// docs/archief/radxa-zero3.md voor de volle uitslag).
package rk3566

import (
	_ "unsafe" // voor go:linkname

	"github.com/usbarmory/tamago/arm64"

	"github.com/xinix00/HopOS/metal/cpu/idle"
	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/fw/fdt"
)

const (
	// De debug-UART (UART2 op de 40-pins header, zoals elk Rockchip-bord):
	// DesignWare APB, 16550-compatibel, reg-shift 2 — dezelfde familie als de
	// LicheeRV-console. De bootloader-keten heeft hem al geconfigureerd
	// (Rockchip-default 1500000 8N1); wij pollen en schrijven alleen, dus de
	// baudrate is wat U-Boot naliet. GEMETEN 05-08: werkt.
	UART2Base = 0xFE660000

	// GMAC (snps,dwmac-4.20a) + de Rockchip-glue eronder: GRF voor
	// RGMII-mode/delays en de pinmux, CRU voor klokken en resets. Uitgewerkt in
	// grf.go, pinmux.go en cru.go; de driver zelf is driver/nic/dwmac4.
	GMAC1Base = 0xFE010000
	GRFBase   = 0xFDC60000
	CRUBase   = 0xFDD20000

	// HOP-venster. GEMETEN 05-08: U-Boot's memory-node is
	// 0x200000..0x80000000 (2046MB) — DRAM begint dus NIET op 0, de eerste 2MB
	// is TF-A, en /memreserve/ bevat verder niets (geen OP-TEE; TF-A meldt zelf
	// "No OPTEE provided by BL2"). Er is hier dus veel meer ruimte dan de 64MB
	// die de probe neemt; de agent-fase kiest het echte venster.
	//
	// 0x02200000 en niet de kernel_addr_r-waarde 0x02080000: een arm64-Image
	// hoort 2MB-uitgelijnd te landen, en 0x2080000 is dat niet. En mkkernel
	// rekent text_offset = RamBase - 0x200000, want booti legt een
	// niet-relocatable Image op bi_dram[0].start + text_offset. Met de vorige
	// combinatie landde het image 2MB te hoog en zweeg hij ("Moving Image from
	// 0x2080000 to 0x2280000" en daarna niets) — dát was de eerste meting.
	RamBase = 0x02200000

	//
	// Boot-scratch: cpuinit legt er het boot-EL en de x0 (DTB-pointer) neer,
	// vóór de MMU aanstaat. Bínnen het eigen venster (boven tamago's
	// paginatabellen op RamBase+0x4000..0x8000, onder de tekst op +0x10000),
	// dus gegarandeerd óns DRAM — een gok buiten het venster kan op dit bord
	// een TrustZone-firewall raken en dan sterft de probe vóór zijn eerste
	// byte. Moet byte-gelijk zijn aan BOOT_SCRATCH/DTB_PTR in cpuinit.s.
	BootScratch = RamBase + 0xF000 // = 0x0220F000, zie cpuinit.s
	DTBPtr      = BootScratch + 8

	// WakeBase: vier levenstekenwoorden voor de secundaire cores (park.go),
	// BEWUST BUITEN het eigen RAM-venster (64MB vanaf RamBase, plus ruim marge)
	// en ver onder waar U-Boot zijn DTB/initrd legt (~0x7ce00000).
	//
	// Waarom buiten: tamago mapt alles binnen [ramStart,ramEnd) als Normal
	// cacheable en al het overige als Device-nGnRnE. Een gewekte core komt met
	// MMU uit binnen en schrijft dus rechtstreeks naar DRAM; zou dit woord in
	// ons cacheable venster liggen, dan lezen wij onze eigen verouderde regel
	// terug — en kan onze dirty nul later zijn MPIDR nog overschrijven ook.
	// GEMETEN 05-08: precies dat gebeurde ("accepted, but core stayed silent").
	// Buiten het venster is het device-gemapt en dus coherent per constructie,
	// net zoals de park-mailbox op de Pi buiten elke RAM-declaratie ligt.
	WakeBase = 0x06300000
)

// ARM64 core-instantie (zelfde constructie als qemuvirt/tamago's imx8mp).
var ARM64 = &arm64.CPU{
	TimerOffset: 1,
}

// RamStart en RamSize worden door de image zelf gedefinieerd (zie
// cmd/proberk3566); alleen de stack-offset is voor iedereen gelijk.
//
//go:linkname ramStackOffset runtime/goos.RamStackOffset
var ramStackOffset uint = 0x100

// memTotal is het bij boot uit de DTB gelezen DRAM (0 = onbekend). Vroeg
// parsen (hwinit1, niet lui): U-Boot legt de DTB waar hij wil en niets
// garandeert dat dat geheugen blijft staan.
var memTotal uint64

//go:linkname hwinit1 runtime/goos.Hwinit1
func hwinit1() {
	ARM64.Init()
	ARM64.EnableCache()
	// CNTFRQ is door de bootloader-keten gezet (Rockchip: 24MHz);
	// InitGenericTimers(0,0) leest hem en berekent alleen de multiplier.
	ARM64.InitGenericTimers(0, 0)
	idle.Enable()

	if p := uintptr(dev.Read64(DTBPtr)); p != 0 {
		if n, ok := fdt.MemTotal(p); ok {
			memTotal = n
		}
	}
}

//go:linkname nanotime runtime/goos.Nanotime
func nanotime() int64 {
	return ARM64.GetTime()
}

// CoreID geeft de eigen core-index. De A55's in de RK3566 zitten in één
// DynamIQ-cluster en nummeren — net als de Pi 5 — in MPIDR aff1
// (0x000/0x100/0x200/0x300).
//
// GEMETEN 05-08, in twee stappen. Core 0 leest MPIDR 0x81000000 (bit31 RES1,
// MT=1, alle affiniteiten 0) → CoreID 0, wat klopt maar aff1 niet bewijst: op
// core 0 zijn aff0 en aff1 beide nul. Trede 2 besliste het: PSCI CPU_ON met
// aff1-targets (0x100/0x200/0x300) bracht alle drie de cores op, aff0-targets
// gaven INVALID_PARAMS — zie Target() hieronder.
func CoreID() int { return int(dev.MPIDR()>>8) & 0xFF }

// MemTotal geeft het bij boot gedetecteerde DRAM (0 = onbekend).
func MemTotal() uint64 { return memTotal }

// BootEL geeft het exceptielevel waarop de firmware ons afleverde, zoals
// cpuinit.s het op de scratch legde. HopOS eist ≥2 (zonder EL2 geen stage-2 en
// dus geen kooi); de main weigert te starten bij 1. GEMETEN 05-08: booti levert
// EL2.
func BootEL() int { return int(dev.Read64(BootScratch)) }

// Target vertaalt een core-index naar het MPIDR-target voor PSCI. GEMETEN
// 05-08: dit silicium nummert in aff1 — CPU_ON accepteert 0x100/0x200/0x300 en
// weigert 0x1/0x2/0x3 met INVALID_PARAMS, en de gewekte cores melden zelf
// MPIDR 0x81000100/0200/0300.
func Target(core uint64) uint64 { return core << 8 }
