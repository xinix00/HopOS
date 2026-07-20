// Package hopnet brengt het netwerk van de HOP-kern op (QEMU virt): onze
// eigen virtio-net-driver (metal/driver/nic/virtionet, bare-metal Go) onder de
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
	"hop-os/metal/net/dhcp"
	"hop-os/metal/net/hopswitch"
)

// Up initialiseert de NIC en de netstack en hangt ze in het net-package. Het
// IP-plan en de NIC-probe komen van het actieve board (op QEMU de slirp-
// defaults; op echt ijzer straks een board met DHCP/DT).
func Up() error {
	// De board levert een kant-en-klaar go-net-device (driver + init zijn
	// board-kennis); hopnet weet niet welke NIC dit is. ProbeNIC vóór Net():
	// op echt ijzer (Pi 5) haalt de probe zelf de DHCP-lease die Net() daarna
	// rapporteert — die volgorde is het contract.
	nic, hw, err := board.Current().ProbeNIC()
	if err != nil {
		return fmt.Errorf("nic: %w", err)
	}
	if nic == nil {
		return fmt.Errorf("no NIC found")
	}
	mac := hw.String()
	nc := board.Current().Net()

	// De NIC achter de NAT-shim van de switch: inbound frames voor
	// gepubliceerde poorten of lopende masquerade-flows worden vóór HOP's
	// stack afgevangen. De CIDR (niet alleen het IP) mee, zodat de NAT weet
	// wat "off-subnet" is (dan is de next-hop de gateway).
	uplink, err := hopswitch.WrapUplink(nic, nc.CIDR, hw)
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
	// HOP's eigen efemere bronpoorten onder het masquerade-bereik houden: de
	// NAT deelt node-poorten uit vanaf hopswitch.MasqBase, dus een inbound
	// antwoord op HOP's eigen poort kan nooit per ongeluk een app-flow matchen.
	if e := gs.Stack.SetPortRange(16000, hopswitch.MasqBase-1); e != nil {
		return fmt.Errorf("poortbereik: %v", e)
	}
	iface.HandleStackErr = func(err error, tx bool) {
		fmt.Printf("netstack (tx=%v): %v\n", tx, err)
	}
	iface.Stack.EnableICMP()

	// De interne gateway-NIC: 10.100.0.1 = "mijn node" voor de apps — de
	// agent/leader zijn dan van binnenuit bereikbaar zonder proxy of NAT
	// (zie internal.go). Een fout is niet fataal: het externe net werkt dan
	// gewoon, alleen de interne route ontbreekt.
	if err := upInternal(gs); err != nil {
		fmt.Printf("net: interne gateway-NIC niet op: %v\n", err)
	}

	// Netstack in Go's standaard net-package hangen: hierna werken
	// net.Listen, net/http en DNS voor alle HOP-kern-code.
	net.SetDefaultNS([]string{nc.DNS})
	net.SocketFunc = iface.Stack.Socket

	// RX-lus: pollen mét microslaap i.p.v. gnet's Gosched-spin, zodat de
	// idle-governor (metal/cpu/idle) de core echt kan laten slapen als het stil
	// is; onder last wordt er nooit geslapen (ring leeg = pas dan slapen).
	go rxLoop(uplink, iface)

	// DHCP-lease levend houden: heeft dit board een verkregen lease (de Pi's),
	// dan vernieuwt KeepAlive hem op T1 via de netstack (UDP-RENEW) — dat kan
	// pas nú, want het leunt op net.SocketFunc hierboven en op rxLoop die de
	// stack voedt. Boards met statische config (qemuvirt) zijn geen LeaseHolder
	// en slaan dit over.
	if lh, ok := board.Current().(board.LeaseHolder); ok {
		if l, has := lh.DHCPLease(); has {
			var m [6]byte
			copy(m[:], hw)
			go dhcp.KeepAlive(m, l)
		}
	}

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
