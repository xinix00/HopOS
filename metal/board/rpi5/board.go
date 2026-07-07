package rpi5

// board.go maakt van rpi5 een board.Board en registreert hem bij het laden.
// Fase-P-status: boot/PSCI/console/timers zijn er; de rest is expliciet
// afwezig tot de bijbehorende fase — een aanroep ervan is een bug, geen
// stille fallback.

import (
	"hop-os/metal/board"
	"hop-os/metal/board/raspi"
	"hop-os/metal/fdt"
)

// machine is de board-implementatie voor de Raspberry Pi 5 (BCM2712).
type machine struct{}

// init registreert dit board: elke rpi5-binary importeert dit pakket al
// (verplicht, voor de tamago runtime-hooks).
func init() { board.Use(machine{}) }

func (machine) BootEL() int { return int(BootEL()) }
func (machine) CoreID() int { return CoreID() }

// MemTotal leest de DTB die de firmware in x0 meegaf (cpuinit.s → DTBPtr) en
// telt het /memory-node op. 0 = niet gevonden. LET OP: op het board te
// verifiëren (levert de Pi-firmware de DTB-pointer in x0 aan een raw kernel?
// zie docs/rpi5.md); de VideoCore-mailbox is de tweede bron (P2b).
func (machine) MemTotal() uint64 {
	if n, ok := fdt.MemTotal(DTBPtr); ok {
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

func (machine) CPUOn(core, entry, ctx uint64) int64 { return CPUOn(core, entry, ctx) }
func (machine) CPUOff() int64                       { return CPUOff() }
func (machine) AffinityInfo(core uint64) board.PowerState {
	return board.PowerState(AffinityInfo(core))
}
func (machine) PSCIVersion() (major, minor uint16) { return PSCIVersion() }

// SGIKill/SGIClearPending: fase P1 — GICv2 (GIC-400) via GICD_SGIR, plus de
// EL2-vectoren/trampoline. Tot die tijd is aanroepen een programmeerfout.
func (machine) SGIKill(core uint64)         { panic("rpi5: hard-kill-SGI is fase P1 (GICv2)") }
func (machine) SGIClearPending(core uint64) { panic("rpi5: SGI-clear is fase P1 (GICv2)") }

// S2TrampPC: fase P1 — de EL2-trampoline (stage-2-kooi) is nog niet geport.
func (machine) S2TrampPC() uint64 { panic("rpi5: EL2-trampoline is fase P1") }

// ProbeNIC: fase P2 — de NIC hangt achter de RP1-southbridge (PCIe, Cadence
// GEM); er is nog geen netwerkpad.
func (machine) ProbeNIC() (base uint64, irq int) { return 0, 0 }

// Net: fase P2 — komt uit DHCP zodra de GEM-driver er is.
func (machine) Net() board.NetConfig { return board.NetConfig{} }

// PCIe: fase P2 — de RP1 hangt aan de BCM2712-PCIe; het adresplan volgt bij
// de RP1-bring-up.
func (machine) PCIe() board.PCIeWindow { return board.PCIeWindow{} }
