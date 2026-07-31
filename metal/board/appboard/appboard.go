// Package appboard is het APP-zichtbare board-contract — het kleine broertje
// van metal/board. Een app-image heeft van zijn board maar twee dingen nodig
// om op te draaien: zijn eigen core-identiteit en de klok-offset die HOP hem
// meegeeft. Al het andere op board.Board (ProbeNIC, PCIe, PSCI-control,
// framebuffer) is HOP-werk; via dít contract kan een app daar niet eens
// tegenaan linken — de isolatie op source-niveau uit docs/archief/indeling.md, nu ook
// voor de board-laag. Bovendien scheelt het elke app-image de complete
// driverstack van zijn board (NIC/PCIe/DHCP, ~2,5k regels die de linker via
// interface-methods nooit kon elimineren).
//
// De basis-helft van elk board (board/qemuvirt, board/rpi4, ...) registreert
// zich hier in zijn init(); de HOP-bedrading (board/<x>/hop) registreert het
// volledige board.Board apart. Een app-binary importeert alleen de basis en
// krijgt zo precies dit contract; een HOP-binary importeert de hop-helft en
// heeft beide. Dit pakket importeert niets.
package appboard

// Board is wat een app van zijn board mag zien.
type Board interface {
	// CoreID geeft de eigen core-index (= slotnummer voor app-cores).
	CoreID() int
	// SetTimerOffset zet de klok-offset (wall-ns bij tellerstand nul) die de
	// app van HOP's control-page overneemt — de teller is gedeeld over alle
	// cores, dus HOP's offset geldt 1-op-1.
	SetTimerOffset(off int64)
}

// active is het geregistreerde board (nil tot Use — vóór elke call).
var active Board

// Use registreert het actieve board. Eenmalig, in het init() van de
// basis-helft van het board-pakket (elke binary importeert die al, voor de
// tamago runtime-hooks).
func Use(b Board) { active = b }

// Current geeft het actieve board.
func Current() Board { return active }

// TimebaseHz is de frequentie van de vrijlopende tijdteller van dit board, of 0
// als het board hem niet aanlevert. Alleen RISC-V heeft dit nodig: daar bestaat
// geen register waaruit de frequentie volgt (waar ARM CNTFRQ_EL0 heeft), dus moet
// iemand het getal zeggen — en dat is board-kennis.
//
// Waarom hier en niet in het generieke app-board zelf: dat pakket hoort op élke
// architectuur hetzelfde te zijn, en een SG2002-frequentie als constante maakte
// het stil board-specifiek. Board-basis-pakketten mogen elkaar niet importeren
// (tools/importcheck), maar dít pakket mogen ze allebei — dus loopt de seam hier
// langs. Zetten in het init() van de board-basis, lezen bij de eerste klok-lees.
var TimebaseHz uint64

// PrintkSink is waar runtime-output van een app heen mag (per byte). Nil —
// de default — is stil: een gekooide core heeft geen UART, dus er is geen
// vanzelfsprekende plek. applib hangt hier bij Init zijn log-ring in, en
// daarmee wordt het éne dat runtime-output nog te melden heeft zichtbaar in
// het task-log: de panic. Zonder deze haak is een panic een exit-code 2
// zonder één regel reden (gemeten 31-07: de apploader stierf vijfmaal
// "exited before staging" en het waaróm — out of memory — bestond nergens).
//
// De aanroeper (runtime/goos.Printk via het board) zit mogelijk mídden in die
// panic: wat hier geregistreerd wordt mag niet alloceren en niet blokkeren.
var PrintkSink func(byte)
