package slots

// DE KOOI-NAAD — het contract tussen de arch-neutrale slot-levenscyclus
// (slots.go, share.go, adopt.go) en de mechaniek die per instructieset anders
// is. Dit bestand is de NAAM van dat contract; de twee implementaties staan in
// cage_arm64.go en cage_riscv64.go: dezelfde functies, dezelfde signatures,
// dezelfde beloftes, alleen het silicium eronder verschilt. De struct onderaan
// dwingt af dat beide helften compleet zijn — een ontbrekende of afwijkende
// functie is een buildfout op die architectuur, geen boot-cyclus.
//
// Het contract is de vertaling van de tabel in docs/technical/isolation.md,
// en díe tabel is leidend: verandert er iets aan privilege-niveau, begrenzing
// of vertaling, dan gaat de tabel eerst en volgt dit bestand.
//
//	                       ARM                            RISC-V
//	het niveau van HOP     EL2                            machine mode
//	het niveau van de app  EL1                            supervisor mode
//	wat de app begrenst    stage-2-tabel + VMID           PMP-whitelist
//	wat de app verplaatst  dezelfde stage-2-tabel         Sv39-tabel onder satp
//	wie de kooi zet        HOP, vóór de ERET              de kooi-stub in de partitie
//	hoe een core start     mailbox + SEV; koud: Cores.Start   Cores.Start (reset) of boot-pending
//	hoe HOP een slot stopt stage-2 intrekken → core parkeert   Cores.Reset, of de kill-tick
//
// Wat het board levert staat in board.Cores (metal/board): starten, resetten,
// toestand, idle-modus, wekken. Een poot die het silicium niet heeft is daar nil, en de
// helft hier weet wat dat betekent — op ARM is er geen Reset en parkeert een
// ingetrokken core zichzelf; op RISC-V is een hart zonder reset-recept een
// core waar onze switcher vanaf de boot op draait (coreParks).
//
// Zes groepen:
//
//	init         cageInit, cageSetFlipCapable, cageAdoptable
//	             — vectoren/switcher klaarzetten; de kern-flip-adoptie
//	bound+reloc  cagePrepare, cageLinkBase, cageLinkWindow, cageFloor
//	             — de kooi bouwen en waar een slot zichzelf ziet
//	spin         cageEntryPC, cageSMPEntryPC, cageDispatch, cageColdStart
//	             — waar een core binnenkomt en hoe hij het startschot krijgt
//	state        coreRunning, coreParks, coreStopped
//	             — de waarheid over een core, uit het silicium of de switcher
//	close        cageRevoke, cageForceYield
//	             — de hard-kill en het terugwinnen van een gekaapte core
//	post-mortem  cageFaultRegs, cageWhy, cageIdent
//	             — wat er te vertellen valt als een slot omviel
//
// De logische core-nummering (1..N, aaneengesloten) is van dit pakket; de
// vertaling naar het fysieke nummer van het board loopt via physCore (slots.go)
// en is de enige plek waar beide werelden elkaar raken.

// cageContract is de vorm van de naad. Positioneel geïnitialiseerd, dus élke
// functie moet er staan, met precies deze signature.
type cageContract struct {
	// init
	init           func()
	setFlipCapable func(v bool)
	adoptable      func() bool
	// bound + relocate
	prepare    func(i int, linkBase, base, size, entry uint64) error
	linkBase   func() uint64
	linkWindow func(size uint64) uint64
	// spin
	entryPC    func(i int) uint64
	smpEntryPC func() uint64
	dispatch   func(core int, entry, ctx uint64) error
	coldStart  func(core int, entry, ctx uint64) error
	// state
	running func(core int) bool
	parks   func(core int) bool
	stopped func(core int) bool
	// close
	revoke     func(i int)
	forceYield func(core, hog int)
	// post-mortem
	faultRegs func(i int) string
	why       func(i int) string
	ident     func(i int) string
}

var _ = cageContract{
	cageInit, cageSetFlipCapable, cageAdoptable,
	cagePrepare, cageLinkBase, cageLinkWindow,
	cageEntryPC, cageSMPEntryPC, cageDispatch, cageColdStart,
	coreRunning, coreParks, coreStopped,
	cageRevoke, cageForceYield,
	cageFaultRegs, cageWhy, cageIdent,
}

// cageFloor hoort er ook bij, als constante: hoeveel bytes HOP vóór de app in
// de partitie legt (ARM 0: de trampoline woont in HOP's image; RISC-V: de
// kooi-stub). Een const kan niet in de struct — vandaar deze regel.
var _ uint64 = cageFloor
