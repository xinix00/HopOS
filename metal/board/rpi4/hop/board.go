// Package hop is de HOP-bedrading van het rpi4-board: de volledige
// board.Board-implementatie mét drivers (GENET, DHCP, framebuffer). Alleen
// HOP-kant-binaries (cmd/) importeren deze helft; app-images importeren
// uitsluitend de basis (board/rpi4: runtime-hooks + appboard-contract) en
// linken zo nooit tegen de driverstack.
//
// Fase-P-status: boot/PSCI/console/timers zijn er; de rest is expliciet
// afwezig tot de bijbehorende fase — een aanroep ervan is een bug, geen
// stille fallback.
package hop

import (
	"fmt"
	"net"
	"time"

	gnet "github.com/usbarmory/go-net"

	"hop-os/metal/abi/layout"
	"hop-os/metal/board"
	"hop-os/metal/board/raspi"
	"hop-os/metal/board/raspi/vcfb"
	"hop-os/metal/board/rpi4"
	"hop-os/metal/cpu/el2"
	"hop-os/metal/driver/fb"
	"hop-os/metal/driver/nic/genet"
	"hop-os/metal/fw/fdt"
	"hop-os/metal/net/dhcp"
)

// machine is de board-implementatie voor de Raspberry Pi 4 (BCM2711).
type machine struct{}

// init registreert dit board: elke HOP-binary voor de Pi 4 importeert deze
// hop-helft (cmd/hopos/board_rpi4.go); de basis registreerde het app-contract
// (appboard) al in háár init.
func init() { board.Use(machine{}) }

func (machine) BootEL() int { return int(raspi.BootEL()) }
func (machine) CoreID() int { return rpi4.CoreID() }

// MemTotal leest de DTB (cpuinit.s → DTBPtr) en telt het /memory-node op.
// 0 = niet gevonden. DTBPtr is het scratch-woord waarin cpuinit x0 legde, dus
// eerst dereferencen: het woord bevat het DTB-adres. Op het board te
// verifiëren (zie docs/rpi4.md).
func (machine) MemTotal() uint64 {
	if n, ok := fdt.MemTotal(raspi.DTB()); ok {
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
// naar het A72-MPIDR-target vertaald (aff0, rpi4.Target). LET OP: anders dan
// op de Pi 5 is TF-A hier geen "mogelijk nodig" maar een harde eis — de stock
// armstub8 heeft helemaal geen PSCI (spin-table) en een SMC hangt dan. Zie
// docs/rpi4.md en sd-rpi4/LEESMIJ.txt.
func (machine) CPUOn(core, entry, ctx uint64) int64 {
	return raspi.CPUOn(rpi4.Target(core), entry, ctx)
}
func (machine) CPUOff() int64 { return raspi.CPUOff() }
func (machine) AffinityInfo(core uint64) board.PowerState {
	return board.PowerState(raspi.AffinityInfo(rpi4.Target(core)))
}
func (machine) PSCIVersion() (major, minor uint16) { return raspi.PSCIVersion() }

// Stage-2/SMP: de trampolines zijn board-neutraal en data-gedreven (gedeeld
// metal/cpu/el2 — geen GIC, geen MPIDR, geen ingebakken adressen; de hard-kill
// loopt via stage2.Revoke, cores parkeren op EL2). Dit board levert het
// PA-plan (rpi4.go) en de faultdump2-tabel op 0x8B000 als RevokeVecPA; de rest
// is hier één-op-één doorgeven.
func (machine) S2TrampPC() uint64    { return el2.S2TrampPC() }
func (machine) S2SMPTrampPC() uint64 { return el2.S2SMPTrampPC() }

// lease bewaart wat ProbeNIC via DHCP ophaalde; Net() leest hem. hopnet.Up
// roept ProbeNIC vóór Net() aan (die volgorde is het contract).
var lease dhcp.Lease

// ProbeNIC brengt de GENET-keten op — boardvast bewezen op echte hardware
// (2026-07-11, één boot): v5-rev → MAC-reset (U-Boot-sequence) → PHY-scan
// (BCM54213PE op adres 1, géén reset-GPIO hier) → autonegotiatie →
// ring-16-DMA in de plan-regio → DHCP-lease. Geen PCIe zoals de Pi 5: de
// GENET is direct memory-mapped en de firmware laat hem gewoon met rust.
func (machine) ProbeNIC() (gnet.NetworkDevice, net.HardwareAddr, error) {
	nic := &genet.Net{
		Base: uintptr(rpi4.GENETBase),
		MAC:  raspi.MACFromSerial(raspi.DTB(), 0x04),
	}
	if rev := nic.Rev() >> 24 & 0xF; rev != 6 {
		return nil, nil, fmt.Errorf("genet: rev-nibble %d (verwacht 6 = v5)", rev)
	}
	nic.Reset()
	addr, _, _, found := nic.PHYScan()
	if !found {
		return nil, nil, fmt.Errorf("genet: geen PHY op de MDIO-bus")
	}
	speed, fd, err := nic.AutoNeg(addr, 8*time.Second)
	if err != nil {
		return nil, nil, err
	}
	if err := nic.Init(layout.NetDMAPA(), layout.NetDMASize, speed, fd); err != nil {
		return nil, nil, err
	}

	l, err := dhcp.Acquire(nic, nic.MAC, 15*time.Second)
	if err != nil {
		return nil, nil, err
	}
	lease = l
	return nic, net.HardwareAddr(nic.MAC[:]), nil
}

// Net geeft de DHCP-lease die ProbeNIC haalde, omgezet naar board.NetConfig
// via de gedeelde omzetting in metal/board (identiek aan de Pi 5).
func (machine) Net() board.NetConfig { return board.NetFromLease(lease) }

// DHCPLease geeft de door ProbeNIC verkregen lease (board.LeaseHolder), zodat
// hopnet er na de stack-bring-up dhcp.KeepAlive op start. false vóór een echte
// ACK (dan is er niets te vernieuwen).
func (machine) DHCPLease() (dhcp.Lease, bool) { return lease, lease.Acquired }

// PCIe: n.v.t. — de enige PCIe-lane van de BCM2711 zit vast aan de
// VL805-USB-controller; geen NVMe op dit board (CM4 uitgezonderd).
func (machine) PCIe() board.PCIeWindow { return board.PCIeWindow{} }

// Framebuffer: DTB-simplefb met mailbox-terugval — de gedeelde Pi-discovery
// (zie board/raspi/vcfb; gemeten 2026-07-11: zonder de terugval bleef de
// Pi 4 op het regenboog-splashscherm hangen).
func (machine) Framebuffer() (fb.Desc, bool) {
	return vcfb.FramebufferVC(rpi4.DTBPtr, uintptr(rpi4.VCMailBase))
}

// EnableTimestamps zet de per-regel-console-stempel aan (optionele interface,
// door cmd/hopos ná de boot-banner aangeroepen). Zie board/raspi/console_ts.go.
func (machine) EnableTimestamps() { raspi.LogTimestamps(true) }
