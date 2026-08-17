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
// (zie hop.go en kern/slots coreParks).
package hop

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/board"
	"github.com/xinix00/HopOS/metal/board/licheerv"
	"github.com/xinix00/HopOS/metal/dev"
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
	// board/hopcore.go): HOP woont op HopHart, elk ander hart is van de
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

// AppHarts is het globale principe in één lus: álle harts behalve waar HOP
// woont. Geen PSCI-telling om op terug te vallen: de hart-lijst is
// board-kennis, de rolverdeling is er geen — die volgt uit HopHart.
func (machine) AppHarts() []int {
	var apps []int
	for _, h := range []int{hartC906B, hartC906L} {
		if h != licheerv.HopHart {
			apps = append(apps, h)
		}
	}
	return apps
}

// HartResettable: alleen de C906L heeft een reset-recept mét boot-vector (zie de
// kop van dit bestand). Voor de grote core is het antwoord nee, en daar hangt in
// kern/slots de hele levenscyclus aan: starten via een boot-pending die de
// rotatie oppikt, beëindigen via de kill-tick, en HOP blijft uit regel 0 van het
// sched-blok.
//
// Zolang de apps op de C906L wonen stelt dit niets voor; woont HOP daar
// (HopHart = 1), dan is dít de reden dat de app-core anders werkt dan op elk
// ander hart van HopOS: starten via boot-pending, beëindigen via kill-tick.
func (machine) HartResettable(hart int) bool { return hart == hartC906L }

// HartTimer geeft wat machine mode op dít hart met zijn comparator mag doen. De
// drie antwoorden staan los omdat ze los bewezen zijn.
func (machine) HartTimer(hart int) board.HartTimer {
	if !appWaker.ok {
		// De probe is niet rond gekomen, of de CLINT-decode bleek gedeeld. Dan
		// niets: geen tick, geen slaap. kern/slots meldt luid als dat betekent
		// dat een app-core niet meer te beëindigen is.
		return board.HartTimer{}
	}
	switch hart {
	case hartC906B:
		// Als de apps hier wonen (HopHart = C906L): dit hart kent geen reset,
		// dus de kill-tick is het mes. Zijn comparator is bewezen door de CLINT-probe
		// bij boot (board/licheerv/clint.go) — die liep op déze core, dus dat is
		// een meting op het hart zelf en geen extrapolatie.
		//
		// MsipPA blijft 0: HOP zit nu op de andere core en de CLINT is
		// core-lokaal, dus er is geen IPI-kanaal naartoe. Een startschot komt bij
		// de eerstvolgende ronde van de parkeerlus aan.
		//
		// SleepCap blijft 0: tikken mag, slapen niet. Zie appHartSleepEnabled.
		t := board.HartTimer{MtimecmpPA: licheerv.MtimecmpAddr(0), Tick: killTickTicks}
		if appHartSleepEnabled {
			t.SleepCap = licheerv.SleepCapTicks
		}
		return t
	case hartC906L:
		// Als de apps hier wonen (HopHart = C906B, de stand van vandaag): hier is de
		// reset het mes, dus een kill-tick voegt niets toe en blijft uit; de
		// adressen komen uit de probe en niet uit het hart-nummer, want die twee
		// zijn verschillende nummerwerelden (zie hartprobe.go).
		if !appHartSleepEnabled {
			return board.HartTimer{}
		}
		return board.HartTimer{MtimecmpPA: appWaker.mtimecmp, SleepCap: licheerv.SleepCapTicks}
	}
	return board.HartTimer{}
}

// appHartSleepEnabled is de hoofdschakelaar van de SLAAP van een app-hart, en hij
// staat UIT op bewijs (01-08, boots 3-8):
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

// BootMode: HOP draait in M-mode (3). Minder kan niet — alleen M-mode
// programmeert PMP en reset harts, en zonder dat is er geen kooi.
func (machine) BootMode() int { return 3 }

// HartOn zet het hart op entry en laat hem lopen. Op een lópend hart is dit
// kill + verse start: de reset-assert stopt hem waar hij ook is, en wist zijn
// PMP-locks — precies wat een slot-restart nodig heeft.
func (machine) HartOn(hart int, entry uint64) error {
	if hart != hartC906L {
		// Geen reset-recept: dit hart staat sinds de loterij geparkeerd op
		// het adoptie-woord (licheerv/lottery_riscv64.s). Starten = adoptie.
		return adoptParked(entry)
	}
	startLittle(entry)
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
func adoptParked(entry uint64) error {
	b := uintptr(bootScratchPA) // zelfde page als de hartprobe (init-check daar)
	dev.Pull(b+licheerv.LotteryProgress, 24)
	if dev.Read64(b+licheerv.LotteryProgress) != 1 {
		return fmt.Errorf("licheerv: no parked hart to adopt (lottery progress %d, echo %d) — was the lottery run?",
			dev.Read64(b+licheerv.LotteryProgress), dev.Read64(b+licheerv.LotteryHartEcho))
	}
	dev.Write64(b+licheerv.LotteryAdoptPC, entry)
	dev.Push(b+licheerv.LotteryAdoptPC, 8)
	return nil
}

// HartOff houdt het hart in reset: kill zonder herstart (het slot is dan leeg
// en zijn locks zijn weg).
func (machine) HartOff(hart int) error {
	if hart != hartC906L {
		return fmt.Errorf("licheerv: hart %d cannot be reset — revoke goes through the kill tick (see hop.go)", hart)
	}
	holdLittle()
	return nil
}

// HartState leest de powertoestand uit het reset-blok. Geen ON_PENDING op dit
// board: de deassert is meteen effectief.
//
// De grote core staat altijd aan — hij is nooit in reset geweest en HopOS zet
// hem er ook nooit in. Voor hem is dit antwoord dan ook niet de waarheid over
// "draait daar een bewoner"; die staat in SchedCurrent (kern/slots coreRunning,
// via HartResettable).
func (machine) HartState(hart int) board.PowerState {
	if hart == hartC906B {
		return board.PowerOn
	}
	if hart != hartC906L || licheerv.Read32(c906lReset)&resetBit == 0 {
		return board.PowerOff
	}
	return board.PowerOn
}

// startLittle en holdLittle zijn het vendor-recept zelf. Apart van HartOn/HartOff
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
