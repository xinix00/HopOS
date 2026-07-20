// TCP-RST bij slot-dood: een hard gekild slot stuurt nooit een FIN, dus zijn
// peers merken de dood pas via hun eigen read-deadline (de display wachtte
// 30s op een ping voor een window verdween — gemeten 19-07). De node wéét
// het wel: slots.Stop trekt het slot uit de switch. Dit bestand laat de
// switch dat moment doorvertellen als een TCP-RST naar elke peer, zodat een
// dode verbinding onmiddellijk een gewone verbindingsfout wordt — zonder
// enige medewerking van de app (dekt dus ook crashes en hangs).
//
// Daarvoor is per verbinding het volgende verwachte sequence-nummer nodig
// (RFC 5961: een RST wordt alleen op het exacte verwachte seq geaccepteerd;
// in-window-maar-niet-exact geeft een challenge-ACK — en die zou naar het
// dode slot gaan). De switch ziet al elk frame, dus we lezen passief mee:
//   - slot ↔ slot: een eigen tabelletje (conns) in het forward-pad;
//   - slot → extern: op de bestaande masquerade-flow (flow.slotSeq).
// Bewust niet gedekt (KISS, pas bij behoefte): inbound DNAT-verbindingen
// (externe client → gepubliceerde poort) — die peer is een volwaardig OS met
// eigen keepalives en herstelt zelf; de pijn zat bij de display op de node.
package hopswitch

import (
	"encoding/binary"
	"fmt"
	"time"

	"hop-os/metal/abi/layout"
	"hop-os/metal/abi/ring"
)

const (
	// maxConns begrenst de slot↔slot-tracking (zelfde anti-DoS-tactiek als
	// maxFlows): een app kan HOP's heap nooit laten vollopen. connIdle volgt
	// tcpIdle — de SURF-pings (elke ~10s) houden een levende verbinding vers.
	maxConns = 1024
	connIdle = tcpIdle

	tcpFlagFIN = 0x01
	tcpFlagSYN = 0x02
	tcpFlagRST = 0x04
)

// skey identificeert één slot↔slot-verbinding, richtingloos genormaliseerd:
// a is altijd de laagste (slot, poort)-kant — zie connKey.
type skey struct {
	a, b   uint8
	ap, bp uint16
}

// sconn is de bijgehouden staat: het volgende verwachte seq VAN elke kant.
type sconn struct {
	nextA, nextB uint32
	seen         time.Time
}

var (
	conns     = map[skey]*sconn{} // onder mu (zoals alle switch-state)
	connsFull bool                // eenmalig loggen bij een volle tabel
)

// tcpSegNext leest uit een gevalideerd IPv4/TCP-frame het "volgende verwachte
// seq van de zender" (seq + payload, SYN en FIN tellen mee) plus de vlaggen.
// ok=false bij een te korte/rare header. totLen (niet len(f)): Ethernet padt
// korte frames op naar 60 bytes.
func tcpSegNext(ip, l4 []byte, ihl int) (next uint32, flags byte, ok bool) {
	dataOff := int(l4[12]>>4) * 4
	totLen := int(binary.BigEndian.Uint16(ip[2:]))
	if dataOff < 20 || totLen < ihl+dataOff || len(ip) < totLen {
		return 0, 0, false
	}
	flags = l4[13]
	segLen := uint32(totLen - ihl - dataOff)
	if flags&tcpFlagSYN != 0 {
		segLen++
	}
	if flags&tcpFlagFIN != 0 {
		segLen++
	}
	return binary.BigEndian.Uint32(l4[4:]) + segLen, flags, true
}

// trackSlotTCP leest passief mee met een slot↔slot-frame (mu vast, vanuit
// forward). Geen geldig TCP = niks doen; een RST van een van beide kanten
// ruimt de entry op.
func trackSlotTCP(src, dst int, f []byte) {
	ihl, proto, ok := ipv4L4(f)
	if !ok || proto != protoTCP {
		return
	}
	ip := f[ethLen:]
	l4 := ip[ihl:]
	next, flags, ok := tcpSegNext(ip, l4, ihl)
	if !ok {
		return
	}
	sport := binary.BigEndian.Uint16(l4[0:])
	dport := binary.BigEndian.Uint16(l4[2:])
	k, fromA := connKey(src, sport, dst, dport)

	if flags&tcpFlagRST != 0 {
		delete(conns, k)
		return
	}
	c := conns[k]
	if c == nil {
		if len(conns) >= maxConns {
			sweepConns()
			if len(conns) >= maxConns {
				if !connsFull {
					connsFull = true
					fmt.Printf("HOPOS_RST_FULL: slot-conn-tabel vol (%d) — nieuwe verbindingen niet gevolgd\n", maxConns)
				}
				return
			}
		}
		connsFull = false
		c = &sconn{}
		conns[k] = c
	}
	if fromA {
		c.nextA = next
	} else {
		c.nextB = next
	}
	c.seen = time.Now()
}

// connKey normaliseert (srcSlot:srcPort, dstSlot:dstPort) naar een skey;
// fromA meldt of de zender de a-kant is.
func connKey(src int, sport uint16, dst int, dport uint16) (skey, bool) {
	if src < dst || (src == dst && sport <= dport) {
		return skey{a: uint8(src), b: uint8(dst), ap: sport, bp: dport}, true
	}
	return skey{a: uint8(dst), b: uint8(src), ap: dport, bp: sport}, false
}

// sweepConns verwijdert entries die langer dan connIdle stil waren (mu vast).
func sweepConns() {
	now := time.Now()
	for k, c := range conns {
		if now.Sub(c.seen) > connIdle {
			delete(conns, k)
		}
	}
}

// ResetPeers vertelt elke peer van het (dode) slot i dat de verbinding weg
// is: een TCP-RST met het exacte verwachte seq. Slot-peers krijgen 'm in hun
// RX-ring, externe masquerade-peers via de uplink (met het node-adres, zoals
// al hun verkeer). Aanroepen uit de slot-teardown, ná Detach (het dode slot
// zelf krijgt niets meer) en vóór UnpublishSlot (die de flows weggooit).
func ResetPeers(i int) {
	if i < 1 || i > layout.MaxSlots {
		return
	}
	mu.Lock()
	defer mu.Unlock()

	for k, c := range conns {
		var peer int
		var peerPort, deadPort uint16
		var seq uint32
		switch {
		case int(k.a) == i:
			peer, peerPort, deadPort, seq = int(k.b), k.bp, k.ap, c.nextA
		case int(k.b) == i:
			peer, peerPort, deadPort, seq = int(k.a), k.ap, k.bp, c.nextB
		default:
			continue
		}
		delete(conns, k)
		if peer < 1 || peer >= len(ports) || ports[peer] == nil {
			continue
		}
		f := rstFrame(layout.SlotMAC(i), layout.SlotMAC(peer),
			layout.SlotIP4(i), layout.SlotIP4(peer), deadPort, peerPort, seq)
		ports[peer].rx.Write(ring.TypeFrame, f[:])
	}

	// Externe peers van masquerade-flows: RST met de al-vertaalde bron
	// (node-IP:nodePort — seq's herschrijft de NAT niet, die zijn geldig).
	// De flows zelf ruimt UnpublishSlot zo op.
	if uplink == nil {
		return
	}
	for _, fl := range flowsFwd {
		if fl.slot != i || fl.proto != protoTCP || !fl.seqKnown {
			continue
		}
		nextHop, known := l2For(fl.dstIP)
		if !known {
			continue
		}
		f := rstFrame(uplink.mac, nextHop,
			uplink.ip, fl.dstIP, fl.nodePort, fl.dstPort, fl.slotSeq)
		uplink.Transmit(f[:])
	}
}

// rstFrame bouwt een minimaal Ethernet+IPv4+TCP-RST-frame (54 bytes, volle
// checksums — dit frame ontstaat hier, er valt niets incrementeel bij te
// werken).
func rstFrame(srcMAC, dstMAC [6]byte, srcIP, dstIP uint32, srcPort, dstPort uint16, seq uint32) [54]byte {
	var f [54]byte
	copy(f[0:6], dstMAC[:])
	copy(f[6:12], srcMAC[:])
	binary.BigEndian.PutUint16(f[12:], etIPv4)

	ip := f[ethLen:]
	ip[0] = 0x45 // IPv4, IHL 20
	binary.BigEndian.PutUint16(ip[2:], 40)
	binary.BigEndian.PutUint16(ip[6:], 0x4000) // DF
	ip[8] = 64                                 // TTL
	ip[9] = protoTCP
	binary.BigEndian.PutUint32(ip[12:], srcIP)
	binary.BigEndian.PutUint32(ip[16:], dstIP)
	binary.BigEndian.PutUint16(ip[10:], csumFin(csumAdd(0, ip[:20])))

	l4 := ip[20:]
	binary.BigEndian.PutUint16(l4[0:], srcPort)
	binary.BigEndian.PutUint16(l4[2:], dstPort)
	binary.BigEndian.PutUint32(l4[4:], seq)
	l4[12] = 5 << 4 // data-offset 20
	l4[13] = tcpFlagRST

	// TCP-checksum over pseudo-header (src, dst, 0, proto, tcpLen) + header.
	var ph [12]byte
	binary.BigEndian.PutUint32(ph[0:], srcIP)
	binary.BigEndian.PutUint32(ph[4:], dstIP)
	ph[9] = protoTCP
	binary.BigEndian.PutUint16(ph[10:], 20)
	binary.BigEndian.PutUint16(l4[16:], csumFin(csumAdd(csumAdd(0, ph[:]), l4[:20])))
	return f
}

// csumAdd sommeert b (big-endian 16-bit woorden) op een lopende checksum.
func csumAdd(sum uint32, b []byte) uint32 {
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	return sum
}

// csumFin vouwt de carries en complementeert (internet-checksum, RFC 1071).
func csumFin(sum uint32) uint16 {
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
