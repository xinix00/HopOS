// Package hopnet brengt het netwerk van de HOP-kern op: de NIC-driver van
// het board onder de netstack, gehookt in Go's standaard net-package. Daarna
// werken net.Listen, net.Dial en net/http gewoon — precies wat de HOP-agent
// nodig heeft. Dit is "geen video, wel een poort".
//
// De stack is leannet (xinix00/lean, sinds 12-08); de opbouw staat in
// stack.go, het device-contract in metal/net/netdev, en de afwegingen in
// lean/leannet/DESIGN.md.
//
// Historie van dit bestand, want het verklaart de vorm: tot 09-08 stond hier
// gVisor (~90k gelinkte regels), daarna lneto via go-net, en sinds 12-08 onze
// eigen stack. Elke wissel raakte precies twee bestanden (stack.go en de
// stack-import) omdat álles eromheen aan het twee-methode-device hangt:
// HandleLocal en de tweede interne NIC zitten op de device-naad (locnet.go +
// internal.go), niet in de stack.
package hopnet

import (
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/xinix00/HopOS/metal/net/netdev"

	"github.com/xinix00/HopOS/metal/board"
	"github.com/xinix00/HopOS/metal/net/hopswitch"
	"github.com/xinix00/lean/leandhcp"
)

// Up initialiseert de NIC en de netstack en hangt ze in het net-package. Het
// IP-plan en de NIC-probe komen van het actieve board (op QEMU de slirp-
// defaults; op echt ijzer straks een board met DHCP/DT).
func Up() error {
	// Het board levert een kant-en-klaar netdev.Device (driver + init zijn
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

	// De stack zelf (stack.go): die hangt ook net.SocketFunc; wat terugkomt
	// is de RX-voeding voor rxLoop.
	recv, err := stackUp(loc, nc, mac)
	if err != nil {
		return err
	}

	// De interne gateway-naad: 10.100.0.1 = "mijn node" voor de apps — de
	// agent/leader zijn dan van binnenuit bereikbaar zonder proxy (zie
	// internal.go).
	upInternal(loc)

	net.SetDefaultNS([]string{nc.DNS})

	// RX-lus: pollen mét microslaap i.p.v. een Gosched-spin, zodat de
	// idle-governor (metal/cpu/idle) de core echt kan laten slapen als het stil
	// is; onder last wordt er nooit geslapen (ring leeg = pas dan slapen).
	// Over de locdev, zodat loopback- en gateway-frames vóór de NIC gaan.
	go rxLoop(loc, recv)

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

// rxLoop pompt frames van het device naar de stack; recv is de
// ingress-voeding die stack.go teruggaf.
func rxLoop(nic netdev.Device, recv func([]byte) error) {
	buf := make([]byte, netdev.MTU+netdev.EthernetMaximumSize)
	for {
		if !rxPass(nic, recv, buf) {
			time.Sleep(300 * time.Microsecond)
		}
	}
}

// rxPass doet één ontvangst-ronde, mét recover — het spiegelbeeld van
// hopswitch.switchPass. Dit is het meest blootgestelde pad van de node: ruwe
// frames van de fysieke NIC, remote en zonder authenticatie, en ze lopen via
// Uplink.Receive tot in de NAT (arpLearn, DNAT, checksum-herschrijving) en de
// netstack. Een panic op frame-inhoud zou hier de RX-goroutine velen, en dat
// is de hele node — álle slots. Frame gedropt, lus draait door; na een panic
// slaapt de lus zijn normale tikje, zodat een panic-storm de core niet opeet.
// Stack-fouten worden bewust niet geprint: dit is de broadcast-ruis van een
// echt LAN (mDNS, SSDP, router advertisements) en de console is een busy-wait
// per byte — GEMETEN 11-08: een LicheeRV ging er na ~250s aan dood. Meten doen
// we met netmeter/vitals, niet in het datapad.
func rxPass(nic netdev.Device, recv func([]byte) error, buf []byte) (worked bool) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("HOPOS_RX_PANIC: %v — frame dropped, RX keeps running\n", r)
		}
	}()
	n, err := nic.Receive(buf)
	if n == 0 || err != nil {
		return false
	}
	_ = recv(buf[:n])
	return true
}
