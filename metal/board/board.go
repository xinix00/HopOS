// Package board is de hardware-naad tussen HopOS' generieke kern en een
// concreet board. Generieke packages (slots, hopnet, pcie, applib) praten
// uitsluitend via Current() — ze noemen nooit een concreet board bij naam.
// Een board (board/qemuvirt; straks board/pi5, board/o6n) implementeert Board
// en registreert zich bij het laden met Use(). Zo lekt geen PSCI-conduit,
// GIC-variant, cluster-topologie of adresplan meer in de generieke code: fase P
// (Pi 5 met GICv2/DHCP, O6N via TF-A/SMC) is dan een nieuw board-pakket, geen
// edit-ronde door elke "generieke" package.
package board

import (
	"fmt"
	"net"
	"time"

	"github.com/xinix00/HopOS/metal/net/netdev"

	"github.com/xinix00/HopOS/metal/board/appboard"
	"github.com/xinix00/HopOS/metal/driver/fb"
	"github.com/xinix00/HopOS/metal/driver/pcie"
	"github.com/xinix00/lean/leandhcp"
)

// NetConfig is het IPv4-plan van het interne net van een node (op QEMU de
// slirp-defaults; op echt ijzer straks uit DHCP/DT).
type NetConfig struct {
	IP   string // eigen adres, bv. "10.0.2.15"
	CIDR string // adres/prefix, bv. "10.0.2.15/24"
	GW   string // gateway
	DNS  string // resolver, "host:poort"
}

// NetFromLease zet een DHCP-lease om in het NetConfig dat HOP verwacht.
// Geen resolver in de lease → de gateway als DNS (thuisrouters resolven
// vrijwel altijd zelf); poort 53. Gedeeld door elk DHCP-board (Pi's, uefi)
// zodat de omzetting één plek heeft.
func NetFromLease(l leandhcp.Lease) NetConfig {
	dns := l.DNSString()
	if dns == "0.0.0.0" {
		dns = l.GWString()
	}
	return NetConfig{
		IP:   l.IPString(),
		CIDR: l.CIDR(),
		GW:   l.GWString(),
		DNS:  dns + ":53",
	}
}

// LeaseHolder wordt optioneel geïmplementeerd door boards die hun IP via DHCP
// kregen (de Pi's): hopnet vraagt na de stack-bring-up de lease op en start
// leandhcp.KeepAlive zodat hij niet verloopt. Boards met een statische config
// (qemuvirt) implementeren het niet — dan draait er geen renewal. De bool is
// false als er (nog) geen verkregen lease is.
type LeaseHolder interface {
	DHCPLease() (leandhcp.Lease, bool)
}

// Het ECAM/MMIO-adresplan van een board is pcie.Window: het type woont bij
// zijn gebruiker (metal/driver/pcie, zoals fb.Desc bij fb) zodat driver/
// niets van dit contract hoeft te importeren; het board levert alleen de
// waarden via PCIe().

// Board is het board-contract: alles wat de generieke kern van élk board nodig
// heeft, en het bestaat één keer — er is geen ARM-helft en geen RISC-V-helft
// meer. Wat per architectuur anders ís (de kooi-mechaniek) woont achter de
// kooi-naad in kern/slots (cage.go); wat per BOARD anders is — hoe een core
// start, of hij te resetten is, hoe zijn cores idlen en gewekt worden — zit
// in Cores(), als poten die nil zijn waar het board ze niet heeft.
//
// Alle methodes draaien op de HOP-kern.
type Board interface {
	appboard.Board // CoreID + SetTimerOffset (de app-zichtbare kern)

	CoreClass(core int) string // clusterklasse ("small"/"mid"/"big")

	// MemTotal is de door de firmware gerapporteerde DRAM-grootte in bytes
	// (uit de Device Tree, metal/fw/fdt), of 0 als detectie faalde. HOP krijgt
	// dit naast de core-count, zodat de leader tegen de echte RAM-ceiling
	// plant — de per-job MemoryLimit is de bescherming, HOP overspawnt niet.
	MemTotal() uint64

	// Generieke-timer-offset: wall-ns bij tellerstand nul, gedeeld over alle
	// cores (dus HOP's offset geldt 1-op-1 voor elke app). De setter zit in het
	// ge-embedde appboard.Board.
	TimerOffset() int64
	SetWallTime(ns int64)

	// Netwerk. ProbeNIC construeert én initialiseert de NIC van dit board — de
	// board kent de driver (virtio-net op QEMU, Cadence GEM op de Pi, DWMAC op
	// de LicheeRV) en geeft 'm als go-net-device terug plus zijn MAC (die zit
	// op het concrete driver-type, niet op de NetworkDevice-interface). Zo
	// blijft hopnet driver-agnostisch. Een nil device = geen NIC gevonden; een
	// error = wel gevonden maar de init faalde.
	ProbeNIC() (netdev.Device, net.HardwareAddr, error)
	Net() NetConfig

	// PCIe-adresplan (leeg op boards zonder PCIe).
	PCIe() pcie.Window

	// Framebuffer geeft de firmware-framebuffer voor de log-console
	// (metal/driver/fb), ontdekt via een universeel mechanisme — geen driver:
	// UEFI GOP of de device-tree simple-framebuffer. ok=false als het board er
	// (nog) geen heeft (QEMU -nographic, of een board vóór zijn beeld-fase).
	// Discovery is board-kennis; het renderen erna is gedeeld.
	Framebuffer() (fb.Desc, bool)

	// Privilege meldt of HopOS op het niveau draait dat een kooi kan dragen —
	// EL2 op ARM, machine mode op RISC-V. nil = ja. Anders zegt de fout wat er
	// ontbreekt, en dát is de weigering die de main print vóór hij iets anders
	// doet: de kooi is een invariant, geen optie. RequireEL2/RequireMMode
	// hieronder zijn de twee zinnen die er zijn.
	Privilege() error

	// Firmware is één consoleregel over wie ons bootte en wat die kan:
	// "psci: v1.1 (boot EL2, SMC conduit)", "iBoot, no PSCI (boot EL2)",
	// "boot: M-mode monitor (no SBI), app harts [1]". Diagnose, geen contract.
	Firmware() string

	// Cores is het core-contract van dit board (zie het type).
	Cores() Cores
}

// Cores is het core-contract: één struct, dezelfde poten op elke architectuur,
// en een poot die het silicium niet heeft is nil. De kern stuurt élke core
// hetzelfde aan — starten, stoppen, toestand — en vult per poot in wat het
// board levert. Nummers zijn FYSIEK (wat het board zelf zijn cores noemt: de
// PSCI-index, m1n1's cpu-index, het hart-ID in het reset-blok); de logische
// 1..N van kern/slots komt hier alleen binnen via Phys.
type Cores struct {
	// App geeft de fysieke nummers van de app-cores, in logische volgorde
	// (logische core i = App()[i-1]) — dus zonder HOP's eigen core. Op ARM is
	// dat meestal de PSCI-telling (ProbeCores); op RISC-V en Apple een lijst
	// uit board-kennis.
	App func() []int

	// Start brengt een core die bij de firmware of het SoC stilstaat op entry
	// met arg: PSCI CPU_ON, Apple's spin-release of PMGR, reset-deassert.
	// Een core die HopOS zelf parkeerde start NIET hierlangs (mailbox/SEV,
	// boot-pending) — dat is kooi-werk. Verplicht.
	Start func(core int, entry, arg uint64) error

	// Reset houdt een core in reset: de hard-kill die ook een tight loop
	// stopt en de kooi-registers wist. nil = dit board kan dat niet; stoppen
	// is dan de kooi intrekken, waarna de core zichzelf parkeert (ARM), of
	// de kill-tick van de switcher (een RISC-V-hart zonder reset-recept).
	Reset func(core int) error

	// Resettable zegt per core of Reset erop werkt. nil = op elke core, als
	// Reset er is. De LicheeRV heeft een recept voor één van zijn twee harts.
	Resettable func(core int) bool

	// State is de powertoestand van een core, uit het silicium of de eigen
	// boekhouding. Buiten {On, Off, OnPending} = geen core met dit nummer.
	State func(core int) PowerState

	// IdleMode is hoe de app-cores van dit board idlen (layout.Idle*), door
	// HOP aan de app meegegeven op zijn control-page (layout.CtrlIdleMode).
	// Een app-image is board-loos, dus dit is de enige weg waarlangs een
	// board zijn eigen idle bij een app-core krijgt. nil = 0: wat de
	// architectuur zelf doet (arm64: WFE op de event-stream).
	IdleMode func(core int) uint64

	// Kick wekt een core die slaapt: de tegenhanger van IdleMode. Wat het
	// is, is board-kennis — een fast IPI op Apple, een SGI op een GIC, msip
	// op een CLINT. HOP's wekker roept hem voor elke app-core die met
	// IdleYield slaapt en due is (kern/slots waker.go). nil = dit board
	// heeft het niet nodig: zijn app-cores wekken zichzelf (de event-stream).
	Kick func(core int)
}

// Phys vertaalt een logische app-core (1..N) naar zijn fysieke nummer; ok=false
// als er geen core met dat nummer is.
func (k Cores) Phys(core int) (int, bool) {
	if k.App == nil {
		return 0, false
	}
	app := k.App()
	if core < 1 || core > len(app) {
		return 0, false
	}
	return app[core-1], true
}

// CanReset: heeft dit board een reset voor déze core?
func (k Cores) CanReset(core int) bool {
	if k.Reset == nil {
		return false
	}
	if k.Resettable == nil {
		return true
	}
	return k.Resettable(core)
}

// IdleModeOf: de idle-modus voor een core (0 zonder IdleMode).
func (k Cores) IdleModeOf(core int) uint64 {
	if k.IdleMode == nil {
		return 0
	}
	return k.IdleMode(core)
}

// ProbeCores is de PSCI-vorm van Cores.App: cores 1..max aflopen zolang state
// een echt power-woord geeft, en stoppen bij het eerste antwoord daarbuiten —
// een ontbrekende core (INVALID_PARAMS) óf een PSCI-fout; beide betekenen "hier
// stopt de topologie". Aaneengesloten per definitie: PSCI nummert vanaf nul.
func ProbeCores(state func(core int) PowerState, max int) []int {
	var cores []int
	for c := 1; c <= max; c++ {
		switch state(c) {
		case PowerOn, PowerOff, PowerOnPending:
			cores = append(cores, c)
		default:
			return cores
		}
	}
	return cores
}

// RequireEL2 is de ARM-zin van Privilege: HopOS eist een EL2-boot — de
// stage-2-kooi is een invariant, geen optie.
func RequireEL2(el int) error {
	if el < 2 {
		return fmt.Errorf("booted at EL%d: HopOS requires EL2 (QEMU: virtualization=on)", el)
	}
	return nil
}

// RequireMMode is de RISC-V-zin: alleen machine mode programmeert PMP en
// reset harts, en zonder dat is er geen kooi.
func RequireMMode(mode int) error {
	if mode != 3 {
		return fmt.Errorf("booted in mode %d: HopOS requires M-mode (3) for the PMP cage", mode)
	}
	return nil
}

// HartTimer beschrijft de comparator van één hart (RISC-V). De drie dingen
// staan los van elkaar omdat ze los BEWEZEN worden — op de SG2002 is gemeten
// dat een comparator kan bestaan en vuren terwijl een wfi erop de node stil
// velt (01-08, boots 6/7), dus "tikken mag" en "slapen mag" zijn niet hetzelfde
// antwoord.
type HartTimer struct {
	MtimecmpPA uint64 // PA van mtimecmp; 0 = geen comparator (geen tick, geen slaap)
	MsipPA     uint64 // PA van msip; 0 = HOP kan dit hart niet wekken (core-lokale CLINT)
	SleepCap   uint64 // max slaapduur in tikken; 0 = niet slapen (de parkeerlus spint)
	Tick       uint64 // periode van de kill-tick in tikken; 0 = geen tick
}

// HartTimerer is het OPTIONELE stuk contract van een board met een M-mode-
// switcher (RISC-V): wat machine mode op hart `hart` met diens comparator mag
// doen. De nulwaarde is "niets", en dat hoort de veilige uitslag te zijn zolang
// een board het niet BEWEZEN heeft — op de SG2002 ontbreekt de helft van de
// SiFive-CLINT-layout (mtime is er niet), dus daar hangt dit antwoord aan een
// probe bij boot en niet aan een datasheet. ARM-boards implementeren dit niet.
type HartTimerer interface {
	HartTimer(hart int) HartTimer
}

// NICInterrupter is het OPTIONELE stuk contract van een board waarvan de NIC
// een interruptlijn heeft die HOP's core bereikt. WaitNIC blokkeert de
// aanroeper tot de NIC gemeld heeft dat er iets ligt, of tot max verstreken is
// (true = gemeld). Een board zonder dit contract wordt gepold; hopnet kiest.
// Interrupts zijn uitsluitend HOP-werk: een app-core wordt nooit een target
// en houdt zijn maskers dicht — dat is de isolatie-invariant.
type NICInterrupter interface {
	WaitNIC(max time.Duration) bool
}

// PowerState is de powertoestand van een core/hart. Eén definitie voor beide
// architecturen, want het zijn dezelfde drie begrippen — alleen de BRON verschilt:
// op ARM komt hij uit PSCI AFFINITY_INFO (ARM DEN 0022), op RISC-V uit het
// SoC-reset-blok van het board.
type PowerState int

const (
	PowerOn        PowerState = 0
	PowerOff       PowerState = 1
	PowerOnPending PowerState = 2
)

// String maakt PowerState een fmt.Stringer, zodat %s de leesbare toestand geeft
// (gedeeld door de probes i.p.v. een powstr-kopie per main).
func (s PowerState) String() string {
	switch s {
	case PowerOn:
		return "ON"
	case PowerOff:
		return "OFF"
	case PowerOnPending:
		return "ON_PENDING"
	}
	return fmt.Sprintf("?%d", int(s))
}

// Thermometer is het optionele stukje board-contract voor een die-sensor.
// Optioneel en geen Board-methode: de meeste boards hebben er een, QEMU en
// menig UEFI-doos niet, en "geen sensor" hoort géén stub-implementatie per
// board te kosten. Wie hem heeft, implementeert dit naast Board.
type Thermometer interface {
	// TempMilliC is de CPU/die-temperatuur in milligraden Celsius; 0 = geen
	// (geldige) meting. Eén getal: heeft een board meer sensoren, dan de
	// heetste — dát is het getal waarop je ingrijpt.
	TempMilliC() int
}

// TempMilliC geeft de die-temperatuur van het actieve board, of 0 als het
// board geen sensor (geregistreerd) heeft. Dit is wat de node op zijn
// heartbeat naar HOP meestuurt.
func TempMilliC() int {
	if t, ok := active.(Thermometer); ok {
		return t.TempMilliC()
	}
	return 0
}

// active is het geregistreerde board (nil tot Use — vóór elke board-call).
var active Board

// Use registreert het actieve board. Eenmalig, bij het laden: een board-pakket
// roept dit aan in zijn init(), zodat elke binary die het board importeert
// (verplicht al, voor de tamago runtime-hooks) meteen een geldig Current() heeft.
func Use(b Board) { active = b }

// Current geeft het actieve board.
func Current() Board { return active }
