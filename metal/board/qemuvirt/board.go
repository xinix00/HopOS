package qemuvirt

// board.go maakt van qemuvirt een board.Board en registreert hem bij het laden.
// Alle board-specifieke waarden die vroeger in generieke packages lekten
// (cluster-topologie in slots, slirp-IP's in hopnet, ECAM/MMIO in pcie) wonen
// nu hier — het ene punt dat per board verandert.

import (
	"fmt"
	"net"

	gnet "github.com/usbarmory/go-net"

	"hop-os/metal/board"
	"hop-os/metal/fb"
	"hop-os/metal/layout"
	"hop-os/metal/virtionet"
)

// machine is de board-implementatie voor de QEMU -M virt arm64-machine.
type machine struct{}

// init registreert dit board. Elke binary importeert qemuvirt al (verplicht,
// voor de tamago runtime-hooks), dus board.Current() is meteen geldig — geen
// expliciete Use()-aanroep in de mains nodig.
func init() { board.Use(machine{}) }

func (machine) BootEL() int { return int(BootEL()) }
func (machine) CoreID() int { return CoreID() }

// MemTotal geeft het bij boot (hwinit1) gedetecteerde DRAM; 0 = niet
// gevonden → de aanroeper valt terug op het statische slot-plan.
func (machine) MemTotal() uint64 { return memTotal }

// CoreClass geeft de clusterklasse van slot i. QEMU -M virt heeft homogene
// cores (alle cortex-a53), dus één klasse voor álle slots — net als de
// (eveneens homogene) Pi-boards, die "big" = de beste/enige klasse melden.
// HOP's plaatsing doet exact-match op klasse: een job die expliciet een andere
// klasse vraagt hoort op dit testboard niet te passen (de tri-cluster is de
// O6N — fase 3/4, een eigen board). Eerder stond hier een verzonnen
// 1-3/4-7/8-11-split die met MaxSlots=3 álle slots "small" maakte → elke
// mid/big-job permanent onplaatsbaar. Board-kennis, geen slot-kennis.
func (machine) CoreClass(i int) string { return "big" }

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

// ProbeNIC vindt het virtio-net-mmio-slot, construeert de driver en zet 'm
// klaar in de net-DMA-subregio. Zo blijft de driverkeuze board-kennis en is
// hopnet NIC-agnostisch.
func (machine) ProbeNIC() (gnet.NetworkDevice, net.HardwareAddr, error) {
	base, _ := ProbeVirtioNet()
	if base == 0 {
		return nil, nil, nil // geen (moderne) virtio-net gevonden
	}
	nic := &virtionet.Net{Base: uintptr(base)}
	if err := nic.Init(layout.NetDMABase, layout.NetDMASize); err != nil {
		return nil, nil, fmt.Errorf("virtio-net init: %w", err)
	}
	return nic, net.HardwareAddr(nic.MAC[:]), nil
}

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
	}
}

// Framebuffer: geen. QEMU -M virt draait -nographic (console = UART); er is
// geen firmware-framebuffer. Zou je ooit een beeld willen op virt, dan is dat
// -device ramfb (fw_cfg) — bewust niet gebouwd: dev-target, geen edge-node.
func (machine) Framebuffer() (fb.Desc, bool) { return fb.Desc{}, false }
