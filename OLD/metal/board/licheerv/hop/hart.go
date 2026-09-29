// Package hop is de HOP-helft van het LicheeRV Nano-board: het deel van
// board.Board dat alleen de HOP-kern nodig heeft. Waar de ARM-boards hier
// PSCI en EL2-trampolines neerzetten, staat op dit board het SoC-reset-blok —
// dat is op RISC-V zowel de slot-start als de kill, en tegelijk het enige dat
// de kooi van het vorige slot weer wist (gemeten op de SG2002, 30-07).
//
// Registerrecept 1:1 uit de vendor-FSBL (plat/cv181x/platform.c, reset_c906l):
//
//	0x03003024 bit6 laag    → reset assert (stopt het hart, ook uit een tight loop)
//	0x020B0004 bit13 hoog   → boot-vector-override aan (SEC_SYS +0x04)
//	0x020B0020/24           → boot-vector lo/hi
//	0x03003024 bit6 hoog    → deassert = hart start op de vector
//
// DAT RECEPT DEKT ÉÉN CORE, en dat is de hele reden dat dit bestand twee soorten
// harts kent. 0x03003024 is SOFT_CPU_RSTN uit de SG200X-TRM: bit 0..3 zijn
// CPUCORE0..3, bit 4..6 zijn CPUSYS0..2, en bit 6 (die van de vendor) is dus het
// subsysteem van de C906L. Er ís een resetbit voor de grote core — het
// vendor-FPGA-script triggert hem met 0x31 — maar er is nergens een
// boot-vector-override voor gedocumenteerd, en zonder die override start hij op
// zijn resetvector in de BROM: dat is de hele bootketen opnieuw, inclusief
// DDR-init, en dus het geheugen van een draaiende node. Daarom raakt HopOS die
// bit niet aan en is de grote core een core die parkeert in plaats van resetten
// (zie de loterij in cpuinit_riscv64.s en kern/slots coreParks).
package hop

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/board/licheerv"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

const (
	c906lReset  = 0x03003024 // SOFT_CPU_RSTN, bit 6 = het subsysteem van de C906L
	secSysCtrl  = 0x020B0004 // SEC_SYS: bit 13 = boot-vector-override
	secSysVecLo = 0x020B0020
	secSysVecHi = 0x020B0024

	// De twee harts van dit board, genummerd zoals het RESET-BLOK ze noemt —
	// niet zoals ze zichzelf zien. Dat onderscheid is gemeten (boot 5, 01-08):
	// beide cores lezen mhartid 0 op hun eigen core-lokale CLINT, dus het
	// hart-ID hier is puur een naam voor "welke knop in het reset-blok".
	//
	// Wie welke rol heeft volgt uit één getal (het globale principe,
	// abi/layout/hopcore.go): HOP woont op HopHart, elk ander hart is van de
	// apps. Geen rol-verhaal — de namen hieronder zijn silicium-kennis
	// (welke knop in het reset-blok), niet rollen.
	hartC906L = 1
	hartC906B = 0
	resetBit  = 1 << 6

	// killTickTicks: de periode van de kill-tick op een app-core zonder
	// reset-recept — 10ms op de vaste 25MHz-timebase. De afweging is
	// eenzijdig: de tick zelf is een handvol instructies, dus dit kost een
	// app niets meetbaars en begrenst hoe lang een intrekking onderweg is.
	killTickTicks = 10 * licheerv.RTCCLK / 1000
)

// Cores is het core-contract van dit board (board.Cores), met het reset-blok
// als Start en Reset — en Resettable als de eerlijke rand: het recept dekt
// één van de twee harts. De poten hieronder zijn losse functies omdat de
// hartprobe en de hop ze óók gebruiken, buiten een slot om.
func (machine) Cores() board.Cores {
	return board.Cores{
		App:        appHarts,
		Start:      hartOn,
		Reset:      hartOff,
		Resettable: hartResettable,
		State:      hartState,
	}
}

// appHarts is het globale principe in één lus: álle harts behalve waar HOP
// woont. Geen PSCI-telling om op terug te vallen: de hart-lijst is
// board-kennis, de rolverdeling is er geen — die volgt uit HopHart.
func appHarts() []int {
	var apps []int
	for _, h := range []int{hartC906B, hartC906L} {
		if h != licheerv.HopHart {
			apps = append(apps, h)
		}
	}
	return apps
}

// hartResettable: alleen de C906L heeft een reset-recept mét boot-vector (zie de
// kop van dit bestand). Voor de grote core is het antwoord nee, en daar hangt in
// kern/slots de hele levenscyclus aan: starten via een boot-pending die de
// rotatie oppikt, beëindigen via de kill-tick, en HOP blijft uit regel 0 van het
// sched-blok.
//
// Zolang de apps op de C906L wonen stelt dit niets voor; woont HOP daar
// (HopHart = 1), dan is dít de reden dat de app-core anders werkt dan op elk
// ander hart van HopOS: starten via boot-pending, beëindigen via kill-tick.
func hartResettable(hart int) bool { return hart == hartC906L }

// HartTimer geeft wat machine mode op dít hart met zijn comparator mag doen.
// In essentie één onderscheid: een parkerende core krijgt de kill-tick (het
// enige mes daar), een resetbaar hart niets (de reset is het mes) — en slapen
// mag alleen waar dat bewezen is (appHartSleepEnabled).
//
// Het comparator-adres is in beide takken feitelijk MtimecmpAddr(0): beide
// cores noemen zichzelf mhartid 0 en de CLINT-decode is core-lokaal (gemeten
// 01-08, boot 8), dus de kill-tick en HOP's eigen wekker raken elkaar niet.
// De comparator van de parkerende C906B is wekenlang productie-bewezen (HOP
// woonde er 30-07→16-08 op UseMSleep); MsipPA blijft 0 — de CLINT is
// core-lokaal, er ís geen IPI-kanaal tussen de harts.
func (m machine) HartTimer(hart int) board.HartTimer {
	if hartResettable(hart) {
		// Reset is het mes; een wekker alleen na een ronde probe op dát hart.
		if !appWaker.ok || !appHartSleepEnabled {
			return board.HartTimer{}
		}
		return board.HartTimer{MtimecmpPA: appWaker.mtimecmp, SleepCap: licheerv.SleepCapTicks}
	}
	// De parkerende core mag WÉL slapen: het bewijs is per CORE, niet per
	// board. Alle stille doden staan op naam van de C906L (01-08 als
	// app-hart, 17-08 met HOP erop); de C906B droeg twee weken productie-wfi
	// (HOP's UseMSleep, 30-07→16-08) — en sinds de hop is de parkerende core
	// precies die C906B. De vlag hieronder gate alleen nog het resetbare
	// hart. (Dit haalt ook één van de twee idle-spins van het board af —
	// fanless, en HOP's eigen slaap staat op de verdachte core juist uit.)
	return board.HartTimer{
		MtimecmpPA: licheerv.MtimecmpAddr(0),
		Tick:       killTickTicks,
		SleepCap:   licheerv.SleepCapTicks,
	}
}

// appHartSleepEnabled is de slaap-schakelaar van een RESETBAAR app-hart — op
// dit board de C906L, en dáár staat hij UIT op bewijs (01-08, boots 3-8). De
// parkerende C906B slaapt wél (HartTimer hierboven): zijn wfi is per-core
// bewezen. Het bewijs hieronder gaat dus over de C906L:
//
//   - wekker AAN (boots 6/7): tweemaal een STILLE NODEDOOD — geen panic, geen
//     fault, console dood, HOP's hart weg — na 70s resp. 40 min. Heisenbug-
//     timing, dus een race of een silicium-conditie.
//   - wekker UIT (boots 3/4): urenlang strak, alles servend.
//   - de wek-keten zelf is op het hart bewezen (probe: mtimecmp vuurt, wfi
//     keert terug) en de decode is core-lokaal (boot 8: HOP zag de 1'en van
//     hart 1's parkeerlus nooit op zijn eigen comparator) — de twee nette
//     verklaringen zijn daarmee GEMETEN afgevallen. Wat overblijft is
//     SoC-onderzoek: klokdomein-gating door de C906L-wfi? een arbiter-kwestie
//     in het c900-CLINT-blok bij gelijktijdig gebruik door beide cores? (Van
//     datzelfde blok is al gemeten dat mtime-reads bus-fouten zijn.)
//
// Tot die jacht gelopen is: park spint, de bewezen stand. Niet aanzetten op
// een redenering — alleen op een boot-soak zonder doden.
//
// LET OP dat dit de slaap gaat en NIET de kill-tick. Die twee zijn sinds de hop
// uit elkaar getrokken omdat ze los bewezen zijn: de tick schrijft alleen de
// comparator en keert meteen terug, en op dat pad is nooit iets stils gebeurd —
// wat stierf was een wfi. Zie board.HartTimer.
const appHartSleepEnabled = false

// Privilege/Firmware: HOP draait in M-mode (3). Minder kan niet — alleen
// M-mode programmeert PMP en reset harts, en zonder dat is er geen kooi. Er is
// geen PSCI en geen SBI: wij ZIJN de laag die op ARM de firmware is — ons image
// vervangt OpenSBI in het MONITOR-slot.
func (machine) Privilege() error { return board.RequireMMode(3) }
func (machine) Firmware() string {
	return fmt.Sprintf("boot: M-mode monitor (no SBI), app harts %v", appHarts())
}

// hartOn zet het hart op entry en laat hem lopen. Op een lópend hart is dit
// kill + verse start: de reset-assert stopt hem waar hij ook is, en wist zijn
// PMP-locks — precies wat een slot-restart nodig heeft.
func hartOn(hart int, entry, arg uint64) error {
	if hart != hartC906L {
		// Geen reset-recept: dit hart staat sinds de loterij geparkeerd op
		// het adoptie-woord (hop/cpuinit_riscv64.s). Starten = adoptie — en
		// die is eenmalig: sinds de switcher er bij boot intrekt (parkenter,
		// kern/slots cageInit) is dit pad ná de boot altijd de foutmelding.
		return adoptParked(entry, arg)
	}
	startLittle(entry) // een reset-start kent geen arg
	return nil
}

// LotteryRescued: heeft de boot-core zichzelf gered omdat HopHart nooit
// opkwam (voortgang 2 in het loterij-blok)? De node draait dan als vanouds
// op de firmware-core, maar de rol-boekhouding verwacht de wissel — elke
// plaatsing faalt luid via adoptParked. Deze vraag is er voor de boardWarn.
func LotteryRescued() bool {
	if licheerv.HopHart == 0 {
		return false
	}
	b := uintptr(bootScratchPA)
	dev.Pull(b+licheerv.LotteryProgress, 8)
	return dev.Read64(b+licheerv.LotteryProgress) == 2
}

// adoptParked geeft de geparkeerde boot-core zijn eerste werk: de entry in
// het adoptie-woord, en de parkeerlus springt erheen. Cache-discipline als
// bij de hartprobe: de parkeerlus leest ongecachet, dus HOP pusht.
//
// Eenmalig, en dat bewaakt de tweede check: ná de adoptie is de parkeerlus weg
// en leest niemand het woord meer — een tweede schrijf zou "gestart" melden
// zonder dat er iets start. Een herstart op deze core loopt via de rotatie
// (boot-pending), nooit meer hierlangs.
func adoptParked(entry, arg uint64) error {
	b := uintptr(bootScratchPA) // zelfde page als de hartprobe (init-check daar)
	dev.Pull(b+licheerv.LotteryProgress, 16)
	if dev.Read64(b+licheerv.LotteryProgress) != 1 {
		return fmt.Errorf("licheerv: no parked hart to adopt (lottery progress %d) — was the lottery run?",
			dev.Read64(b+licheerv.LotteryProgress))
	}
	if dev.Read64(b+licheerv.LotteryAdoptPC) != 0 {
		return fmt.Errorf("licheerv: parked hart already adopted — a restart goes through the rotation, not a new adoption")
	}
	// Het adoptie-arg éérst (de parkeerlus geeft het als X11 mee aan de
	// entry): de aanroeper weet wat de entry nodig heeft — kern/slots geeft
	// parkenter zijn sched-blok mee, een reset-vrije probe zou 0 geven.
	dev.Write64(b+licheerv.LotteryParkArg, arg)
	dev.Push(b+licheerv.LotteryParkArg, 8)
	dev.Write64(b+licheerv.LotteryAdoptPC, entry)
	dev.Push(b+licheerv.LotteryAdoptPC, 8)
	return nil
}

// hartOff houdt het hart in reset: kill zonder herstart (het slot is dan leeg
// en zijn locks zijn weg).
func hartOff(hart int) error {
	if hart != hartC906L {
		return fmt.Errorf("licheerv: hart %d cannot be reset — revoke goes through the kill tick (see the lottery, cpuinit_riscv64.s)", hart)
	}
	holdLittle()
	return nil
}

// hartState leest de powertoestand uit het reset-blok. Geen ON_PENDING op dit
// board: de deassert is meteen effectief.
//
// De grote core staat altijd aan: hij is nooit in reset geweest, HopOS zet hem
// er nooit in, en sinds cageInit hem bij boot de switcher laat intrekken
// (parkenter) draait daar vanaf de eerste seconde onze rotatie. De waarheid
// over "draait daar een bewoner" staat in SchedCurrent (kern/slots
// coreRunning), niet hier. (Er heeft hier één dag een PowerOff-tot-adoptie
// gestaan, als smeermiddel voor een koud pad dat inmiddels niet meer bestaat —
// boots 7-11, 17-08.)
func hartState(hart int) board.PowerState {
	if hart == hartC906B {
		return board.PowerOn
	}
	if hart != hartC906L || licheerv.Read32(c906lReset)&resetBit == 0 {
		return board.PowerOff
	}
	return board.PowerOn
}

// startLittle en holdLittle zijn het vendor-recept zelf. Apart van hartOn/hartOff
// omdat de hop ze óók gebruikt, en die gaat niet over een app-slot: hij start de
// C906L om er HOP op te zetten. Eén plek waar de registers staan, twee
// aanroepers met een heel verschillende bedoeling.
func startLittle(entry uint64) {
	licheerv.Write32(c906lReset, licheerv.Read32(c906lReset)&^resetBit)
	licheerv.Write32(secSysCtrl, licheerv.Read32(secSysCtrl)|1<<13)
	licheerv.Write32(secSysVecLo, uint32(entry))
	licheerv.Write32(secSysVecHi, uint32(entry>>32))
	licheerv.Write32(c906lReset, licheerv.Read32(c906lReset)|resetBit)
}

func holdLittle() {
	licheerv.Write32(c906lReset, licheerv.Read32(c906lReset)&^resetBit)
}
