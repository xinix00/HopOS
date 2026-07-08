// Package hopswitch is HOP's interne L2-frame-switch (per-slot netwerk):
// elke app-core draait een eigen netstack (applib/appnet) over rauwe
// Ethernet-frames door de per-slot frame-ringen; HOP kopieert die frames
// uitsluitend ring-naar-ring op de dst-MAC — app↔app-verkeer raakt nooit
// een TCP-stack op core 0. "Apps rekenen, HOP sjouwt data", maar dan zonder
// twee keer door core 0's hele TCP-stack te tunnelen.
//
// Adressering is deterministisch, geen tabellen die leren: slot i heeft MAC
// 02:00:00:00:00:<i> en IP 10.100.0.<i+1>/24; HOP zelf hangt als gewone
// deelnemer aan de switch (MAC ..:00, IP 10.100.0.1) via een eigen go-net
// Interface — daarmee kan HOP apps bereiken (health/control) zonder zijn
// externe stack (hopnet) aan te raken.
package hopswitch

import (
	"context"
	"fmt"
	"net"
	"sync"
	"syscall"
	"time"

	gnet "github.com/usbarmory/go-net"

	"hop-os/metal/layout"
	"hop-os/metal/ring"
)

// Het interne subnet: HOP = .1, slot i = .(i+1).
const (
	HostIP  = "10.100.0.1"
	prefix  = 24
	hostMAC = "02:00:00:00:00:00"

	// maxBurst begrenst het aantal frames per poort per switch-ronde, zodat
	// één drukke poort de rest niet verhongert.
	maxBurst = 64
)

// SlotIP geeft het interne IP van slot i.
func SlotIP(i int) string { return fmt.Sprintf("10.100.0.%d", i+1) }

// slotIP4 is SlotIP als uint32 (big-endian volgorde).
func slotIP4(i int) uint32 { return 10<<24 | 100<<16 | uint32(i+1) }

// slotMACBytes / hostMACBytes: de deterministische MACs als bytes.
func slotMACBytes(i int) []byte { return []byte{0x02, 0, 0, 0, 0, byte(i)} }

var hostMACBytes = []byte{0x02, 0, 0, 0, 0, 0}

// NetCfg geeft de twee control-page-woorden (layout.CtrlNetIP/CtrlNetGW)
// waarmee HOP de net-config van slot i aan de app meegeeft.
func NetCfg(i int) (ipCfg, gwCfg uint64) {
	return uint64(slotIP4(i)) | uint64(prefix)<<32, uint64(10)<<24 | uint64(100)<<16 | 1
}

// port is één switch-poort: de frame-ringen van een actief slot. De switch
// is per richting de enige tegenhanger van de app (SPSC): consumer op TX,
// producer op RX.
type port struct {
	tx *ring.Ring // app → switch
	rx *ring.Ring // switch → app
}

var (
	mu    sync.Mutex
	ports [layout.MaxSlots + 1]*port
	up    bool

	// HOP's eigen aansluiting op de switch: de gvisor-stack levert frames
	// via hostOut aan de switch-lus en krijgt ze binnen via hostIn — de
	// kanalen spelen de rol die de frame-ringen voor een slot spelen.
	hostStack *gnet.GVisorStack
	hostIn    chan []byte
	hostOut   chan []byte
)

// hostNIC is het go-net NetworkDevice van HOP's interne stack.
type hostNIC struct{}

func (hostNIC) Receive(buf []byte) (int, error) {
	select {
	case p := <-hostIn:
		return copy(buf, p), nil
	default:
		return 0, nil
	}
}

func (hostNIC) Transmit(buf []byte) error {
	p := make([]byte, len(buf))
	copy(p, buf)
	select {
	case hostOut <- p:
	default: // vol: drop, zoals een echte switch — TCP herstelt
	}
	return nil
}

// Up start de switch-lus en HOP's interne stack; idempotent. Aanroepen vóór
// de eerste slots.Start.
func Up() error {
	mu.Lock()
	defer mu.Unlock()
	if up {
		return nil
	}
	hostIn = make(chan []byte, 256)
	hostOut = make(chan []byte, 256)

	hostStack = gnet.NewGVisorStack(1)
	iface := &gnet.Interface{NetworkDevice: hostNIC{}, Stack: hostStack}
	if err := iface.Init(fmt.Sprintf("%s/%d", HostIP, prefix), hostMAC, ""); err != nil {
		return fmt.Errorf("interne stack: %w", err)
	}
	iface.Stack.EnableICMP()

	// RX-lus met microslaap i.p.v. gnet's Gosched-spin (zie metal/idle).
	go func() {
		buf := make([]byte, gnet.MTU+gnet.EthernetMaximumSize)
		nic := hostNIC{}
		for {
			// Per-iteratie recover: een panic in Receive (→ natInbound) of in
			// gvisor's RecvInboundPacket mag core 0 niet vellen — pakket
			// droppen, RX draait door. Zie switchPass.
			func() {
				defer func() {
					if r := recover(); r != nil {
						fmt.Printf("HOPOS_RX_PANIC: %v — pakket gedropt, RX draait door\n", r)
					}
				}()
				n, err := nic.Receive(buf)
				if n == 0 || err != nil {
					time.Sleep(300 * time.Microsecond)
					return
				}
				iface.Stack.RecvInboundPacket(buf[:n])
			}()
		}
	}()

	go loop()
	up = true
	return nil
}

// Attach koppelt slot i aan de switch (door slots.Start, ná de ring-init).
func Attach(i int) {
	if i < 1 || i > layout.MaxSlots {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	ports[i] = &port{
		tx: ring.Open(layout.NetRingTX(i)),
		rx: ring.Open(layout.NetRingRX(i)),
	}
}

// Detach ontkoppelt slot i. Keert pas terug als de switch-lus de ringen
// gegarandeerd niet meer aanraakt — aanroepen vóór een ring-herinit.
func Detach(i int) {
	if i < 1 || i > layout.MaxSlots {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	ports[i] = nil
}

// loop is dé switch: drain alle poorten, bezorg per frame op dst-MAC.
// Eén goroutine — daarmee is HOP vanzelf de enige producer per RX-ring en
// de enige consumer per TX-ring (SPSC zonder verdere sloten).
func loop() {
	// Eén hergebruikte leesbuffer voor alle TX-ringen (de switch-lus is één
	// goroutine): geen allocatie per frame op de netwerk-hot-path. forward
	// kopieert het frame synchroon in de dst-ring(en)/uplink; alleen toHost
	// geeft het aan een kanaal door en kopieert daarom zelf.
	buf := make([]byte, layout.NetRingDataCap)
	for {
		if !switchPass(buf) {
			time.Sleep(200 * time.Microsecond)
		}
	}
}

// switchPass draint alle poorten één ronde onder mu. Diepteverdediging: een
// panic (een bug, of frame-inhoud die tot in gvisor/nat reikt) mag core 0 —
// en dus álle slots — niet vellen. De defer ontgrendelt mu (ook bij een panic,
// anders deadlockt de volgende ronde) en recovert: het frame wordt gedropt en
// de switch draait door.
func switchPass(buf []byte) (worked bool) {
	mu.Lock()
	defer func() {
		mu.Unlock()
		if r := recover(); r != nil {
			fmt.Printf("HOPOS_SWITCH_PANIC: %v — frame gedropt, switch draait door\n", r)
		}
	}()
host:
	for range maxBurst {
		select {
		case p := <-hostOut:
			forward(0, p)
			worked = true
		default:
			break host
		}
	}
	for i := 1; i <= layout.MaxSlots; i++ {
		pt := ports[i]
		if pt == nil {
			continue
		}
		for range maxBurst {
			typ, n, ok := pt.tx.ReadInto(buf)
			if !ok {
				break
			}
			if typ != ring.TypeFrame {
				continue
			}
			forward(i, buf[:n])
			worked = true
		}
	}
	return worked
}

// forward bezorgt één frame op grond van de dst-MAC — meer switch is er
// niet. Onbekende bestemming of volle ring = drop (zoals echt Ethernet).
// Aanroepen met mu vast (vanuit loop).
func forward(src int, p []byte) {
	if len(p) < 14 {
		return
	}
	if p[0]&1 != 0 { // broadcast/multicast (ARP): iedereen behalve de bron
		for i := 1; i <= layout.MaxSlots; i++ {
			if i != src && ports[i] != nil {
				ports[i].rx.Write(ring.TypeFrame, p)
			}
		}
		if src != 0 {
			toHost(p)
		}
		return
	}
	if p[0] != 0x02 || p[1]|p[2]|p[3]|p[4] != 0 {
		return // geen switch-MAC
	}
	dst := int(p[5])
	if dst == 0 {
		if src != 0 {
			// Eerst de NAT: antwoorden van een gepubliceerde poort gaan
			// herschreven de externe NIC uit; de rest is voor HOP zelf.
			if natFromSlot(src, p) {
				return
			}
			toHost(p)
		}
		return
	}
	if dst != src && dst <= layout.MaxSlots && ports[dst] != nil {
		ports[dst].rx.Write(ring.TypeFrame, p)
	}
}

func toHost(p []byte) {
	// p wijst naar de hergebruikte switch-leesbuffer; kopiëren vóór het door
	// het kanaal aan HOP's stack-goroutine te geven (die leest 'm later pas).
	pp := make([]byte, len(p))
	copy(pp, p)
	select {
	case hostIn <- pp:
	default:
	}
}

// Dial opent een TCP-verbinding over het interne net (HOP → app, bv. voor
// health-checks). HOP's gewone net.Dial blijft aan de externe stack
// (hopnet) hangen; dit is de expliciete weg naar de slots.
func Dial(addr string, timeout time.Duration) (net.Conn, error) {
	mu.Lock()
	s := hostStack
	mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("switch niet gestart (Up)")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	var portNum int
	if _, err := fmt.Sscanf(portStr, "%d", &portNum); err != nil {
		return nil, fmt.Errorf("poort %q: %w", portStr, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("geen IP: %q", host)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, err := s.Socket(ctx, "tcp4", syscall.AF_INET, syscall.SOCK_STREAM,
		nil, &net.TCPAddr{IP: ip, Port: portNum})
	if err != nil {
		return nil, err
	}
	return c.(net.Conn), nil
}
