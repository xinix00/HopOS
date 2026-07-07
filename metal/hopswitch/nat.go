// Poort-publicatie: stateloze DNAT tussen de externe NIC en het interne net.
// Elke gepubliceerde poort heeft een vaste bestemming (node-IP:poort →
// slot-IP:poort), dus per pakket worden alleen de headers herschreven en de
// checksums incrementeel bijgewerkt (RFC 1624) — geen conntrack, geen
// TCP-terminatie op core 0: het externe verkeer wordt doorgerouterd, niet
// getunneld.
//
// Bewust niet gedekt (conntrack nodig, pas bouwen bij behoefte): hairpin
// (een interne client die via het nóde-IP naar een gepubliceerde poort
// dialt — gebruik het slot-IP) en masquerade voor uitgaand app-verkeer
// (uitgaand loopt via fetch/HOP). Niet-eerste IP-fragmenten dragen geen
// L4-header en gaan ongemoeid naar de HOP-stack (die ze negeert).
package hopswitch

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"

	gnet "github.com/usbarmory/go-net"

	"hop-os/metal/layout"
)

const (
	ethLen   = 14
	etIPv4   = 0x0800
	protoTCP = 6
	protoUDP = 17
)

// pub is één gepubliceerde poort. HOP's conventie (ER_PORT_*): de app bindt
// hetzelfde poortnummer als het node-poortnummer, maar de vertaling kan het
// aan als ze verschillen.
type pub struct {
	proto    byte
	nodePort uint16
	slot     int
	slotPort uint16
}

// NAT-state, onder het switch-mutex (mu): het uitgaande pad loopt toch al
// door de switch-lus, en Publish/Unpublish zijn zeldzaam.
var (
	pubs       []pub
	uplink     *Uplink
	nextHopFor = map[uint32][6]byte{} // client-IP → L2-next-hop (geleerd van inbound)
	uplinkTxMu sync.Mutex
)

// Uplink omhult de externe NIC: inkomende frames voor gepubliceerde poorten
// worden vóór de gvisor-stack afgevangen (DNAT → interne switch); de
// zendkant krijgt een mutex omdat gvisor én de NAT er allebei op zenden
// (virtionet.Transmit is zelf niet goroutine-veilig).
type Uplink struct {
	nic gnet.NetworkDevice
	ip  uint32
	mac [6]byte
}

// WrapUplink registreert de externe NIC bij de NAT en geeft de wrapper terug
// die hopnet in zijn go-net-Interface hangt. nodeIP is het externe node-IP.
func WrapUplink(nic gnet.NetworkDevice, nodeIP string, mac net.HardwareAddr) (*Uplink, error) {
	ip4 := net.ParseIP(nodeIP).To4()
	if ip4 == nil || len(mac) != 6 {
		return nil, fmt.Errorf("uplink: ongeldig IP %q of MAC %v", nodeIP, mac)
	}
	u := &Uplink{nic: nic, ip: binary.BigEndian.Uint32(ip4)}
	copy(u.mac[:], mac)
	mu.Lock()
	uplink = u
	mu.Unlock()
	return u, nil
}

// Receive haalt één frame van de NIC; frames voor gepubliceerde poorten
// worden geclaimd (DNAT, het interne net op) en bereiken de HOP-stack nooit.
func (u *Uplink) Receive(buf []byte) (int, error) {
	n, err := u.nic.Receive(buf)
	if n == 0 || err != nil {
		return n, err
	}
	if natInbound(buf[:n]) {
		return 0, nil
	}
	return n, err
}

// Transmit verstuurt één frame op de NIC (geserialiseerd).
func (u *Uplink) Transmit(buf []byte) error {
	uplinkTxMu.Lock()
	defer uplinkTxMu.Unlock()
	return u.nic.Transmit(buf)
}

// Publish routeert node-IP:nodePort → slot:slotPort (proto "tcp" of "udp").
// Fout bij een al gepubliceerde poort. De publicatie leeft tot UnpublishSlot
// (slots.Start/Stop koppelen dat aan de task-lifecycle).
func Publish(proto string, nodePort uint16, slot int, slotPort uint16) error {
	var p byte
	switch proto {
	case "tcp":
		p = protoTCP
	case "udp":
		p = protoUDP
	default:
		return fmt.Errorf("publish: proto %q (tcp/udp)", proto)
	}
	if slot < 1 || slot > layout.MaxSlots {
		return fmt.Errorf("publish: slot %d buiten bereik", slot)
	}
	if nodePort == 0 || slotPort == 0 {
		return fmt.Errorf("publish: poort 0")
	}
	mu.Lock()
	defer mu.Unlock()
	for _, e := range pubs {
		if e.proto == p && e.nodePort == nodePort {
			return fmt.Errorf("publish: %s/%d al gepubliceerd (slot %d)", proto, nodePort, e.slot)
		}
	}
	pubs = append(pubs, pub{proto: p, nodePort: nodePort, slot: slot, slotPort: slotPort})
	return nil
}

// UnpublishSlot trekt alle publicaties van slot i in.
func UnpublishSlot(i int) {
	mu.Lock()
	defer mu.Unlock()
	keep := pubs[:0]
	for _, e := range pubs {
		if e.slot != i {
			keep = append(keep, e)
		}
	}
	pubs = keep
}

// ipv4L4 valideert een IPv4-frame met TCP/UDP en volledige L4-header en
// geeft de offsets terug; ok=false voor al het andere (ARP, fragmenten, …).
func ipv4L4(f []byte) (ihl int, proto byte, ok bool) {
	if len(f) < ethLen+20 || binary.BigEndian.Uint16(f[12:]) != etIPv4 {
		return 0, 0, false
	}
	ip := f[ethLen:]
	if ip[0]>>4 != 4 {
		return 0, 0, false
	}
	ihl = int(ip[0]&0xf) * 4
	proto = ip[9]
	if ihl < 20 || len(ip) < ihl+8 || (proto != protoTCP && proto != protoUDP) ||
		binary.BigEndian.Uint16(ip[6:])&0x1fff != 0 {
		return 0, 0, false
	}
	return ihl, proto, true
}

// natInbound: frame van de externe NIC; true = geclaimd. DNAT: dst node-IP:
// nodePort → slot-IP:slotPort, dan als gewoon intern frame de switch in.
func natInbound(f []byte) bool {
	ihl, proto, ok := ipv4L4(f)
	if !ok {
		return false
	}
	ip := f[ethLen:]
	l4 := ip[ihl:]
	dport := binary.BigEndian.Uint16(l4[2:])

	mu.Lock()
	defer mu.Unlock()
	if uplink == nil || binary.BigEndian.Uint32(ip[16:]) != uplink.ip {
		return false
	}
	var m *pub
	for j := range pubs {
		if pubs[j].proto == proto && pubs[j].nodePort == dport {
			m = &pubs[j]
			break
		}
	}
	if m == nil {
		return false
	}

	// Leer de L2-next-hop van deze client voor het antwoordpad.
	nextHopFor[binary.BigEndian.Uint32(ip[12:])] = [6]byte(f[6:12])

	oldIP := binary.BigEndian.Uint32(ip[16:])
	newIP := slotIP4(m.slot)
	binary.BigEndian.PutUint32(ip[16:], newIP)
	fixCsum32(ip[10:], oldIP, newIP)
	rewriteL4(l4, proto, 2, oldIP, newIP, dport, m.slotPort)

	copy(f[0:6], slotMACBytes(m.slot))
	copy(f[6:12], hostMACBytes)
	p := make([]byte, len(f))
	copy(p, f)
	// Via het host-kanaal de switch in: forward(0, ·) routeert op de
	// dst-MAC naar het slot. Vol = drop (TCP herstelt).
	select {
	case hostOut <- p:
	default:
	}
	return true
}

// natFromSlot (onder mu, vanuit de switch-lus): frame van slot src richting
// de gateway; true = geclaimd. Het antwoordpad: SNAT slot-IP:slotPort →
// node-IP:nodePort en de externe NIC uit.
func natFromSlot(src int, f []byte) bool {
	ihl, proto, ok := ipv4L4(f)
	if !ok || uplink == nil {
		return false
	}
	ip := f[ethLen:]
	l4 := ip[ihl:]
	sport := binary.BigEndian.Uint16(l4[:])

	var m *pub
	for j := range pubs {
		if pubs[j].proto == proto && pubs[j].slot == src && pubs[j].slotPort == sport {
			m = &pubs[j]
			break
		}
	}
	if m == nil {
		return false
	}
	nextHop, seen := nextHopFor[binary.BigEndian.Uint32(ip[16:])]
	if !seen {
		return true // nooit inbound van deze client gezien: drop
	}

	oldIP := binary.BigEndian.Uint32(ip[12:])
	binary.BigEndian.PutUint32(ip[12:], uplink.ip)
	fixCsum32(ip[10:], oldIP, uplink.ip)
	rewriteL4(l4, proto, 0, oldIP, uplink.ip, sport, m.nodePort)

	copy(f[0:6], nextHop[:])
	copy(f[6:12], uplink.mac[:])
	uplink.Transmit(f)
	return true
}

// rewriteL4 werkt poort (op portOff: 0 = src, 2 = dst) en checksum van een
// TCP/UDP-header bij voor een IP- én poortwijziging. UDP-checksum 0 blijft 0.
func rewriteL4(l4 []byte, proto byte, portOff int, oldIP, newIP uint32, oldPort, newPort uint16) {
	csumOff := 16 // TCP
	if proto == protoUDP {
		csumOff = 6
	}
	binary.BigEndian.PutUint16(l4[portOff:], newPort)
	if proto == protoUDP && binary.BigEndian.Uint16(l4[csumOff:]) == 0 {
		return
	}
	fixCsum32(l4[csumOff:], oldIP, newIP) // pseudo-header
	fixCsum16(l4[csumOff:], oldPort, newPort)
}

// fixCsum16 werkt een internet-checksum (big-endian op b[0:2]) incrementeel
// bij voor één veranderd 16-bit woord (RFC 1624: HC' = ~(~HC + ~m + m')).
func fixCsum16(b []byte, old, new uint16) {
	sum := uint32(^binary.BigEndian.Uint16(b)) & 0xffff
	sum += uint32(^old) & 0xffff
	sum += uint32(new)
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	binary.BigEndian.PutUint16(b, ^uint16(sum))
}

// fixCsum32: idem voor een veranderd 32-bit woord (een IPv4-adres).
func fixCsum32(b []byte, old, new uint32) {
	fixCsum16(b, uint16(old>>16), uint16(new>>16))
	fixCsum16(b, uint16(old), uint16(new))
}
