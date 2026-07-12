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

	gnet "github.com/usbarmory/go-net"

	"hop-os/metal/dhcp"
	"hop-os/metal/fb"
)

// PowerState is de powertoestand van een core (PSCI AFFINITY_INFO, ARM DEN 0022).
type PowerState int

const (
	PowerOn        PowerState = 0
	PowerOff       PowerState = 1
	PowerOnPending PowerState = 2
)

// String maakt PowerState een fmt.Stringer, zodat %s de leesbare toestand
// geeft (gedeeld door de probes i.p.v. een powstr-kopie per main).
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

// PSCISuccess is de PSCI-return-code voor succes (SMCCC).
const PSCISuccess int64 = 0

// NetConfig is het IPv4-plan van het interne net van een node (op QEMU de
// slirp-defaults; op echt ijzer straks uit DHCP/DT).
type NetConfig struct {
	IP   string // eigen adres, bv. "10.0.2.15"
	CIDR string // adres/prefix, bv. "10.0.2.15/24"
	GW   string // gateway
	DNS  string // resolver, "host:poort"
}

// LeaseHolder wordt optioneel geïmplementeerd door boards die hun IP via DHCP
// kregen (de Pi's): hopnet vraagt na de stack-bring-up de lease op en start
// dhcp.KeepAlive zodat hij niet verloopt. Boards met een statische config
// (qemuvirt) implementeren het niet — dan draait er geen renewal. De bool is
// false als er (nog) geen verkregen lease is.
type LeaseHolder interface {
	DHCPLease() (dhcp.Lease, bool)
}

// PCIeWindow is het ECAM- en MMIO-adresplan van een board (fase 3): omdat wij
// zonder firmware-hulp booten wijst HOP zelf de BAR's toe uit MMIOBase.
type PCIeWindow struct {
	ECAMBase uintptr
	MMIOBase uintptr
}

// Board is één concreet board. Alle methodes draaien op de HOP-kern (core 0),
// behalve CPUOff, dat de aanroepende core zelf uitzet.
type Board interface {
	// Boot & topologie.
	BootEL() int               // ≥2 vereist (stage-2-kooi); 1 = EL1: mains weigeren
	CoreID() int               // eigen core-index (= slotnummer voor app-cores)
	CoreClass(core int) string // clusterklasse ("small"/"mid"/"big")

	// MemTotal is de door de firmware gerapporteerde DRAM-grootte in bytes
	// (uit de Device Tree, metal/fdt), of 0 als detectie faalde. HOP krijgt
	// dit naast de core-count, zodat de leader tegen de echte RAM-ceiling
	// plant — de per-job MemoryLimit is de bescherming, HOP overspawnt niet.
	MemTotal() uint64

	// Generieke-timer-offset: wall-ns bij tellerstand nul, gedeeld over alle
	// cores (dus HOP's offset geldt 1-op-1 voor elke app).
	TimerOffset() int64
	SetTimerOffset(off int64)
	SetWallTime(ns int64)

	// PSCI power-control (return: PSCISuccess of een foutcode).
	CPUOn(core, entry, ctx uint64) int64
	CPUOff() int64
	AffinityInfo(core uint64) PowerState
	PSCIVersion() (major, minor uint16)

	// S2TrampPC is het fysieke entrypoint van de EL2-trampoline voor app-cores
	// onder stage-2-isolatie. De hard-kill vereist géén board-methode meer: die
	// loopt board-neutraal via stage2.Revoke (stage-2-intrekking + HVC/TLBI),
	// niet via de interrupt-controller.
	S2TrampPC() uint64

	// SMP (fase 5): één app over meerdere cores met een gedeelde heap. Een
	// secundaire core komt op via CPU_ON naar S2SMPTrampPC (fysiek, in de
	// HOP-image) en ERET't naar SMPStubPC (de EL1-stub in het app-image, IPA).
	// HOP publiceert S2SMPTrampPC op de control-page; de app leest 'm en gebruikt
	// SMPStubPC (zijn eigen symbool) als ELR-doel. De app blijft oblivious — de
	// OS-laag (goos.Task) brengt de cores op, app-code raakt dit niet aan.
	S2SMPTrampPC() uint64
	SMPStubPC() uint64

	// Netwerk. ProbeNIC construeert én initialiseert de NIC van dit board — de
	// board kent de driver (virtio-net op QEMU, Cadence GEM op de Pi, RTL8126
	// op de O6N) en geeft 'm als go-net-device terug plus zijn MAC (die zit op
	// het concrete driver-type, niet op de NetworkDevice-interface). Zo blijft
	// hopnet driver-agnostisch. Een nil device = geen NIC gevonden; een error =
	// wel gevonden maar de init faalde.
	ProbeNIC() (gnet.NetworkDevice, net.HardwareAddr, error)
	Net() NetConfig

	// PCIe-adresplan.
	PCIe() PCIeWindow

	// Framebuffer geeft de firmware-framebuffer voor de log-console (metal/fb),
	// ontdekt via een universeel mechanisme — geen driver: UEFI GOP of de
	// device-tree simple-framebuffer. ok=false als het board er (nog) geen
	// heeft (QEMU -nographic, of een board vóór zijn beeld-fase). Discovery is
	// board-kennis; het renderen erna is gedeeld.
	Framebuffer() (fb.Desc, bool)
}

// CoreCountHinter is een OPTIONEEL contract naast Board: een board dat zijn
// app-core-aantal kent mag het declareren, zodat slots.NumSlots een
// onbetrouwbare PSCI AFFINITY_INFO kan overbruggen — op sommige silicium meldt
// AFFINITY_INFO INVALID_PARAMS voor bestaande cores, waardoor de core-telling
// stil op 0 (of te laag) uitkomt en HOP nul slots adverteert. ExpectedAppCores
// geeft het aantal app-cores (cores 1..N, dus exclusief HOP's core 0). Boards
// met werkende AFFINITY_INFO (QEMU, Pi) implementeren dit NIET — de PSCI-telling
// blijft dan leidend; slots.NumSlots type-assert hier optioneel op.
type CoreCountHinter interface {
	ExpectedAppCores() int
}

// active is het geregistreerde board (nil tot Use — vóór elke board-call).
var active Board

// Use registreert het actieve board. Eenmalig, bij het laden: een board-pakket
// roept dit aan in zijn init(), zodat elke binary die het board importeert
// (verplicht al, voor de tamago runtime-hooks) meteen een geldig Current() heeft.
func Use(b Board) { active = b }

// Current geeft het actieve board.
func Current() Board { return active }
