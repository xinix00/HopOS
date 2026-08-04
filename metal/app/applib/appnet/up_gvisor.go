package appnet

import (
	"fmt"
	"net"
	"time"

	gnet "github.com/usbarmory/go-net"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/abi/ring"
	"github.com/xinix00/HopOS/metal/app/applib"
)

// Up brengt de eigen netstack (gVisor) op en hangt hem in Go's net-package;
// geeft het eigen IP terug. Alle config is afgeleid uit het slotnummer via het
// gedeelde net-plan (layout) — HOP hoeft niets per slot door te geven; de
// switch en de app-stack leiden hetzelfde IP/gateway/MAC af (layout.IP4Str
// incluis), dus ze lopen nooit uiteen.
func Up(a *applib.App) (string, error) {
	ip := layout.IP4Str(layout.SlotIP4(a.Slot))
	cidr := fmt.Sprintf("%s/%d", ip, layout.NetPrefix)
	m := layout.SlotMAC(a.Slot)
	mac := fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", m[0], m[1], m[2], m[3], m[4], m[5])

	nd := &nic{
		tx: ring.Open(layout.NetRingTXAt(a.RAMStart, a.RAMSize)),
		rx: ring.Open(layout.NetRingRXAt(a.RAMStart, a.RAMSize)),
	}
	iface := &gnet.Interface{NetworkDevice: nd}
	if err := iface.Init(cidr, mac, layout.IP4Str(layout.HostIP4())); err != nil {
		return "", fmt.Errorf("netstack init: %w", err)
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
