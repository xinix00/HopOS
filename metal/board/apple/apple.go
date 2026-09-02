// Package apple is de board-basis voor Apple silicon, vandaag de Mac mini M4
// (t8132, target "J773g": 6× E-core "sawtooth" in cluster 0 + 4× P-core
// "everest" in cluster 1, 24GB LPDDR5). Geen firmware met PSCI, geen GIC, geen
// device tree van de fabrikant: iBoot levert een Apple Device Tree (ADT) en
// m1n1 (Asahi) is de laag die ons afzet — op EL2, MMU uit, x0 = een door m1n1
// geprepareerde FDT of nul. Gemeten 28-08 via m1n1's proxy: CurrentEL=2, VTCR_EL2
// vrij beschrijfbaar (Apple's SPTM staat de stage-2-kooi niet in de weg), 9 van
// 10 cores starten via m1n1's spin-table.
//
// Wat dit board anders maakt dan de andere ARM-boards, en waar dat woont:
//
//   - DRAM begint op 1TiB (0x100_0000_0000). tamago's vlakke 39-bit-map (512GB)
//     reikt daar niet; de tamago-fork (hopos-highram) bouwt een L0-tabel zodra
//     RamStart boven de 512GB ligt. Het venster hier (RamBase) ligt 4GB boven
//     de DRAM-basis: ruim boven m1n1, zijn heap en zijn kernel-buffer.
//   - Geen PSCI. m1n1 parkeert de secundaire cores in een spin-table (WFE) en
//     dat is precies het cpu-release-addr-protocol: Release() in spintable.go
//     is de CPUOn van dit board. De release-adressen komen van de loader.
//   - Geen firmware-bootparams in x0 (nog): de loader (image/apple/load-probe.py)
//     legt een param-blok op ParamBase met alles wat uit de ADT nodig is —
//     UART-bases, DRAM, framebuffer, spin-table. params.go leest het.
//   - Console = de dockchannel-FIFO en/of de Samsung-UART (uart0); welke van de
//     twee de laptop bereikt is nog een meting, dus console.go schrijft naar
//     allebei. Beeld: de display-firmware (DCP) faalt onder m1n1 op M4, dus geen
//     framebuffer-console tot dat opgelost is.
//   - EL2 draait vermoedelijk met E2H=1 (VHE, geen FEAT_E2H0): cpuinit.s
//     probeert E2H te wissen en legt de effectieve HCR_EL2 op de scratch, zodat
//     de probe het meet in plaats van aanneemt. De stage-2-kooi (cpu/el2) is
//     daar nog niet op geaudit — dat is het werk ná de probe.
//
// Alleen voor GOOS=tamago GOARCH=arm64.
package apple

import (
	_ "unsafe" // voor go:linkname

	"github.com/usbarmory/tamago/arm64"

	"github.com/xinix00/HopOS/metal/cpu/idle"
	"github.com/xinix00/HopOS/metal/dev"
)

const (
	// DRAM-basis van élk Apple-silicon-systeem sinds de M1 (ADT /chosen
	// dram-base; GEMETEN 28-08 op de M4 mini: 0x100_0000_0000, 24GB).
	DRAMBase = 0x100_0000_0000

	// RamBase: het HOP-venster. 4GB boven de DRAM-basis, want daaronder woont
	// de boot-keten: iBoot's boot-args en de ADT (~0x100_0000_0000+0x36c4000),
	// m1n1 zelf (0x100_03a9_0000), zijn dlmalloc-arena (128MB) en de
	// heapblock-allocaties waar linux.py zijn kernel neerlegt (tot 512MB). De
	// bovenkant van het DRAM is van iBoot (framebuffer en carveouts vanaf
	// ~0x105_e530_4000). 4GB is dus ruim vrij in beide richtingen.
	//
	// Het image is niet positie-onafhankelijk: de loader schrijft het op dít
	// adres en springt ernaar. Moet byte-gelijk zijn aan RAM_BASE in
	// image/apple/load-probe.py en aan de #defines in cpuinit.s.
	RamBase = DRAMBase + 0x1_0000_0000 // 0x101_0000_0000

	// Scratch-woorden die cpuinit.s (MMU uit, EL2) neerlegt vóór de drop naar
	// EL1. Binnen het eigen venster, in het gat tussen tamago's tabellen
	// (+0x4000..+0x9000, plus de L0/L1-laag van de fork op +0x9000..+0xB000)
	// en de tekst op +0x10000. Pariteit met cpuinit.s.
	BootScratch    = RamBase + 0xE000   // CurrentEL bij binnenkomst (2 verwacht)
	HCRScratch     = BootScratch + 0x10 // HCR_EL2 zoals hij ná onze write leest
	CNTHCTLScratch = BootScratch + 0x18 // CNTHCTL_EL2 idem
	// De hop-woorden, naast de andere scratch. Ze moeten HIER staan en niet in
	// de struct-regio verderop: die is nog niet van ons zolang m1n1 leeft, en
	// een schrijf erheen levert een SError met L2C_ERR op — gemeten 29-08 met
	// m1n1's eigen exception-handler, FAR 0x1011a01000. Dit stuk heeft m1n1's
	// proxy zelf al beschreven (het image, het param-blok), dus het is bereikbaar.
	//
	// Twee werelden lezen ze: een core met de MMU uit (rechtstreeks geheugen) en
	// een met de MMU aan (gecachet). Wie hier schrijft veegt zijn cacheregel dus
	// mee — in assembly met dc civac, in Go met dev.CleanInv.
	MPIDRScratch = BootScratch + 0x20 // MPIDR van de boot-core
	HopAlive     = BootScratch + 0x28 // de zuinige core: "ik leef, parkeer door"
	HopParkPC    = BootScratch + 0x30 // adoptie-entry voor de geparkeerde core
	HopParkArg   = BootScratch + 0x38 // x0 voor die adoptie
	HopParkFor   = BootScratch + 0x40 // MPIDR van wie die adoptie bedoeld is

	// Wat de bootstub achterliet (bootstub.s). StubSrc is het adres waar de
	// firmware het image neerzette — en dus de waarde die RVBAR draagt, waarmee
	// de vraag "zijn de cores van ons" beantwoord is. StubX0 is de x0 waarmee
	// we binnenkwamen: iBoot's boot_args-blok, de enige ingang die dit board
	// van de firmware krijgt (firmware.go).
	StubSrc = BootScratch + 0x48
	StubX0  = BootScratch + 0x50

	// ParamBase: het param-blok van de loader (params.go voor de layout).
	ParamBase = RamBase + 0xE100

	// HopRAMSize: het venster van de HOP-agent (cmd/hopos/board_apple.go);
	// de probe neemt 64MB. Alles hieronder in de eerste GB van het venster
	// is van HOP zelf; StructBase begint erboven.
	HopRAMSize = 0x1000_0000 // 256MB

	// StructBase: HOP's eigen structuren, BUITEN élke RAM-declaratie en dus
	// device-gemapt → coherent met een core die met MMU uit binnenkomt, zonder
	// cache-onderhoud (dezelfde indeling als rk3566/plan.go, ruimer bemeten:
	// de stage-2-blokken gaan tot de kooi-cap van 128 slots = 8MB).
	//
	//	+0x0000000   64KB  control-pages van HOP's eigen cores (NodeCtrlPA)
	//	+0x00F0000    2KB  EL2-vectortabel van de boot-core (TrapVecPA — cpuinit-vast!)
	//	+0x00F8000    8B   kern-flip-vluchtrecorder (FlipScratch) — MOET buiten
	//	                   het image liggen: iBoot legt het bootobject bij élke
	//	                   boot terug over RamBase+0..imageSize, dus een spoor
	//	                   op de boot-scratch wist zichzelf (gemeten 01-09)
	//	+0x0100000  8.1MB  app-core-vectoren + stage-2-tabelblokken (CagePA)
	//	+0x0A00000   4KB   levenstekenwoorden van de probe (WakeBase)
	//	+0x1000000   8MB   NIC-DMA (NetDMAPA) — de tg3
	//	+0x1800000   8MB   opslag-DMA (StorageDMAPA) — de queues van de ANS
	//	+0x2000000         vanaf hier de partitie-pool (PoolBase)
	StructBase  = RamBase + HopRAMSize
	NodeCtrlPA  = StructBase + 0x0000000
	RevokeVec   = StructBase + 0x00F0000
	FlipScratch = StructBase + 0x00F8000
	CagePA      = StructBase + 0x0100000
	WakeBase    = StructBase + 0x0A00000

	NetDMAPA = StructBase + 0x1000000
	PoolBase = StructBase + 0x2000000

	// StorageDMA is waar de queues en TCB-tabellen van de ANS liggen. Buiten
	// élke RAM-declaratie, dus device-gemapt en ongecachet — de coprocessor
	// leest ze zonder dat wij cache-onderhoud doen.
	StorageDMAPA   = StructBase + 0x1800000
	StorageDMASize = 0x800000

	// EL2Vectors: cpuinit.s bouwt hier (MMU uit) de EL2-vectortabel van de
	// boot-core: 16 × 0x80, elk een sprong naar el2fault, dat de exceptie op
	// de dockchannel meldt en parkeert — op nieuw silicium is een stille hang
	// de duurste uitkomst. Het ís RevokeVec: HOP's stage2.InitVectors plugt
	// daar alleen de HVC-revoke-handler in (offset 0x400) en laat onze
	// foutmelders staan, precies zoals layout.Plan dat toestaat. Pariteit met
	// EL2_VECTORS in cpuinit.s wordt in SetupPlan gecheckt.
	EL2Vectors = RevokeVec

	// MMIO uit de ADT (reg + de arm-io-ranges, 0x2_0000_0000 erbij), beide
	// bevestigd door m1n1's bootlog op dit ijzer. De loader levert ze óók in
	// het param-blok; dit zijn de terugvalwaarden als dat blok ontbreekt.
	DockChannelBase = 0x3_8812_8000 // aapl,dock-channels — m1n1's console op M4
	UART0Base       = 0x3_ad20_0000 // uart-1,samsung — de klassieke debug-UART
)

// ARM64 core-instantie (zelfde constructie als de andere ARM-boards).
var ARM64 = &arm64.CPU{
	TimerOffset: 1,
}

// RamStart en RamSize worden door de image zelf gedefinieerd (cmd/probeapple);
// alleen de stack-offset is voor iedereen gelijk.
//
//go:linkname ramStackOffset runtime/goos.RamStackOffset
var ramStackOffset uint = 0x100

//go:linkname hwinit1 runtime/goos.Hwinit1
func hwinit1() {
	ARM64.Init()
	ARM64.EnableCache()
	// CNTFRQ is door iBoot gezet (Apple: 24MHz); InitGenericTimers(0,0) leest
	// hem en berekent alleen de multiplier. EL1-toegang tot de fysieke teller
	// vereist CNTHCTL_EL2.EL1PCTEN — cpuinit.s zet die in beide lay-outs.
	ARM64.InitGenericTimers(0, 0)
	idle.Enable()
	// De deadline-slaap (WFI op de fysieke timer) aanzetten: op dit silicium
	// keert WFE ín de scheduler-lus meteen terug (3,3M idle-rondes/s) terwijl
	// WFI met een timer-deadline exact slaapt — beide gemeten met cmd/probeapple
	// op 29-08, zie docs/archief/apple-m4.md. Ná Enable: die zet de cap.
	//
	// Maar alleen als de timer-FIQ deze core ook echt bereikt. Op t8132 is dat
	// niet vanzelfsprekend: op een core die de firmware niet zelf configureerde
	// gáát de timer wel af (ISTATUS wordt 1) maar wekt hij niet, en het register
	// dat die poort opent is vergrendeld. WFI is dan een eeuwige slaap. Meten,
	// niet aannemen — TimerWakes doet dat zonder ooit te kunnen hangen.
	if TimerWakes() {
		idle.Use(idle.WFISleep)
	}
}

//go:linkname nanotime runtime/goos.Nanotime
func nanotime() int64 {
	return ARM64.GetTime()
}

// CoreID geeft de logische core-index zoals m1n1 (en Linux) hem nummert:
// eerst de zes E-cores van cluster 0, dan de vier P-cores van cluster 1.
// Apple's MPIDR: aff0 = core in het cluster, aff1 = cluster, aff2 = die.
// GEMETEN 28-08 (m1n1's smp-log): cpu 0..5 = (0:0:0..5), cpu 6..9 = (0:1:0..3).
// De clustergrootte 6 is dit silicium (t8132); een ander Apple-model krijgt
// hier zijn eigen tabel.
func CoreID() int {
	m := dev.MPIDR()
	return int(m>>8&0xFF)*6 + int(m&0xFF)
}

// BootEL geeft het exceptielevel waarop m1n1 ons afleverde (scratch van
// cpuinit.s). HopOS eist ≥2. GEMETEN 28-08 via de proxy: 2.
func BootEL() int { return int(dev.Read64(BootScratch)) }

// BootMPIDR is het MPIDR van de core die de boot deed — op Apple géén core 0:
// m1n1 boot op een P-core (cpu 6, MPIDR aff1=1). Alles wat "alleen op de
// HOP-core" moet gebeuren vergelijkt hiermee, niet met nul.
func BootMPIDR() uint64 { return dev.Read64(MPIDRScratch) }

// EffectiveHCR geeft HCR_EL2 zoals hij ná cpuinit's write teruglas: bit 34
// (E2H) zegt of dit silicium FEAT_E2H0 heeft (0 = wij draaien nVHE zoals
// elk ander board; 1 = E2H is RES1 en de kooi-code moet de _EL12-encoderingen
// leren). Bit 27 (TGE) hoort 0 te zijn — anders bestaat EL1 niet eens.
func EffectiveHCR() uint64 { return dev.Read64(HCRScratch) }

// EffectiveCNTHCTL geeft CNTHCTL_EL2 zoals teruggelezen.
func EffectiveCNTHCTL() uint64 { return dev.Read64(CNTHCTLScratch) }
