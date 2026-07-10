package rpi5

// board.go maakt van rpi5 een board.Board en registreert hem bij het laden.
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

// machine is de board-implementatie voor de Raspberry Pi 5 (BCM2712).
type machine struct{}

// init registreert dit board: elke rpi5-binary importeert dit pakket al
// (verplicht, voor de tamago runtime-hooks).
func init() { board.Use(machine{}) }

func (machine) BootEL() int { return int(raspi.BootEL()) }
func (machine) CoreID() int { return CoreID() }

// MemTotal leest de DTB die de firmware in x0 meegaf (cpuinit.s → DTBPtr) en
// telt het /memory-node op. 0 = niet gevonden. DTBPtr is het scratch-woord
// waarin cpuinit x0 legde, dus eerst dereferencen: het woord bevat het
// DTB-adres. LET OP: op het board te verifiëren (levert de Pi-firmware de
// DTB-pointer in x0 aan een raw kernel? zie docs/rpi5.md); de
// VideoCore-mailbox is de tweede bron (P2b).
func (machine) MemTotal() uint64 {
	if n, ok := fdt.MemTotal(uintptr(dev.Read64(DTBPtr))); ok {
		return n
	}
	return 0
}

// CoreClass: de Pi 5 is homogeen (4× Cortex-A76) — per PLAN.md fase P zijn
// alle slots big-class.
func (machine) CoreClass(i int) string { return "big" }

func (machine) TimerOffset() int64     { return raspi.ARM64.TimerOffset }
func (machine) SetTimerOffset(o int64) { raspi.ARM64.TimerOffset = o }
func (machine) SetWallTime(ns int64)   { raspi.ARM64.SetTime(ns) }

// PSCI loopt via de gedeelde raspi-laag (TF-A/armstub op EL3, conduit SMC);
// hier wordt alleen de core-index naar het A76-MPIDR-target vertaald (aff1).
// LET OP (meetpunt probe): de standaard Pi-armstub zet secundaire cores
// mogelijk al "aan" (CPU_ON → ALREADY_ON) — dan vervangen we hem door een
// zelfgebouwde upstream-TF-A bl31.bin (armstub= in config.txt), die cores
// netjes geparkeerd houdt tot CPU_ON. Zie docs/rpi5.md.
func (machine) CPUOn(core, entry, ctx uint64) int64 { return raspi.CPUOn(target(core), entry, ctx) }
func (machine) CPUOff() int64                       { return raspi.CPUOff() }
func (machine) AffinityInfo(core uint64) board.PowerState {
	return board.PowerState(raspi.AffinityInfo(target(core)))
}
func (machine) PSCIVersion() (major, minor uint16) { return raspi.PSCIVersion() }

// Stage-2/SMP: de trampolines zelf zijn board-neutraal (gedeeld metal/el2 —
// geen GIC, geen MPIDR; de hard-kill loopt via stage2.Revoke) en de vectoren
// halen het slot uit VTTBR_EL2.VMID. Wat fase P1 hier nog vergt is geen port
// maar verificatie op het board: het adresplan (layout op Pi-DRAM),
// cache-maintenance in het image-loadpad (A76 is echt, QEMU verhulde dat) en
// VBAR_EL2 → RevokeVecBase in cpuinit. Tot die verificatie: expliciet afwezig.
func (machine) S2TrampPC() uint64    { panic("rpi5: stage-2-kooi is fase P1 (verificatie op board)") }
func (machine) S2SMPTrampPC() uint64 { panic("rpi5: SMP is fase P1 (verificatie op board)") }
func (machine) SMPStubPC() uint64    { panic("rpi5: SMP is fase P1 (verificatie op board)") }

// ProbeNIC: fase P2 — de NIC hangt achter de RP1-southbridge (PCIe, Cadence
// GEM, metal/gem); er is nog geen netwerkpad, dus nog geen device.
func (machine) ProbeNIC() (gnet.NetworkDevice, net.HardwareAddr, error) { return nil, nil, nil }

// Net: fase P2 — komt uit DHCP zodra de GEM-driver er is.
func (machine) Net() board.NetConfig { return board.NetConfig{} }

// PCIe: fase P2 — de RP1 hangt aan de BCM2712-PCIe; het adresplan volgt bij
// de RP1-bring-up.
func (machine) PCIe() board.PCIeWindow { return board.PCIeWindow{} }

// Framebuffer: de firmware-simple-framebuffer uit de DTB (HDMI-log-console
// zonder debug-kabel). Op het board te verifiëren (levert de Pi-firmware een
// /chosen/framebuffer aan een raw kernel? zie docs/rpi5.md).
func (machine) Framebuffer() (fb.Desc, bool) { return raspi.Framebuffer(DTBPtr) }
