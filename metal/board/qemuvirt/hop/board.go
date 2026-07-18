// Package hop is de HOP-bedrading van het qemuvirt-board: de volledige
// board.Board-implementatie mét drivers (virtio-net, DMA-init). Alleen
// HOP-kant-binaries (cmd/) importeren deze helft; app-images importeren
// uitsluitend de basis (board/qemuvirt: runtime-hooks + appboard-contract)
// en linken zo nooit tegen de driverstack — de isolatie op source-niveau uit
// docs/archief/indeling.md, nu ook voor de board-laag.
package hop

import (
	"fmt"
	"net"

	gnet "github.com/usbarmory/go-net"

	"hop-os/metal/abi/layout"
	"hop-os/metal/board"
	"hop-os/metal/board/qemuvirt"
	"hop-os/metal/cpu/el2"
	"hop-os/metal/cpu/psci"
	"hop-os/metal/driver/fb"
	"hop-os/metal/driver/pcie"
	"hop-os/metal/driver/nic/virtionet"
)

// machine is de board-implementatie voor de QEMU -M virt arm64-machine.
type machine struct{}

// init registreert dit board. Elke HOP-binary importeert deze hop-helft
// (cmd/hopos/board_virt.go), dus board.Current() is meteen geldig; de basis
// registreerde het app-contract (appboard) al in háár init.
func init() { board.Use(machine{}) }

// Conformiteit compile-time bewezen: zonder deze regel leunt het Board-
// contract puur op board.Use() at runtime en wordt een gemiste methode pas
// op het bord zichtbaar (Derek, 18-07).
var _ board.Board = machine{}

func (machine) BootEL() int { return int(qemuvirt.BootEL()) }
func (machine) CoreID() int { return qemuvirt.CoreID() }

// MemTotal geeft het bij boot (hwinit1) gedetecteerde DRAM; 0 = niet
// gevonden → de aanroeper valt terug op het statische slot-plan.
func (machine) MemTotal() uint64 { return qemuvirt.MemTotal() }

// CoreClass geeft de clusterklasse van slot i. QEMU -M virt heeft homogene
// cores (alle cortex-a53), dus één klasse voor álle slots — net als de
// (eveneens homogene) Pi-boards, die "big" = de beste/enige klasse melden.
// HOP's plaatsing doet exact-match op klasse: een job die expliciet een andere
// klasse vraagt hoort op dit testboard niet te passen (de tri-cluster is de
// O6N — fase 3/4, een eigen board). Eerder stond hier een verzonnen
// 1-3/4-7/8-11-split die met MaxSlots=3 álle slots "small" maakte → elke
// mid/big-job permanent onplaatsbaar. Board-kennis, geen slot-kennis.
func (machine) CoreClass(i int) string { return "big" }

func (machine) TimerOffset() int64     { return qemuvirt.ARM64.TimerOffset }
func (machine) SetTimerOffset(o int64) { qemuvirt.ARM64.TimerOffset = o }
func (machine) SetWallTime(ns int64)   { qemuvirt.ARM64.SetTime(ns) }

// PSCI via de gedeelde wrappers (metal/cpu/psci); op virt is core N gewoon
// MPIDR-target N — geen vertaling nodig.
func (machine) CPUOn(core, entry, ctx uint64) int64 { return psci.On(core, entry, ctx) }
func (machine) CPUOff() int64                       { return psci.Off() }
func (machine) AffinityInfo(core uint64) board.PowerState {
	return board.PowerState(psci.AffinityInfo(core))
}
func (machine) PSCIVersion() (major, minor uint16) { return psci.Version() }

// De EL2-trampolines (stage-2-kooi + SMP) zijn board-neutraal en wonen in het
// gedeelde metal/el2-pakket; dit board geeft alleen de symbooladressen door.
// De EL1-SMP-stub heeft geen board-methode meer: de app-OS-laag (metal/cpu/smp)
// leest zijn eigen el2.SMPStubPC rechtstreeks.
func (machine) S2TrampPC() uint64    { return el2.S2TrampPC() }
func (machine) S2SMPTrampPC() uint64 { return el2.S2SMPTrampPC() }

// ProbeNIC vindt het virtio-net-mmio-slot, construeert de driver en zet 'm
// klaar in de net-DMA-subregio. Zo blijft de driverkeuze board-kennis en is
// hopnet NIC-agnostisch.
func (machine) ProbeNIC() (gnet.NetworkDevice, net.HardwareAddr, error) {
	base, _ := probeVirtioNet()
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
func (machine) PCIe() pcie.Window {
	return pcie.Window{
		ECAMBase: 0x3f000000,
		MMIOBase: 0x10000000,
	}
}

// Framebuffer: geen. QEMU -M virt draait -nographic (console = UART); er is
// geen firmware-framebuffer. Zou je ooit een beeld willen op virt, dan is dat
// -device ramfb (fw_cfg) — bewust niet gebouwd: dev-target, geen edge-node.
func (machine) Framebuffer() (fb.Desc, bool) { return fb.Desc{}, false }
