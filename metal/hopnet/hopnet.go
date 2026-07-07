// Package hopnet brengt het netwerk van de HOP-kern op (QEMU virt): onze
// eigen virtio-net-driver (metal/virtionet, bare-metal Go) onder de
// gVisor-netstack (go-net), gehookt in Go's standaard net-package. Daarna
// werken net.Listen, net.Dial en net/http gewoon — precies wat de HOP-agent
// nodig heeft. Dit is "geen video, wel een poort".
package hopnet

import (
	"fmt"
	"net"
	"time"

	gnet "github.com/usbarmory/go-net"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"hop-os/metal/board"
	"hop-os/metal/hopswitch"
	"hop-os/metal/layout"
	"hop-os/metal/virtionet"
)

// Up initialiseert de NIC en de netstack en hangt ze in het net-package. Het
// IP-plan en de NIC-probe komen van het actieve board (op QEMU de slirp-
// defaults; op echt ijzer straks een board met DHCP/DT).
func Up() error {
	nc := board.Current().Net()
	base, _ := board.Current().ProbeNIC()
	if base == 0 {
		return fmt.Errorf("geen (moderne) virtio-net gevonden")
	}

	nic := &virtionet.Net{Base: uintptr(base)}
	if err := nic.Init(layout.NetDMABase, layout.NetDMASize); err != nil {
		return fmt.Errorf("virtio-net init: %w", err)
	}
	mac := net.HardwareAddr(nic.MAC[:]).String()

	// De NIC achter de NAT-shim van de switch: frames voor gepubliceerde
	// task-poorten worden vóór HOP's stack afgevangen en doorgerouterd.
	uplink, err := hopswitch.WrapUplink(nic, nc.IP, net.HardwareAddr(nic.MAC[:]))
	if err != nil {
		return err
	}

	// HandleLocal: verbindingen naar het eigen IP worden intern afgeleverd
	// (agent ↔ leader op dezelfde node); zonder dit verdwijnen die frames
	// richting slirp, die niet hairpint.
	opts := gnet.DefaultStackOptions
	opts.HandleLocal = true
	gs := gnet.NewGVisorStack(1)
	gs.Stack = stack.New(opts)

	iface := &gnet.Interface{
		NetworkDevice: uplink,
		Stack:         gs,
	}
	if err := iface.Init(nc.CIDR, mac, nc.GW); err != nil {
		return fmt.Errorf("netstack init: %w", err)
	}
	iface.HandleStackErr = func(err error, tx bool) {
		fmt.Printf("netstack (tx=%v): %v\n", tx, err)
	}
	iface.Stack.EnableICMP()

	// Netstack in Go's standaard net-package hangen: hierna werken
	// net.Listen, net/http en DNS voor alle HOP-kern-code.
	net.SetDefaultNS([]string{nc.DNS})
	net.SocketFunc = iface.Stack.Socket

	// RX-lus: pollen mét microslaap i.p.v. gnet's Gosched-spin, zodat de
	// idle-governor (metal/idle) de core echt kan laten slapen als het stil
	// is; onder last wordt er nooit geslapen (ring leeg = pas dan slapen).
	go rxLoop(uplink, iface)

	fmt.Printf("net: %s (mac %s, gw %s) — HOPOS_NET_UP\n", nc.IP, mac, nc.GW)
	return nil
}

// rxLoop is de de-spun variant van gnet's Interface.Start.
func rxLoop(nic gnet.NetworkDevice, iface *gnet.Interface) {
	buf := make([]byte, gnet.MTU+gnet.EthernetMaximumSize)
	for {
		n, err := nic.Receive(buf)
		if n == 0 || err != nil {
			time.Sleep(300 * time.Microsecond)
			continue
		}
		if err := iface.Stack.RecvInboundPacket(buf[:n]); err != nil && iface.HandleStackErr != nil {
			iface.HandleStackErr(err, false)
		}
	}
}
