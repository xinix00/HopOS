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

// PCIeWindow is het ECAM- en MMIO-adresplan van een board (fase 3): omdat wij
// zonder firmware-hulp booten wijst HOP zelf de BAR's toe uit MMIOBase.
type PCIeWindow struct {
	ECAMBase uintptr
	MMIOBase uintptr
	MMIOSize uintptr
}

// Board is één concreet board. Alle methodes draaien op de HOP-kern (core 0),
// behalve CPUOff, dat de aanroepende core zelf uitzet.
type Board interface {
	// Boot & topologie.
	BootEL() int               // 1 = EL1-boot (HVC-conduit), ≥2 = EL2-boot (SMC)
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

	// Hard-kill via de interrupt-controller (de GIC-variant is board-specifiek).
	SGIKill(core uint64)
	SGIClearPending(core uint64)

	// S2TrampPC is het fysieke entrypoint van de EL2-trampoline voor app-cores
	// onder stage-2-isolatie.
	S2TrampPC() uint64

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
}

// active is het geregistreerde board (nil tot Use — vóór elke board-call).
var active Board

// Use registreert het actieve board. Eenmalig, bij het laden: een board-pakket
// roept dit aan in zijn init(), zodat elke binary die het board importeert
// (verplicht al, voor de tamago runtime-hooks) meteen een geldig Current() heeft.
func Use(b Board) { active = b }

// Current geeft het actieve board.
func Current() Board { return active }
