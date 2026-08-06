// Package hop is de HOP-bedrading van het RK3566-board (Radxa Zero 3E): de
// volledige board.Board-implementatie. Alleen HOP-kant-binaries (cmd/)
// importeren deze helft; app-images gebruiken het generieke app-board
// (board/hopslot) en linken zo nooit tegen boardcode — dezelfde bronsplitsing
// als bij de Pi's en de LicheeRV.
//
// Het netwerk (ProbeNIC, Net, DHCPLease) staat in net.go; de SoC-glue eronder
// in board/rk3566 zelf (pinmux.go, grf.go, cru.go). Die splitsing is hier
// zwaarder beladen dan op de andere boards: U-Boot raakt het ethernet niet aan
// ("No ethernet found"), dus is élke laag onder de MAC — pinnen, IO-mux-set,
// GRF-modus, klokgates, klokdeler, PHY-reset — werk van ons.
package hop

import (
	"sync"

	"github.com/xinix00/HopOS/metal/board"
	"github.com/xinix00/HopOS/metal/board/rk3566"
	"github.com/xinix00/HopOS/metal/cpu/el2"
	"github.com/xinix00/HopOS/metal/cpu/psci"
	"github.com/xinix00/HopOS/metal/driver/fb"
	"github.com/xinix00/HopOS/metal/driver/pcie"
)

// machine is de board-implementatie voor de Radxa Zero 3E (RK3566).
type machine struct{}

// init registreert dit board: elke HOP-binary voor dit bord importeert deze
// hop-helft (cmd/hopos/board_rk3566.go), dus board.Current() is meteen geldig.
func init() { board.Use(machine{}) }

// Conformiteit compile-time bewezen: zonder deze regel leunt het Board-contract
// puur op board.Use() at runtime en wordt een gemiste methode pas op het bord
// zichtbaar (Derek, 18-07).
var _ board.Board = machine{}

func (machine) CoreID() int      { return rk3566.CoreID() }
func (machine) BootEL() int      { return rk3566.BootEL() }
func (machine) MemTotal() uint64 { return rk3566.MemTotal() }

// CoreClass: vier identieke Cortex-A55 in één DynamIQ-cluster, dus homogeen.
// "big" betekent hier "de beste klasse die dít board heeft" — een A55 is
// langzamer dan de A76 van een Pi 5, maar dat is een node-keuze en geen
// slot-keuze (zelfde afweging als bij de Pi 4).
func (machine) CoreClass(core int) string { return "big" }

func (machine) TimerOffset() int64       { return rk3566.ARM64.TimerOffset }
func (machine) SetTimerOffset(off int64) { rk3566.ARM64.TimerOffset = off }
func (machine) SetWallTime(ns int64)     { rk3566.ARM64.SetTime(ns) }

// PSCI via de gedeelde wrappers (TF-A bl31 op EL3, conduit SMC — gemeten
// v1.1). De core-index → MPIDR-vertaling is board-kennis: GEMETEN 05-08 dat dit
// silicium in aff1 nummert (targets 0x100/0x200/0x300 werken, aff0-targets
// geven INVALID_PARAMS).
func (machine) CPUOn(core, entry, ctx uint64) int64 {
	return psci.On(rk3566.Target(core), entry, ctx)
}

func (machine) AffinityInfo(core uint64) board.PowerState {
	return board.PowerState(psci.AffinityInfo(rk3566.Target(core)))
}

func (machine) PSCIVersion() (major, minor uint16) { return psci.Version() }

// Stage-2/SMP: de trampolines zijn board-neutraal en data-gedreven (gedeeld
// metal/cpu/el2); het board levert alleen het PA-plan (rk3566/plan.go).
func (machine) S2TrampPC() uint64    { return el2.S2TrampPC() }
func (machine) S2SMPTrampPC() uint64 { return el2.S2SMPTrampPC() }

// PCIe: de RK3566 heeft een PCIe2-controller, maar op dít bordje hangt er niets
// aan (geen M.2, geen RP1-achtige southbridge) — leeg venster.
func (machine) PCIe() pcie.Window { return pcie.Window{} }

// Framebuffer: een RAM-buffer uit het PA-plan, en dat is een BEWUSTE keuze die
// uitleg verdient.
//
// GEMETEN 05-08: de Radxa-U-Boot patcht géén simple-framebuffer in de boot-DTB,
// en hij heeft zelfs helemaal geen video meegecompileerd (`Out:
// serial@fe660000`, geen vidconsole). Er is hier dus niets om op te spiegelen.
//
// Dit board wijkt daarmee af van het principe dat elders in de boom geldt (beeld
// = firmware-buffer, geen driver — daarom geen HVS-driver op de Pi): de RK3566
// hééft een HDMI-uitgang en die hoort te werken (Derek, 05-08). De scanout is dus
// in twee stappen opgezet:
//
//	stap 1: deze buffer in DRAM, zichtbaar over het netwerk (/kvm).
//	stap 2: VOP2 leest DEZELFDE buffer uit en de HDMI-TX zet hem op de
//	        connector — GEMETEN WERKEND 06-08, 1920x1080p60 in DVI-mode.
//
// Eén buffer, twee lezers. Daarom was stap 1 geen omweg maar de helft van het
// werk: het adres en de geometrie zijn precies wat de VOP2-layer als bron
// krijgt.
//
// DE KETEN ZELF (PD_VO → VOP2 → HDMI-TX) IS GUI-WERK en woont in gui/driver/rkscan;
// dit board mag gui niet importeren (indeling.md: alleen cmd importeert gui
// terug), dus cmd/hopos/gui_rk3566.go registreert hem hier via UseScanout. In
// de kale smaak registreert niemand iets en zegt Framebuffer() dus false: geen
// display-code gelinkt, geen fb-logconsole die in een buffer tekent die
// niemand uitleest — "geen dood gewicht" (Derek, 06-08). De 8MB fb-regio in
// het PA-plan blijft in béíde smaken bestaan: één plan is goedkoper dan twee.
// De console van een kale node is de UART plus hopos.console (poort 5555 in de
// meegeleverde headless-config).
//
// De keten die er in de gui-smaak aan hangt:
//
//   - de logconsole spiegelt erop (cmd/hopos/main.go), dus een node zonder
//     debug-kabel heeft alsnog een beeldkanaal;
//   - `FB=1` kent hem via gui/fbgrant exclusief aan de display-app toe, die hem
//     op http://<node>/kvm serveert.
//
// Het beeld verlaat de node dus over het netwerk in plaats van over een kabel.
// Dat is de goedkoopste manier om de GUI te tónen (Derek, 05-08: "zolang we de
// GUI goedkoop kunnen tonen = klaar"), en het schrijven ernaartoe is niet duur:
// fb.Init remapt de regio naar Normal-NC (write-combine), dezelfde
// v1.8.5-behandeling als op de Pi.
//
// Consequentie om te weten: zonder netwerk ziet niemand deze buffer. Dat is geen
// regressie — zonder netwerk was er op dit board helemaal geen beeld.
func (machine) Framebuffer() (fb.Desc, bool) {
	if scanout == nil {
		return fb.Desc{}, false
	}
	scanoutOnce.Do(scanout)
	base, w, h, stride := rk3566.FB()
	// SwapRB blijft UIT: GEMETEN 06-08 met een testpatroon van vier balken —
	// rood, groen, blauw, wit stonden in díe volgorde op het scherm, dus de
	// VOP2 leest XRGB8888 zoals wij het schrijven (R in bits 23-16). Rechte
	// balken bevestigden meteen ook de stride, en de rand de actieve regio.
	return fb.Desc{Base: base, Width: w, Height: h, Stride: stride, BPP: 32}, true
}

// scanout is de beeldketen-opbrenger, gezet door de gui-smaak (UseScanout) en
// nil in de kale. scanoutOnce: Framebuffer() wordt méér dan eens aangeroepen
// (main voor de logconsole, gui/fbgrant bij elke grant en release), en de
// scanout opbrengen is zwaar werk met een PLL erin. Eén keer is genoeg — en
// twee keer zou de PLL onder een lopende scanout verzetten.
var (
	scanout     func()
	scanoutOnce sync.Once
)

// UseScanout registreert de beeldketen — één keer, uit een init vóór het
// eerste Framebuffer()-gebruik (cmd/hopos/gui_rk3566.go).
func UseScanout(f func()) { scanout = f }
