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
	CLINT_BASE = 0x74000000
	UART0_BASE = 0x04140000

	// timebase-frequency uit de dts (de vaste 25MHz-osc)
	RTCCLK = 25000000

	// De slot-geometrie van dít board. Beide kanten kennen deze getallen —
	// HOP plaatst het slot-blob op SlotBase en geeft precies die partitie in
	// de kooi vrij (kern/cage), en de app-kant maakt er zijn RAM-plan van
	// (mem_slot.go, -tags linkramsize). De control page woont in de staart
	// van elke partitie (layout.CtrlPageAt, slot-ABI v2).
	// HopBase is waar ons image draait — NIET DRAM-start: de FSBL
	// decomprimeert U-Boot naar 0x80200020 nádat hij ons geladen heeft, dus
	// alles daaronder is vuil gebied (gemeten 30-07, zie
	// image/licheerv-agent.sh).
	HopBase = 0x84000000

	// HopSize is HOP's venster (image + Go-heap). 32MB sinds 14-08, gemeten
	// (QEMU, agent kaal, mét een echte plaatsing en SSE-load): het image is
	// 4,9MB, de runtime houdt er 5MB van vast, en de vloer ligt tussen 16 en
	// 20MB — daaronder past de eerste 4MiB-heaparena niet meer naast het
	// image. 32 is het midden tussen die vloer en de oude 64: dubbele marge,
	// en de netstack-pot (ramSize/8) blijft met 4MB ruim voor de
	// 100Mbit-poort. De 17MB-piek uit de oude meting (31-07) was mét
	// imagecopy; een plaatsing streamt sindsdien en piekte in de meting op
	// 2MB heap-in-use.
	//
	// Wat vrijkomt (HopBase+HopSize..SlotBase, 32MB) wordt een eigen
	// pool-regio in het plan — en NIET een verschuiving van SlotBase, want
	// dat is het línkadres van elk riscv64-app-image: verschuiven zou élk
	// gepubliceerd artifact ongeldig maken. Zo kost de hersnit geen enkele
	// relink (zelfde afweging als de vorige hersnit hier).
	HopSize = 0x02000000

	SlotBase   = 0x88000000
	SlotSize   = 0x04000000 // 64MB
	SlotSizeMB = SlotSize >> 20

	// StubMbox is het VANGNET-scratch van de kooi-stub (lege slot-tabel = de
	// pre-agent-demo; productie krijgt zijn scratch via de slot-tabel in de
	// partitie-staart, layout.AbiStubOff) plus de demo-velden van slotdemo.
	// In de vrije top van de 2MB-staart, buiten élke plan-regio; heette
	// CtrlPage en stond op 0x8FF10000 — dood als control page (die woont in
	// de partitie-staart) en sinds de staart-hersnit (14-08) midden in
	// slot-blok 15. Zelfde waarde als MBOX in stub-slot.S.
	StubMbox = 0x8FFF0000
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
	// Allereerst de boot-hart-loterij (lottery.go): woont HOP volgens
	// board.HopCore op een ander hart, dan start dat hart hier het image
	// opnieuw en parkeert déze core tot zijn adoptie als app-hart. Vóór
	// al het andere: wat hieronder staat hoort maar op één core te draaien.
	hartLottery()

	RV64.Init()
}

// Model returns the SoC model name.
func Model() string {
	return "SG2002"
}
