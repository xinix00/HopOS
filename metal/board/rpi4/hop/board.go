// Package hop is de HOP-bedrading van het rpi4-board: de volledige
// board.Board-implementatie mét drivers (GENET, DHCP, framebuffer). Alleen
// HOP-kant-binaries (cmd/) importeren deze helft; app-images importeren
// uitsluitend de basis (board/rpi4: runtime-hooks + appboard-contract) en
// linken zo nooit tegen de driverstack.
//
// Het gedeelde Pi-deel (boot-EL, DTB-RAM, timers, PSCI-plumbing, stage-2,
// lease/Net, framebuffer-discovery) woont in board/raspi/hop.Base; hier
// staat alleen het rpi4-eigene: de A72-MPIDR-nummering (aff0), de
// framebuffer-adressen en ProbeNIC.
//
// PSCI-eigenaardigheid van dít board: anders dan op de Pi 5 is TF-A hier geen
// "mogelijk nodig" maar een harde eis — de stock armstub8 heeft helemaal geen
// PSCI (spin-table) en een SMC hangt dan. Zie docs/rpi4.md en
// sd-rpi4/LEESMIJ.txt.
package hop

import (
	"fmt"
	"net"
	"time"

	gnet "github.com/usbarmory/go-net"

	"hop-os/metal/abi/layout"
	"hop-os/metal/board"
	"hop-os/metal/board/raspi"
	raspihop "hop-os/metal/board/raspi/hop"
	"hop-os/metal/board/rpi4"
	"hop-os/metal/driver/nic/genet"
	"hop-os/metal/net/dhcp"
)

// machine is de board-implementatie voor de Raspberry Pi 4 (BCM2711): de
// gedeelde Pi-basis plus de rpi4-naden.
type machine struct{ raspihop.Base }

func base() machine {
	return machine{raspihop.Base{
		CoreIDFn:   rpi4.CoreID,
		Target:     rpi4.Target,
		DTBPtr:     rpi4.DTBPtr,
		VCMailBase: uintptr(rpi4.VCMailBase),
	}}
}

// init registreert dit board: elke HOP-binary voor de Pi 4 importeert deze
// hop-helft (cmd/hopos/board_rpi4.go); de basis registreerde het app-contract
// (appboard) al in háár init.
func init() { board.Use(base()) }

// Conformiteit compile-time bewezen: zonder deze regel leunt het Board-
// contract puur op board.Use() at runtime en wordt een gemiste methode pas
// op het bord zichtbaar (Derek, 18-07).
var _ board.Board = machine{}

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
	raspihop.Lease = l
	return nic, net.HardwareAddr(nic.MAC[:]), nil
}
