// Package hopswitch is HOP's interne L2-frame-switch (per-slot netwerk):
// elke app-core draait een eigen netstack (applib/appnet) over rauwe
// Ethernet-frames door de per-slot frame-ringen; HOP kopieert die frames
// uitsluitend ring-naar-ring op de dst-MAC — app↔app-verkeer raakt nooit
// een TCP-stack op core 0. "Apps rekenen, HOP sjouwt data."
//
// HOP heeft géén eigen interne TCP-stack meer. Wat de apps van de gateway
// nodig hebben is precies twee dingen, en die doet de switch zelf:
//   - ARP voor de gateway (10.100.0.1) beantwoorden (arpReplyGateway);
//   - uitgaand verkeer naar buiten masqueraden en de antwoorden rechtstreeks
//     in de slot-ring terugleggen (nat.go/deliverLocked) — geen tunnel,
//     alleen header-herschrijving.
//
// Adressering is deterministisch, geen tabellen die leren: het net-plan
// (subnet, per-slot IP/MAC, gateway) leeft in metal/abi/layout, zodat de switch
// en de app-stacks nooit uiteenlopen. HOP is de gateway op .1 (MAC ..:00).
package hopswitch

import (
	"fmt"
	"sync"
	"time"

	"hop-os/metal/abi/layout"
	"hop-os/metal/abi/ring"
)

const (
	prefix = layout.NetPrefix

	// maxBurst begrenst het aantal frames per poort per switch-ronde, zodat
	// één drukke poort de rest niet verhongert.
	maxBurst = 64
)

// HostIP is HOP's interne adres (de gateway), SlotIP dat van slot i — beide
// uit het net-plan in layout, als string voor de mains (layout.IP4Str: de
// string-vorm woont bij de bron van het plan).
var HostIP = layout.IP4Str(layout.HostIP4())

func SlotIP(i int) string { return layout.IP4Str(layout.SlotIP4(i)) }

// hostMAC is HOP's MAC op het interne net (slot 0 → ..:00).
var hostMAC = layout.SlotMAC(0)

// port is één switch-poort: de frame-ringen van een actief slot. De switch
// is per richting de enige tegenhanger van de app (SPSC): consumer op TX,
// producer op RX.
type port struct {
	tx *ring.Ring // app → switch
	rx *ring.Ring // switch → app
}

var (
	mu    sync.Mutex
	ports []*port // [1..MaxSlots]; in Up() gedimensioneerd (MaxSlots is runtime)
	up    bool
)

// Up start de switch-lus; idempotent. Aanroepen vóór de eerste slots.Start.
func Up() error {
	mu.Lock()
	defer mu.Unlock()
	if up {
		return nil
	}
	ports = make([]*port, layout.MaxSlots+1) // MaxSlots staat vast na board-init
	go loop()
	up = true
	return nil
}

// Attach koppelt slot i aan de switch (door slots.Start, ná de ring-init).
// netPA is de fysieke net-ring-basis van dít slot — de partitie-staart, door
// kern/slots per lifecycle berekend en als parameter meegegeven (er is geen
// register dat stale kan worden); de TX/RX-offsets komen uit het layout-plan.
// No-op zolang de switch niet Up() is: ports is dan nog nil (lazy op de
// runtime-MaxSlots gedimensioneerd), en een board dat geen switch draait
// (de Pi-mains starten slots zonder hopswitch.Up) mag hier niet crashen —
// vóór de array→slice-wissel was dit een onschuldige no-op.
func Attach(i int, netPA uintptr) {
	if i < 1 || i > layout.MaxSlots {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if !up {
		return
	}
	ports[i] = &port{
		tx: ring.Open(netPA + layout.NetTXOff),
		rx: ring.Open(netPA + layout.NetRXOff),
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
	if !up {
		return
	}
	ports[i] = nil
}

// loop is dé switch: drain alle poorten, bezorg per frame op dst-MAC. Ringen
// worden uitsluitend onder mu beschreven (deze lus én het NAT-bezorgpad,
// deliverLocked) — daarmee is de mu-houder vanzelf de enige producer per
// RX-ring en de enige consumer per TX-ring (SPSC zonder verdere sloten).
func loop() {
	// Eén hergebruikte leesbuffer voor alle TX-ringen (de switch-lus is één
	// goroutine): geen allocatie per frame op de netwerk-hot-path. forward
	// kopieert het frame synchroon in de dst-ring(en) of via nat de uplink.
	buf := make([]byte, layout.NetRingDataCap)
	for {
		if !switchPass(buf) {
			time.Sleep(200 * time.Microsecond)
		}
	}
}

// switchPass draint alle poorten één ronde onder mu. (De uplink-inbound-kant
// — DNAT en masquerade-antwoorden — loopt hier niet meer doorheen: natInbound
// bezorgt onder ditzelfde mu rechtstreeks in de slot-ring, deliverLocked; de
// oude inject-queue kostte een allocatie + kopie + wachtbeurt per frame.)
// Diepteverdediging: een panic (een bug, of frame-inhoud die tot in nat
// reikt) mag core 0 — en dus álle slots — niet vellen. De defer ontgrendelt
// mu (ook bij een panic, anders deadlockt de volgende ronde) en recovert: het
// frame wordt gedropt en de switch draait door.
func switchPass(buf []byte) (worked bool) {
	mu.Lock()
	defer func() {
		mu.Unlock()
		if r := recover(); r != nil {
			fmt.Printf("HOPOS_SWITCH_PANIC: %v — frame gedropt, switch draait door\n", r)
		}
	}()
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

// deliverLocked legt een (door de NAT al herschreven) inbound frame
// rechtstreeks in de RX-ring van slot i — zonder tussenstop: élke
// RX-ring-write gebeurt onder mu (de switch-lus én dit pad), dus de
// SPSC-invariant (één producer) staat al; de oude inject-queue voegde alleen
// een allocatie, een kopie en een wachtbeurt op de switch-ronde toe (~2 van
// de ~5 ongecachte passes per frame — netdoorvoer-analyse 17-07). Vol of
// niet aangesloten = drop (zoals echt Ethernet; TCP herstelt). Aanroepen met
// mu vast (vanuit natInbound).
func deliverLocked(i int, p []byte) {
	if i < 1 || i >= len(ports) || ports[i] == nil {
		return
	}
	ports[i].rx.Write(ring.TypeFrame, p)
}

// forward bezorgt één frame op grond van de dst-MAC — meer switch is er
// niet. Onbekende bestemming of volle ring = drop (zoals echt Ethernet).
// Aanroepen met mu vast (vanuit switchPass).
func forward(src int, p []byte) {
	if len(p) < 14 {
		return
	}
	if p[0]&1 != 0 { // broadcast/multicast (ARP): iedereen behalve de bron
		if arpReplyGateway(src, p) { // who-has de gateway? HOP antwoordt zelf
			return
		}
		for i := 1; i <= layout.MaxSlots; i++ {
			if i != src && ports[i] != nil {
				ports[i].rx.Write(ring.TypeFrame, p)
			}
		}
		return
	}
	if p[0] != 0x02 || p[1]|p[2]|p[3]|p[4] != 0 {
		return // geen switch-MAC
	}
	dst := int(p[5])
	if dst == 0 { // naar HOP toe (de gateway-MAC)
		if src != 0 {
			// Eerst het antwoord van een gepubliceerde poort (SNAT de externe
			// NIC uit); anders uitgaand verkeer masqueraden. Wat geen van beide
			// is (er is verder geen interne HOP-stack meer) valt weg.
			if natFromSlot(src, p) {
				return
			}
			natOutbound(src, p)
		}
		return
	}
	if dst != src && dst <= layout.MaxSlots && ports[dst] != nil {
		ports[dst].rx.Write(ring.TypeFrame, p)
	}
}

// arpReplyGateway beantwoordt een ARP-request voor de gateway (10.100.0.1)
// namens HOP en schrijft het antwoord in de RX-ring van de vragende slot;
// true = afgehandeld. Andere ARP's (slot ↔ slot) worden gewoon geflood, die
// beantwoordt de doel-slot zelf. Aanroepen met mu vast.
//
// ARP-payload (RFC 826, na de 14-byte Ethernet-kop): htype(2) ptype(2)
// hlen(1) plen(1) oper(2) sha(6) spa(4) tha(6) tpa(4) = 28 bytes.
func arpReplyGateway(src int, p []byte) bool {
	if src < 1 || src > layout.MaxSlots || ports[src] == nil {
		return false
	}
	if len(p) < ethLen+28 || p[12] != 0x08 || p[13] != 0x06 {
		return false // geen ARP
	}
	a := p[ethLen:]
	// Ethernet/IPv4-request (oper=1) naar het gateway-IP?
	if a[0] != 0x00 || a[1] != 0x01 || a[2] != 0x08 || a[3] != 0x00 || a[6] != 0x00 || a[7] != 0x01 {
		return false
	}
	tpa := uint32(a[24])<<24 | uint32(a[25])<<16 | uint32(a[26])<<8 | uint32(a[27])
	if tpa != layout.HostIP4() {
		return false // niet voor de gateway: laat het floodpad het doen
	}
	sha := a[8:14]  // vragende MAC
	spa := a[14:18] // vragende IP

	var r [ethLen + 28]byte
	copy(r[0:6], sha)         // dst = de vrager
	copy(r[6:12], hostMAC[:]) // src = HOP
	r[12], r[13] = 0x08, 0x06 // ARP
	b := r[ethLen:]
	b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7] = 0x00, 0x01, 0x08, 0x00, 6, 4, 0x00, 0x02 // reply
	copy(b[8:14], hostMAC[:])                                                                 // sha = HOP
	gw := layout.HostIP4()
	b[14], b[15], b[16], b[17] = byte(gw>>24), byte(gw>>16), byte(gw>>8), byte(gw) // spa = gateway
	copy(b[18:24], sha)                                                            // tha = de vrager
	copy(b[24:28], spa)                                                            // tpa = zijn IP
	ports[src].rx.Write(ring.TypeFrame, r[:])
	return true
}
