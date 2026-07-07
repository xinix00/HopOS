// Package appnet geeft een app zijn eigen netstack (per-slot netwerk): een
// gVisor-stack (via go-net) over de frame-ringen naar HOP's L2-switch
// (metal/hopswitch). Na Up werken net.Listen en net.Dial gewoon — op het
// interne net (10.100.0.0/24) praat een app rechtstreeks met andere apps en
// met HOP, zonder dat er ooit een TCP-stack op core 0 tussen zit.
//
// Bewust een apart pakket naast applib: alleen apps die netwerk willen
// linken de netstack mee (gVisor is fors); wie het niet importeert houdt
// een kleine image.
package appnet

import (
	"fmt"
	"net"
	"sync"
	"time"

	gnet "github.com/usbarmory/go-net"

	"hop-os/metal/applib"
	"hop-os/metal/dev"
	"hop-os/metal/layout"
	"hop-os/metal/ring"
)

// nic is het go-net NetworkDevice over de eigen frame-ringen.
type nic struct {
	mu sync.Mutex // Transmit kan uit meerdere goroutines komen; ring is SPSC
	tx *ring.Ring // app → switch (wij producer)
	rx *ring.Ring // switch → app (wij consumer)
}

// Receive levert één frame uit de RX-ring (0 = niets; go-net pollt).
// Uitsluitend door de RX-lus van go-net aangeroepen — één consumer. ReadInto
// leest het frame rechtstreeks in buf: geen allocatie én geen extra kopie per
// frame (buf is ruim MTU-groot, dus elk doorgezet Ethernet-frame past).
func (n *nic) Receive(buf []byte) (int, error) {
	typ, m, ok := n.rx.ReadInto(buf)
	if !ok || typ != ring.TypeFrame {
		return 0, nil
	}
	return m, nil
}

// Transmit zet één frame in de TX-ring; vol = drop (TCP herstelt).
func (n *nic) Transmit(buf []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.tx.Write(ring.TypeFrame, buf)
	return nil
}

func ip4str(v uint32) string {
	return fmt.Sprintf("%d.%d.%d.%d", byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

// Up brengt de eigen netstack op met de config die HOP bij de start op de
// control-page zette en hangt hem in Go's net-package; geeft het eigen IP
// terug. Fout als HOP geen netwerk voor dit slot inrichtte.
func Up(a *applib.App) (string, error) {
	ipCfg := dev.Read64(layout.CtrlPage(a.Slot) + layout.CtrlNetIP)
	if ipCfg == 0 {
		return "", fmt.Errorf("geen netwerk ingericht voor slot %d", a.Slot)
	}
	gwCfg := dev.Read64(layout.CtrlPage(a.Slot) + layout.CtrlNetGW)

	ip := ip4str(uint32(ipCfg))
	cidr := fmt.Sprintf("%s/%d", ip, ipCfg>>32&0xFF)
	mac := fmt.Sprintf("02:00:00:00:00:%02x", a.Slot)

	nd := &nic{
		tx: ring.Open(layout.NetRingTX(a.Slot)),
		rx: ring.Open(layout.NetRingRX(a.Slot)),
	}
	iface := &gnet.Interface{NetworkDevice: nd}
	if err := iface.Init(cidr, mac, ip4str(uint32(gwCfg))); err != nil {
		return "", fmt.Errorf("netstack init: %w", err)
	}
	iface.Stack.EnableICMP()

	// In Go's standaard net-package hangen: hierna werken net.Listen en
	// net.Dial voor deze app. Geen DNS op het interne net — IP's zijn
	// deterministisch (hopswitch.SlotIP) en komen via env binnen.
	net.SocketFunc = iface.Stack.Socket

	// RX-lus met microslaap i.p.v. gnet's Gosched-spin: een idle job laat
	// zo zijn hele core slapen (zie metal/idle).
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
