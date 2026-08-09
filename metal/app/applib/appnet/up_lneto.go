package appnet

import (
	"fmt"
	"net"
	"net/netip"
	"time"

	gnet "github.com/usbarmory/go-net"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/abi/ring"
	"github.com/xinix00/HopOS/metal/app/applib"
)

// Up brengt de eigen netstack (lneto via go-net) op en hangt hem in Go's
// net-package; geeft het eigen IP terug. Alle config is afgeleid uit het
// slotnummer via het gedeelde net-plan (layout) — HOP hoeft niets per slot
// door te geven; de switch en de app-stack leiden hetzelfde IP/gateway/MAC af
// (layout.IP4Str incluis), dus ze lopen nooit uiteen.
//
// Tot 09-08 stond hier gVisor: ~2,7MB van elk app-image en 340k allocaties
// per 64MiB op het RX-pad. De flip is gemeten op de netmeter-bank (metal/
// cmd/netmeter); bouw apps mét -tags nodefaultstack, anders linkt go-net's
// Interface-fallback gvisor alsnog mee.
func Up(a *applib.App) (string, error) {
	ip := layout.IP4Str(layout.SlotIP4(a.Slot))
	cidr := fmt.Sprintf("%s/%d", ip, layout.NetPrefix)
	m := layout.SlotMAC(a.Slot)
	mac := fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", m[0], m[1], m[2], m[3], m[4], m[5])

	nd := &nic{
		tx: ring.Open(layout.NetRingTXAt(a.RAMStart, a.RAMSize)),
		rx: ring.Open(layout.NetRingRXAt(a.RAMStart, a.RAMSize)),
	}

	// App-maten: het interne net is LAN-graad (microseconden-RTT), dus 32KiB
	// buffers volstaan ruim (venster past dan zonder scaling); uitgaand
	// WAN-verkeer (cloudflared) is bescheiden van tempo. Élke net.Listen
	// alloceert MaxListenerConns×2×buffer vooraf — apps delen een partitie
	// van tientallen MB, dus dit blijft bewust klein.
	cfg := gnet.DefaultLnetoStackConfig()
	cfg.Hostname = "hopapp" // alleen DHCP gebruikt dit, en apps doen geen DHCP
	cfg.TCPBufferSize = 32 << 10
	cfg.TCPQueueSize = 16
	cfg.MaxActiveTCPPorts = 32
	cfg.MaxListenerConns = 8
	// Het interne net is deterministisch (layout nummert IP's én MAC's per
	// slot), dus er wordt NIETS geresolved: de gateway-MAC statisch in de
	// config (geen ARP-goroutine) en 10.100.0.1 als vaste buur geseed — dan
	// hebben dials naar de node en listener-antwoorden nul ARP nodig. Dat is
	// hetzelfde principe als de statische buren van het oude interne-NIC-
	// ontwerp: een geflood who-has gelooft wie het eerst antwoordt.
	cfg.GatewayHardwareAddr = layout.SlotMAC(0)

	st := gnet.NewLnetoStack(cfg)
	iface := &gnet.Interface{NetworkDevice: nd, Stack: st}
	if err := iface.Init(cidr, mac, layout.IP4Str(layout.HostIP4())); err != nil {
		return "", fmt.Errorf("netstack init: %w", err)
	}
	gwIP, _ := netip.AddrFromSlice([]byte{byte(layout.HostIP4() >> 24), byte(layout.HostIP4() >> 16), byte(layout.HostIP4() >> 8), byte(layout.HostIP4())})
	if err := st.SeedNeighbor(gwIP, layout.SlotMAC(0)); err != nil {
		return "", fmt.Errorf("seed gateway neighbor: %w", err)
	}
	iface.Stack.EnableICMP()

	// Een dode peer merken is APP-WERK, niet kernelwerk (Derek, 26-07). Er
	// stond hier kort een OnExit-hook met Stack.Close, en daarna deed de switch
	// het met gefabriceerde RST's (hopswitch/rst.go) — beide gesloopt. De reden:
	// een switch of router kan een verbinding altijd stil doodmaken, dus een app
	// moét tegen stilte kunnen. Dan is een snelkoppeling voor één van de
	// oorzaken pure redundantie — HOP heeft al twee lagen die dit dekken (de
	// health-check op de task en de app-eigen ping). Wie snel wil merken dat
	// zijn peer weg is, zet zijn read-deadline op een paar keer zijn
	// ping-interval; korter dan dat sloopt gezonde verbindingen.

	// In Go's standaard net-package hangen: hierna werken net.Listen en
	// net.Dial voor deze app. Interne IP's zijn deterministisch (geen DNS
	// nodig); voor uitgaand verkeer krijgt de app de node-resolver via HOP_DNS
	// mee (queries lopen als UDP door HOP's masquerade).
	net.SocketFunc = iface.Stack.Socket
	if dns := a.Env("HOP_DNS"); dns != "" {
		net.SetDefaultNS([]string{dns})
	}

	// RX-lus met microslaap i.p.v. gnet's Gosched-spin: een idle job laat
	// zo zijn hele core slapen (zie metal/cpu/idle).
	go func() {
		buf := make([]byte, gnet.MTU+gnet.EthernetMaximumSize)
		for {
			n, err := nd.Receive(buf)
			if n == 0 || err != nil {
				time.Sleep(300 * time.Microsecond)
				continue
			}
			iface.Stack.RecvInboundPacket(buf[:n])
		}
	}()
	return ip, nil
}
