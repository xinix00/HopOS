//go:build riscv64

package board

// Board is één concreet RISC-V64-board. Bovenop Common staat hier wat op deze
// architectuur anders ís dan op ARM — en dat is precies de kooi-mechaniek:
//
//   - Geen exceptielevels maar privilege-modes: HOP draait in machine mode, de
//     app in SUPERVISOR mode (een hart zonder misa.S wordt geweigerd — zie de
//     kooi-stub). De kooi is een PMP-whitelist plus een aparte map-tabel
//     (metal/kern/cage) i.p.v. één stage-2-mapping onder EL2. Er is dus geen
//     S2TrampPC-equivalent: de loader-stub van het slot programmeert de kooi
//     zelf en verifieert hem vóór de sprong naar de app.
//   - Geen PSCI. Een hart start en stopt via het SoC-reset-blok van het board:
//     assert reset, zet de boot-vector, deassert. Dat is kill én slot-start
//     ineen (gemeten op de SG2002, 30-07).
//
// De VOLLEDIGE en LEIDENDE ARM/RISC-V-correspondentie staat in
// docs/technical/isolation.md ("Same idea, different letters"). Wijzigt er iets
// aan privilege-mode, PMP of translatie, dan gaat die tabel eerst — dit
// commentaar en dat van de kooi-naad volgen. Dat is geen procedure om de
// procedure: deze regels hebben een half etmaal het ÓUDE model beschreven (app in
// M-mode, gelockte entries) nadat de code al het omgekeerde deed, en een comment
// dat de isolatie-invariant verkeerd beschrijft is erger dan geen comment.
//
// Alle methodes draaien op de HOP-kern.
type Board interface {
	Common

	// BootMode is de privilege-mode waarin HOP draait: 3 (M-mode) is vereist,
	// want alleen M-mode kan PMP programmeren en harts resetten. Alles daaronder
	// = de mains weigeren te starten, net zoals BootEL()<2 op ARM.
	BootMode() int

	// HartOn plaatst hart `hart` op `entry` en laat hem lopen: reset asserten
	// (stopt hem ook uit een tight loop), boot-vector zetten, deasserten.
	// Opnieuw aanroepen op een lopend hart = kill + verse start; de PMP-locks
	// van het oude slot zijn daarna weg.
	HartOn(hart int, entry uint64) error

	// HartOff houdt hart `hart` in reset (kill zonder herstart).
	HartOff(hart int) error

	// HartState geeft de powertoestand van hart `hart` (uit het reset-blok).
	HartState(hart int) PowerState

	// AppHarts geeft de harts die HOP als app-slot mag gebruiken (dus zonder
	// zijn eigen hart). Op RISC-V is er geen PSCI-telling om op terug te
	// vallen: de topologie is board-kennis, punt.
	AppHarts() []int

	// HartWaker geeft de wekker van een hart: de PA van zijn mtimecmp, de PA van
	// zijn msip, en hoe lang hij hoogstens achter elkaar mag slapen (in
	// timebase-tikken). Alleen dán mag de switcher zijn parkeerlus vervangen door
	// een echte wfi — zonder wekker blijft hij spinnen, want een hart dat niet
	// meer wakker wordt is een dode node.
	//
	// ok=false is dus de veilige uitslag en hoort dat ook te zijn zolang een
	// board het niet BEWEZEN heeft: op de SG2002 ontbreekt de helft van de
	// SiFive-CLINT-layout (mtime is er niet), dus daar hangt dit antwoord aan een
	// probe bij boot en niet aan een datasheet.
	HartWaker(hart int) (mtimecmp, msip, capTicks uint64, ok bool)
}
