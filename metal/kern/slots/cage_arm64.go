//go:build arm64

package slots

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/board"
	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/kern/stage2"
)

// De ARM64-helft van de kooi-naad (zie cage_riscv64.go voor de andere, en
// cage.go voor het contract). Hier is de kooi een **stage-2-mapping onder
// EL2**: HOP bouwt de tabel, schrijft haar wortel op de control-page, en de
// EL2-trampoline activeert haar en ERET't pas dán naar de app-entry. De app
// draait in EL1 en heeft nooit EL2 gezien.
//
// Dit bestand is een pure verplaatsing van wat in slots.go stond: de zes
// aanknooppunten die niet arch-neutraal zijn, nu achter vier namen. Gedrag
// ongewijzigd — dat is precies wat de ARM-gate bewijst.

// cageInit zet de EL2-vectoren, de parkeerlus en de revoke-vectoren klaar.
// Moet vóór de eerste dispatch gebeuren (de mailbox-cold-detectie leest een
// geveegde mailbox) en is idempotent bij de aanroeper (vectorsOnce).
func cageInit() { stage2.InitVectors() }

// cagePrepare bouwt de kooi van slot i en legt haar beschrijving op de plek
// waar de arch-entry hem leest: op ARM is dat de control-page (CtrlS2Table),
// die de trampoline data-gedreven uitleest. De entry doet hier niets — die
// schrijft armSlot al op de control-page, waar de trampoline hem opzoekt.
//
// cageFloor is nul: HOP zet hier niets vóór de app in de partitie (het stukje
// vertrouwde code is de EL2-trampoline, en die woont in HOP's eigen image).
func cagePrepare(i int, linkBase, base, size, entry uint64) error {
	l1, err := stage2.Build(i, linkBase, base, size)
	if err != nil {
		return fmt.Errorf("stage-2 slot %d: %w", i, err)
	}
	ctrlWrite(i, layout.CtrlS2Table, l1)
	return nil
}

// cageEntryPC is het fysieke adres waar de core binnenkomt: de EL2-trampoline.
// Die leest alles van de control-page (ctx), dus alle dispatch-routes eindigen
// in exact dezelfde boot. Eén adres voor élk slot — de stage-2 legt de partitie
// onder een canoniek IPA, dus de trampoline hoeft niet te weten wélk slot hij
// start; het slot-argument is er voor de kant waar dat níet zo is (RISC-V start
// in de kooi-stub van de partitie zelf).
func cageEntryPC(int) uint64 { return board.Current().S2TrampPC() }

// De parkeer-mailbox is HET ARM-mechanisme voor de core-levenscyclus: HopOS
// bezit zijn cores, dus een gestopte app-core gaat niet terug naar de firmware
// maar parkeert op EL2 in een WFE-lus op zijn mailbox (layout.ParkMboxPA).
//
//	word0 == 0  cold   — nooit geparkeerd; eerste bring-up via PSCI CPU_ON
//	word0 == 1  parked — gestopt, wachtend op dispatch
//	word0 >= 2  running — word0 draagt de ctx (fysieke ctrl-page) die HOP zette
const (
	mboxCold   = 0
	mboxParked = 1
)

func mboxWord0(core int) uint64 { return dev.Read64(layout.ParkMboxPA(core)) }

// coreRunning: de app op deze core draait nog (niet geparkeerd).
func coreRunning(core int) bool { return mboxWord0(core) >= 2 }

// coreParks: op ARM parkeert ELKE app-core in de EL2-lus — PSCI CPU_OFF is op de
// Pi 5-stockfirmware een one-way door (gemeten 2026-07-10), dus HopOS bezit zijn
// cores en zet ze nooit uit. Toch is dit hier vals, want het predicaat vraagt
// niet "parkeert hij?" maar "moet de RISC-V-uitwijk gebruikt worden?": start via
// boot-pending, intrekken via de kill-tick, en HOP blijft uit regel 0 van het
// sched-blok. Op ARM is niets daarvan nodig — de mailbox is device-gemapt (dus
// coherent), de parkeerlus wacht op een SEV, en intrekken is het nullen van de
// stage-2-tabel. Dat pad is over vier boards bewezen en blijft ongemoeid.
func coreParks(int) bool { return false }

// coreStopped: de core staat geparkeerd in zijn WFE-lus.
func coreStopped(core int) bool { return mboxWord0(core) == mboxParked }

// cageDispatch geeft het startschot: {ctx, doel-PC} in de mailbox, en dan óf de
// eenmalige PSCI CPU_ON (cold), óf een SEV die de parkeerlus de trampoline in
// laat springen. word0 = ctx maakt de core meteen "running" voor coreRunning.
func cageDispatch(core int, entry, ctx uint64) error {
	mbox := layout.ParkMboxPA(core)
	cold := mboxWord0(core) == mboxCold
	dev.Write64(mbox+8, entry) // word1 = doel-PC
	dev.Write64(mbox+0, ctx)   // word0 = ctx (= "running")
	dev.MB()
	if cold {
		return cageColdStart(core, entry, ctx)
	}
	dev.SEV() // geparkeerde core: wek hem
	return nil
}

// cageColdStart brengt een koude core op via de firmware (PSCI CPU_ON). Alleen
// de eerste keer per core; daarna leeft hij in HopOS' eigen parkeerlus en gaat
// dispatch via mailbox + cageWake.
func cageColdStart(core int, entry, ctx uint64) error {
	if ret := board.Current().CPUOn(uint64(core), entry, ctx); ret != board.PSCISuccess {
		return fmt.Errorf("PSCI CPU_ON core %d: %d", core, ret)
	}
	return nil
}

// cageSMPEntryPC is het fysieke adres van de EL2 SMP-trampoline: waar een
// secundaire core van een SMP-app binnenkomt (zelfde partitie en stage-2, dus
// gedeelde heap).
func cageSMPEntryPC() uint64 { return board.Current().S2SMPTrampPC() }

// coreExists meldt of core een écht power-woord teruggeeft.
//
// Logisch nummer == fysiek nummer op deze architectuur, en dat is een EIGENSCHAP
// en geen toeval: PSCI nummert cores aaneengesloten vanaf nul, dus de
// aaneengesloten 1..N waarmee kern/slots rekent valt samen met het silicium. Waar
// dat níet zo is (RISC-V levert een lijst hart-ID's) vertaalt de andere helft van
// de naad — zie hartOf in cage_riscv64.go.
//
// Op ARM is dit de PSCI-telling: alles buiten {On,Off,OnPending} is een ontbrekende core
// (INVALID_PARAMS) óf een PSCI-fout — beide betekenen "hier stopt de topologie".
func coreExists(core int) bool {
	switch board.Current().AffinityInfo(uint64(core)) {
	case board.PowerOn, board.PowerOff, board.PowerOnPending:
		return true
	}
	return false
}

// cageRevoke trekt de kooi van slot i in — de hard-kill. Het nult de
// stage-2-map en doet één HVC→TLBI, waarna élke core van het slot (ze delen
// tabel én VMID) op zijn eerstvolgende vertaalde toegang naar de EL2-vectoren
// faultt en dáár parkeert. De cores gaan nóóit terug naar de firmware.
func cageRevoke(i int) { stage2.Revoke(i) }

// cageForceYield wint een core terug van een bewoner die nooit yieldt — op
// ARM chirurgisch: de kooi van de vasthouder intrekken laat hem bij zijn
// eerstvolgende geheugentoegang in EL2 faulten, de switch meldt hem dood en
// de rotatie leeft gewoon door (en pikt de boot-pending bewoner op). De
// medebewoners merken niets.
func cageForceYield(core, hog int) {
	if hog >= 1 && hog <= layout.MaxSlots {
		stage2.Revoke(hog)
		return
	}
	fmt.Printf("HOPOS_CORE_RECLAIM_FAILED: core %d holds no attributable resident\n", core)
}

// cageFloor is nul: HOP zet niets vóór de app in de partitie. Het stukje
// vertrouwde code is de EL2-trampoline, en die woont in HOP's eigen image.
const cageFloor = 0

// cageFaultRegs: de EL2-switch dumpt hier (nog) geen registers bij een fault —
// het rapport is ESR/FAR op de control-page, en dat was op deze architectuur
// altijd al genoeg om te jagen. Zie de RISC-V-helft voor wat er meer kan.
func cageFaultRegs(int) string { return "" }

// cageWhy: hier staat geen stub vóór de app die kan stranden — het stukje
// vertrouwde code is de EL2-trampoline in HOP's eigen image, en een fout daar
// landt in het fault-rapport op de control-page (CtrlFaultVec/ESR/FAR), dat het
// post-mortem al toont. Dus niets toe te voegen.
func cageWhy(i int) string { return "" }

// cageLinkBase: waar een verplaatst slot verschijnt. Op ARM is dat het canonieke
// IPA-venster van slot 1 — de stage-2 legt élke partitie daar, dus draait één
// artifact in elk slot.
func cageLinkBase() uint64 { return uint64(layout.SlotBase(1)) }

// cageLinkWindow: hoe groot het linkvenster van een slot is. Op ARM het canonieke
// IPA-venster per slot — de partitie kan kleiner zijn, maar het venster is de
// adresruimte die de stage-2 voor dit slot beschrijft.
func cageLinkWindow(size uint64) uint64 { return uint64(layout.SlotStride) }

// cageIdent: op ARM is er niets te melden dat HOP niet al weet. De CPU-identiteit
// staat hier niet achter de kooi-naad — HOP leest MIDR/MPIDR van elke core zelf
// via de board-laag, en app-cores zijn dezelfde cores.
func cageIdent(i int) string { return "" }

// cageSetFlipCapable geeft door of deze node zichzelf later mag vervangen. Op
// ARM beslist kern/stage2 daarop of de EL2-blobs naar de plan-regio verhuizen.
func cageSetFlipCapable(v bool) { stage2.SetFlipCapable(v) }

// cageAdoptable: mag de slot-laag de bewoners van de vórige kern overnemen
// (kern-flip, docs/kern-flip.md)? Op ARM houdt kern/stage2 dat antwoord vast:
// hij kreeg de adoptie-stand van kernflip en trekt hem in als de switch-code
// die in de plan-regio staat niet byte-voor-byte de zijne blijkt — dan heeft
// hij die regio vers neergezet en bestaan de bewoners niet meer.
func cageAdoptable() bool { return stage2.Adopting() }
