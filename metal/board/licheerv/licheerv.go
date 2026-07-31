// Package licheerv is de BASIS-helft van het LicheeRV Nano-board (Sophgo
// SG2002 / CV181x, XuanTie C906): de runtime-hooks die élke binary op dit
// board nodig heeft — console, tijd, RNG, RAM-plan, cache-onderhoud. De
// HOP-helft (board/licheerv/hop) implementeert daarbovenop board.Board:
// harts starten/killen via het reset-blok. Zelfde splitsing als raspi/rpi5.
//
// Dit is het eerste RISC-V-board van HopOS. Wat hier fundamenteel anders is
// dan op ARM staat in board/board_riscv64.go (geen EL2/PSCI/stage-2, wel
// machine mode + PMP-kooi) en in kern/cage.
//
// Registeradressen geverifieerd uit de vendor-dts
// (LicheeRV-Nano-Build: build/boards/default/dts/cv181x_riscv/ +
// sg200x/sg2002_licheervnano_sd):
//
//	PLIC   0x70000000
//	CLINT  0x74000000 (T-Head c900, sifive-layout, GEEN 64-bit MMIO)
//	UART0  0x04140000 (DW APB 16550, reg-shift=2, door FSBL op 115200 gezet)
//	DRAM   0x80000000, 256MB
//	RTCCLK 25MHz (timebase-frequency)
//
// Het tamago-image wordt als MONITOR-slot in fip.bin geplaatst en door de
// vendor-FSBL geladen en in M-mode aangesprongen (i.p.v. OpenSBI) — op
// 0x83000000, NIET op DRAM-start: de FSBL decomprimeert daarna U-Boot naar
// 0x80200020 en zou een groter image overschrijven (gemeten 30-07). Het
// boot-recept staat in image/licheerv-agent.sh.
package licheerv

import (
	_ "unsafe"

	"github.com/usbarmory/tamago/riscv64"
)

// Peripheral base addresses
const (
	PLIC_BASE  = 0x70000000
	CLINT_BASE = 0x74000000
	UART0_BASE = 0x04140000

	// timebase-frequency uit de dts (de vaste 25MHz-osc, ongevoelig voor
	// terugklokken van de core — zie clock.go)
	RTCCLK = 25000000

	// De slot-geometrie van dít board: waar een app-partitie ligt en waar
	// zijn control page. Beide kanten kennen deze getallen — HOP plaatst het
	// slot-blob op SlotBase en geeft precies die partitie in de kooi vrij
	// (kern/cage), en de app-kant maakt er zijn RAM-plan van (mem_slot.go,
	// -tags linkramsize). Onder SlotBase hoort HOP; daarboven de app.
	// HopBase is waar ons image draait — NIET DRAM-start: de FSBL
	// decomprimeert U-Boot naar 0x80200020 nádat hij ons geladen heeft, dus
	// alles daaronder is vuil gebied (gemeten 30-07, zie
	// image/licheerv-agent.sh).
	//
	// 64MB voor HOP, niet 80: het image is 12,5MB en de rest is Go-heap. Gemeten
	// (QEMU-bench 31-07, de regel bij "image placed"): HOP houdt 5MB vast bij
	// boot en 17MB op zijn zwaarste moment — een plaatsing, met de imagecopy en
	// het cache-onderhoud over een hele partitie. 64MB laat daar ruim marge op
	// voor een node met véél kooien.
	//
	// Wat vrijkomt gaat naar de apps, en het gaat naar de regio ONDER SlotBase
	// (pool B wordt 48→64MB) juist omdat SlotBase het línkadres van elk
	// riscv64-app-image is: dat verschuiven zou élk gepubliceerd artifact
	// ongeldig maken. Zo kost deze hersnit geen enkele relink.
	HopBase = 0x84000000

	SlotBase    = 0x88000000
	SlotSize    = 0x04000000 // 64MB
	SlotSizeMB  = SlotSize >> 20
	CtrlPage    = 0x8FF10000 // control page: HOP ↔ app, 4KB
	SlotScratch = 0x8FE00000 // DRAM buiten de kooi (kooi-tests)
)

// RV64 is de eigen kern (XuanTie C906) — het tamago-CPU-object dat de
// runtime-hooks (Hwinit1, Nanotime) bedienen.
var RV64 = &riscv64.CPU{
	Counter:         Counter,
	TimerMultiplier: 1,
	// required before Init()
	TimerOffset: 1,
}

// Mtime geeft de tijdteller — via de TIME CSR (rdtime), NIET via CLINT-MMIO.
// GEMETEN op silicium (30-07, trapstub): de T-Head c900-CLINT heeft géén
// mtime-register — 0xbff8 (SiFive-layout) is een gat en elke read is een
// bus-fout (mcause=5, mtval=0x7400bffc), ook met mxstatus.CLINTEE aan en
// mapbaddr=0x70000000 bevestigd. msip/mtimecmp bestaan wél (IPI/timer);
// de tijd komt op de C906 exclusief uit de TIME CSR, gevoed door dezelfde
// 25MHz osc. Bonus: dit is hetzelfde pad als het kooi-besluit voor apps
// (CLINT buiten de whitelist, tijd via rdtime) — HOP-hart en app-hart
// lezen de tijd nu identiek.
func Mtime() uint64 {
	return rdtime()
}

// Rdtime leest de TIME CSR — zelfde tijdbasis als Mtime maar zónder
// CLINT-MMIO. Op de app-hart is dit het enige tijdpad (CLINT zit niet in
// de kooi-whitelist); de probe verifieert dat beide gelijk lopen.
func Rdtime() uint64 {
	return rdtime()
}

//go:nosplit
func rdtime() uint64

// Counter returns the number of nanoseconds counted from the RTCCLK input.
func Counter() uint64 {
	// 25MHz → exact 40ns per tick
	return Mtime() * 40
}

//go:linkname nanotime runtime/goos.Nanotime
func nanotime() int64 {
	return RV64.GetTime()
}

// Init takes care of the lower level initialization triggered early in
// runtime setup (post World start).
//
//go:linkname Init runtime/goos.Hwinit1
func Init() {
	RV64.Init()
}

// Model returns the SoC model name.
func Model() string {
	return "SG2002"
}
