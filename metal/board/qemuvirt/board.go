package qemuvirt

// board.go maakt van qemuvirt een board.Board en registreert hem bij het laden.
// Alle board-specifieke waarden die vroeger in generieke packages lekten
// (cluster-topologie in slots, slirp-IP's in hopnet, ECAM/MMIO in pcie) wonen
// nu hier — het ene punt dat per board verandert.

import "hop-os/metal/board"

// machine is de board-implementatie voor de QEMU -M virt arm64-machine.
type machine struct{}

// init registreert dit board. Elke binary importeert qemuvirt al (verplicht,
// voor de tamago runtime-hooks), dus board.Current() is meteen geldig — geen
// expliciete Use()-aanroep in de mains nodig.
func init() { board.Use(machine{}) }

func (machine) BootEL() int { return int(BootEL()) }
func (machine) CoreID() int { return CoreID() }

// CoreClass geeft de cluster-klasse van core i op de O6N-indeling (1-3 small,
// 4-7 mid, 8-11 big). In QEMU zijn de cores homogeen; dit is de beoogde mapping
// die op echt ijzer via MPIDR/DT wordt bevestigd. Board-kennis, geen slot-kennis.
func (machine) CoreClass(i int) string {
	switch {
	case i >= 8:
		return "big"
	case i >= 4:
		return "mid"
	default:
		return "small"
	}
}

func (machine) TimerOffset() int64     { return ARM64.TimerOffset }
func (machine) SetTimerOffset(o int64) { ARM64.TimerOffset = o }
func (machine) SetWallTime(ns int64)   { ARM64.SetTime(ns) }

func (machine) CPUOn(core, entry, ctx uint64) int64 { return CPUOn(core, entry, ctx) }
func (machine) CPUOff() int64                       { return CPUOff() }
func (machine) AffinityInfo(core uint64) board.PowerState {
	return board.PowerState(AffinityInfo(core))
}
func (machine) PSCIVersion() (major, minor uint16) { return PSCIVersion() }

func (machine) SGIKill(core uint64)         { SGIKill(core) }
func (machine) SGIClearPending(core uint64) { SGIClearPending(core) }
func (machine) S2TrampPC() uint64           { return S2TrampPC() }

func (machine) ProbeNIC() (base uint64, irq int) { return ProbeVirtioNet() }

// Net geeft het interne net-plan: de QEMU slirp-defaults (-netdev user). Op
// echt ijzer komt dit straks uit DHCP/DT — dan een ander board.
func (machine) Net() board.NetConfig {
	return board.NetConfig{
		IP:   "10.0.2.15",
		CIDR: "10.0.2.15/24",
		GW:   "10.0.2.2",
		DNS:  "10.0.2.3:53",
	}
}

// PCIe geeft het adresplan van QEMU -M virt (hw/arm/virt.c, highmem-ecam=off):
// ECAM en het 32-bit MMIO-venster waaruit HOP zelf de BAR's toewijst.
func (machine) PCIe() board.PCIeWindow {
	return board.PCIeWindow{
		ECAMBase: 0x3f000000,
		MMIOBase: 0x10000000,
		MMIOSize: 0x2eff0000,
	}
}
