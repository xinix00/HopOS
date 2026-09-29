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
	// decomprimeert U-Boot naar 0x80200020 (~600KB) nádat hij ons geladen
	// heeft, dus alles daaronder is vuil gebied (gemeten 30-07, zie
	// image/licheerv-agent.sh).
	//
	// TERUG OP 0x84000000 (19-08, poging naar beneden mislukt): met
	// RUNADDR=0x80400000 BOOT HET BOARD NIET, ook al komt U-Boot maar tot
	// ~0x8029a000.
	//
	// De verklaring, en die wijst de volgende poging de ANDERE kant op: het is
	// niet het laadadres dat misging. MONITOR_LOADADDR in de FIP is een OFFSET
	// IN HET BESTAND (fiptool.pack_monitor: `len(fip_bin)`), niet een
	// DRAM-adres, en MONITOR_RUNADDR is wél het DRAM-adres waar de FSBL ons
	// neerzet én binnenspringt. Dat werkt dus. Wat er overblijft: de FSBL zelf
	// woont in het LAGE DRAM en draait daar nog — hij moet ná ons image ook
	// LOADER_2ND laden. Ons image van ~5,4MB op 0x80400000 schrijft dan dwars
	// door de code die het aan het laden is. Dát is waarom "alles onder RUNADDR
	// vuil" is: daar leeft de FSBL, niet alleen zijn U-Boot-decompressie.
	//
	// Dus: als de pool ooit één regio moet worden, moet HOP OMHOOG en niet naar
	// beneden — naar net onder de 2MB-staart (HopBase = osBase - HopSize), zodat
	// het lage DRAM volledig van de FSBL blijft en de pool 0x80000000..HopBase
	// wordt: 222MB aan één stuk, méér dan de 218 die de poging naar beneden zou
	// geven. Dat is één regel hier plus RUNADDR in image/licheerv-agent.sh, plus
	// het pool-plan. Voorwaarde: de UART eraan, want die overleeft een mislukte
	// boot en de netwerk-console niet.
	//
	// Wat de verhuizing zou opleveren staat hieronder beschreven en blijft waar;
	// alleen het adres is teruggedraaid. Eén regel hier plus RUNADDR in
	// image/licheerv-agent.sh, en de pool is weer één stuk.
	//
	// WAT DE BEDOELING WAS, en waarom het nog steeds de moeite is: HOP stond mídden in het DRAM, en dat knipte de
	// app-pool in drie stukken: 126MB boven SlotBase, 64MB eronder, en de 32MB
	// die de HopSize-hersnit ertussen vrijgaf. Drie regio's betekent dat 60MB
	// vrij kan zijn terwijl er nergens 36MB aan één stuk ligt — en dan laat HOP
	// een job toe die de plaatser moet weigeren, elke vijf seconden opnieuw
	// (19-08: die lus velde de node drie keer via gemiste watchdog-pets).
	//
	// Onderaan tegen het vuile gebied aan is de pool ÉÉN stuk van 218MB. De
	// prijs is de 4MB onder dit adres, die vroeger als pool B aan apps ging
	// zodra U-Boot klaar was: 222MB in drie stukken wordt 218MB in één stuk.
	// SlotBase verschuift NIET (0x88000000 blijft het linkadres van elk
	// app-image, dus geen enkel gepubliceerd artifact wordt ongeldig); hij ligt
	// nu binnen de regio, en dat mag omdat de kooi verplaatst — zie
	// kern/cage/relocate.go, en bewezen op ijzer (stulp draaide op 0x81c00000,
	// stulp-weather op 0x86a00000).
	//
	// 4MB en niet krapper: U-Boot beslaat 0x80200020 + ~600KB, dus tot ruwweg
	// 0x8029a000. Dat laat 1,4MB lucht voor een dikkere U-Boot van de vendor,
	// en het adres blijft 2MB-uitgelijnd zoals de partitie-korrel.
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
	RV64.Init()
}

// Model returns the SoC model name.
func Model() string {
	return "SG2002"
}
