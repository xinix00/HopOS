package rpi4

// board.go maakt van rpi4 een board.Board en registreert hem bij het laden.
// Fase-P-status: boot/PSCI/console/timers zijn er; de rest is expliciet
// afwezig tot de bijbehorende fase — een aanroep ervan is een bug, geen
// stille fallback.

import (
	"hop-os/metal/board"
	"hop-os/metal/board/raspi"
)

// machine is de board-implementatie voor de Raspberry Pi 4 (BCM2711).
type machine struct{}

// init registreert dit board: elke rpi4-binary importeert dit pakket al
// (verplicht, voor de tamago runtime-hooks).
func init() { board.Use(machine{}) }

func (machine) BootEL() int { return int(BootEL()) }
func (machine) CoreID() int { return CoreID() }

// CoreClass: de Pi 4 is homogeen (4× Cortex-A72) — net als op de Pi 5 zijn
// alle slots big-class ("big" = de beste klasse die dít board heeft; een
// A72 is trager dan een A76, maar dat is een node-keuze, geen slot-keuze).
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
func (machine) SGIKill(core uint64)         { panic("rpi4: hard-kill-SGI is fase P1 (GICv2)") }
func (machine) SGIClearPending(core uint64) { panic("rpi4: SGI-clear is fase P1 (GICv2)") }

// S2TrampPC: fase P1 — de EL2-trampoline (stage-2-kooi) is nog niet geport.
func (machine) S2TrampPC() uint64 { panic("rpi4: EL2-trampoline is fase P1") }

// ProbeNIC: fase P2 — de NIC is de geïntegreerde GENET (0xFD580000); er is
// nog geen driver en dus geen netwerkpad.
func (machine) ProbeNIC() (base uint64, irq int) { return 0, 0 }

// Net: fase P2 — komt uit DHCP zodra de GENET-driver er is.
func (machine) Net() board.NetConfig { return board.NetConfig{} }

// PCIe: n.v.t. — de enige PCIe-lane van de BCM2711 zit vast aan de
// VL805-USB-controller; geen NVMe op dit board (CM4 uitgezonderd).
func (machine) PCIe() board.PCIeWindow { return board.PCIeWindow{} }
