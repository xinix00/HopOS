package rpi4

// board.go maakt van rpi4 een board.Board en registreert hem bij het laden.
// Fase-P-status: boot/PSCI/console/timers zijn er; de rest is expliciet
// afwezig tot de bijbehorende fase — een aanroep ervan is een bug, geen
// stille fallback.

import (
	"net"

	gnet "github.com/usbarmory/go-net"

	"hop-os/metal/board"
	"hop-os/metal/board/raspi"
	"hop-os/metal/dev"
	"hop-os/metal/fb"
	"hop-os/metal/fdt"
)

// machine is de board-implementatie voor de Raspberry Pi 4 (BCM2711).
type machine struct{}

// init registreert dit board: elke rpi4-binary importeert dit pakket al
// (verplicht, voor de tamago runtime-hooks).
func init() { board.Use(machine{}) }

func (machine) BootEL() int { return int(raspi.BootEL()) }
func (machine) CoreID() int { return CoreID() }

// MemTotal leest de DTB (cpuinit.s → DTBPtr) en telt het /memory-node op.
// 0 = niet gevonden. DTBPtr is het scratch-woord waarin cpuinit x0 legde, dus
// eerst dereferencen: het woord bevat het DTB-adres. Op het board te
// verifiëren (zie docs/rpi4.md).
func (machine) MemTotal() uint64 {
	if n, ok := fdt.MemTotal(uintptr(dev.Read64(DTBPtr))); ok {
		return n
	}
	return 0
}

// CoreClass: de Pi 4 is homogeen (4× Cortex-A72) — net als op de Pi 5 zijn
// alle slots big-class ("big" = de beste klasse die dít board heeft; een
// A72 is trager dan een A76, maar dat is een node-keuze, geen slot-keuze).
func (machine) CoreClass(i int) string { return "big" }

func (machine) TimerOffset() int64     { return raspi.ARM64.TimerOffset }
func (machine) SetTimerOffset(o int64) { raspi.ARM64.TimerOffset = o }
func (machine) SetWallTime(ns int64)   { raspi.ARM64.SetTime(ns) }

// PSCI loopt via de gedeelde raspi-laag; hier wordt alleen de core-index
// naar het A72-MPIDR-target vertaald (aff0). LET OP: anders dan op de Pi 5
// is TF-A hier geen "mogelijk nodig" maar een harde eis — de stock armstub8
// heeft helemaal geen PSCI (spin-table) en een SMC hangt dan. Zie
// docs/rpi4.md en sd-rpi4/LEESMIJ.txt.
func (machine) CPUOn(core, entry, ctx uint64) int64 { return raspi.CPUOn(target(core), entry, ctx) }
func (machine) CPUOff() int64                       { return raspi.CPUOff() }
func (machine) AffinityInfo(core uint64) board.PowerState {
	return board.PowerState(raspi.AffinityInfo(target(core)))
}
func (machine) PSCIVersion() (major, minor uint16) { return raspi.PSCIVersion() }

// Stage-2/SMP: de trampolines zijn board-neutraal (gedeeld metal/el2 — geen
// GIC, geen MPIDR; slot uit VTTBR_EL2.VMID). Fase P1 = verificatie op het
// board (adresplan, cache-maintenance in het loadpad, VBAR_EL2 in cpuinit),
// geen port. Tot die verificatie: expliciet afwezig.
func (machine) S2TrampPC() uint64    { panic("rpi4: stage-2-kooi is fase P1 (verificatie op board)") }
func (machine) S2SMPTrampPC() uint64 { panic("rpi4: SMP is fase P1 (verificatie op board)") }
func (machine) SMPStubPC() uint64    { panic("rpi4: SMP is fase P1 (verificatie op board)") }

// ProbeNIC: fase P2 — de NIC is de geïntegreerde GENET (0xFD580000); er is
// nog geen driver en dus geen netwerkpad, dus nog geen device.
func (machine) ProbeNIC() (gnet.NetworkDevice, net.HardwareAddr, error) { return nil, nil, nil }

// Net: fase P2 — komt uit DHCP zodra de GENET-driver er is.
func (machine) Net() board.NetConfig { return board.NetConfig{} }

// PCIe: n.v.t. — de enige PCIe-lane van de BCM2711 zit vast aan de
// VL805-USB-controller; geen NVMe op dit board (CM4 uitgezonderd).
func (machine) PCIe() board.PCIeWindow { return board.PCIeWindow{} }

// Framebuffer: de firmware-simple-framebuffer uit de DTB (HDMI-log-console
// zonder debug-kabel). Op het board te verifiëren (zie docs/rpi4.md).
func (machine) Framebuffer() (fb.Desc, bool) { return raspi.Framebuffer(DTBPtr) }
