// Package hopnet brengt het netwerk van de HOP-kern op: de NIC-driver van
// het board onder de lneto-netstack (go-net's LnetoStack), gehookt in Go's
// standaard net-package. Daarna werken net.Listen, net.Dial en net/http
// gewoon — precies wat de HOP-agent nodig heeft. Dit is "geen video, wel
// een poort".
//
// Tot 09-08 stond hier gVisor (~90k gelinkte regels, PacketBuffer-machinerie
// en een goroutine-handoff per segment); de flip naar lneto is gemeten op de
// netmeter-bank: RX 26→61MB/s, 66 mallocs i.p.v. 340k voor 64MiB, 0 GC-druk
// op het pakket-pad. Wat gVisor aan features droeg is verhuisd: HandleLocal
// en de tweede interne NIC zitten nu op de device-naad (locnet.go +
// internal.go), window scaling zit in lneto zelf (RFC 7323, 09-08).
package hopnet

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"

	gnet "github.com/xinix00/go-net"

	"github.com/xinix00/HopOS/metal/board"
	"github.com/xinix00/HopOS/metal/net/hopswitch"
	"github.com/xinix00/lean/leandhcp"
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

	// Het lokale-verkeer-schilletje om de uplink (locnet.go): self-dial op
	// het eigen IP (agent ↔ leader op dezelfde node — gvisor's HandleLocal,
	// nu op de device-naad), ARP naar het eigen adres, en de 1:1-vertaling
	// van het interne subnet. Het IP als getal is de sleutel van die naad.
	ip4 := net.ParseIP(nc.IP).To4()
	if ip4 == nil {
		return fmt.Errorf("node IP %q is not IPv4", nc.IP)
	}
	loc := &locdev{nic: uplink, ip: binary.BigEndian.Uint32(ip4)}
	copy(loc.mac[:], hw)

	// De stack-maten. De buffers bepalen via window scaling (RFC 7323) het
	// TCP-venster: 128KiB = shift 2 — de downloads (het kern-geldpad) halen
	// daarmee ~venster/RTT zonder dat de listener-pools de kern leegeten
	// (élke net.Listen alloceert MaxListenerConns×2×buffer vooraf).
	cfg := gnet.DefaultLnetoStackConfig()
	cfg.Hostname = "hopos"
	cfg.TCPBufferSize = 128 << 10
	cfg.TCPQueueSize = 32
	cfg.MaxActiveTCPPorts = 64 // poorttabel: downloads (4) + API + S3 + marge
	cfg.MaxListenerConns = 8   // per listener (agent/leader/console)

	iface := &gnet.Interface{
		NetworkDevice: loc,
		Stack:         gnet.NewLnetoStack(cfg),
	}
	if err := iface.Init(nc.CIDR, mac, nc.GW); err != nil {
		return fmt.Errorf("netstack init: %w", err)
	}
	// Geen SetPortRange meer (dat was gvisor): lneto deelt zijn efemere
	// poorten sequentieel uit in [49152, 65535], en de masquerade stopt op
	// hopswitch.MasqEnd = 49152 — de bereiken zijn per constructie disjunct.
	iface.HandleStackErr = func(err error, tx bool) {
		fmt.Printf("netstack (tx=%v): %v\n", tx, err)
	}
	iface.Stack.EnableICMP()

	// De interne gateway-naad: 10.100.0.1 = "mijn node" voor de apps — de
	// agent/leader zijn dan van binnenuit bereikbaar zonder proxy (zie
	// internal.go).
	upInternal(loc)

	// Netstack in Go's standaard net-package hangen: hierna werken
	// net.Listen, net/http en DNS voor alle HOP-kern-code.
	net.SetDefaultNS([]string{nc.DNS})
	net.SocketFunc = iface.Stack.Socket

	// RX-lus: pollen mét microslaap i.p.v. gnet's Gosched-spin, zodat de
	// idle-governor (metal/cpu/idle) de core echt kan laten slapen als het stil
	// is; onder last wordt er nooit geslapen (ring leeg = pas dan slapen).
	// Over de locdev, zodat loopback- en gateway-frames vóór de NIC gaan.
	go rxLoop(loc, iface)

	// DHCP-lease levend houden: heeft dit board een verkregen lease (de Pi's),
	// dan vernieuwt KeepAlive hem op T1 via de netstack (UDP-RENEW) — dat kan
	// pas nú, want het leunt op net.SocketFunc hierboven en op rxLoop die de
	// stack voedt. Boards met statische config (qemuvirt) zijn geen LeaseHolder
	// en slaan dit over.
	if lh, ok := board.Current().(board.LeaseHolder); ok {
		if l, has := lh.DHCPLease(); has {
			var m [6]byte
			copy(m[:], hw)
			// Eigen recover: KeepAlive verwerkt DHCP-antwoorden van het LAN
			// (onvertrouwde inhoud) op een eigen goroutine — een panic daar zou
			// de hele node vellen. Zonder lease-vernieuwing blijft de node
			// werken tot de lease verloopt; dat is oneindig veel beter dan dood.
			go func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Printf("HOPOS_DHCP_PANIC: %v — lease renewal stopped, node keeps running\n", r)
					}
				}()
				leandhcp.KeepAlive(m, l)
			}()
		}
	}

	fmt.Printf("net: %s (mac %s, gw %s) — HOPOS_NET_UP\n", nc.IP, mac, nc.GW)
	return nil
}

// rxLoop is de de-spun variant van gnet's Interface.Start.
func rxLoop(nic gnet.NetworkDevice, iface *gnet.Interface) {
	buf := make([]byte, gnet.MTU+gnet.EthernetMaximumSize)
	for {
		if !rxPass(nic, iface, buf) {
			time.Sleep(300 * time.Microsecond)
		}
	}
}

// rxPass doet één ontvangst-ronde, mét recover — het spiegelbeeld van
// hopswitch.switchPass. Dit is het meest blootgestelde pad van de node: ruwe
// frames van de fysieke NIC, remote en zonder authenticatie, en ze lopen via
// Uplink.Receive tot in de NAT (arpLearn, DNAT, checksum-herschrijving) en de
// gvisor-stack. Een panic op frame-inhoud zou hier de RX-goroutine velen, en dat
// is de hele node — álle slots. Frame gedropt, lus draait door; na een panic
// slaapt de lus zijn normale tikje, zodat een panic-storm de core niet opeet.
func rxPass(nic gnet.NetworkDevice, iface *gnet.Interface, buf []byte) (worked bool) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("HOPOS_RX_PANIC: %v — frame dropped, RX keeps running\n", r)
		}
	}()
	n, err := nic.Receive(buf)
	if n == 0 || err != nil {
		return false
	}
	if err := iface.Stack.RecvInboundPacket(buf[:n]); err != nil && iface.HandleStackErr != nil {
		iface.HandleStackErr(err, false)
	}
	return true
}
