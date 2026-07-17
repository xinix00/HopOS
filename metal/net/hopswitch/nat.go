// NAT tussen de externe NIC en het interne net — twee richtingen, geen tunnel:
//
//   - Poort-publicatie (DNAT): een vaste bestemming node-IP:poort → slot-IP:
//     poort. Stateloos: per pakket alleen headers herschrijven, checksums
//     incrementeel (RFC 1624). natInbound (extern → slot) en natFromSlot
//     (slot-antwoord → extern).
//   - Uitgaand (masquerade / PAT): een app dialt naar buiten; HOP herschrijft
//     bron slot-IP:poort → node-IP:node-poort en houdt een kleine conntrack
//     bij zodat het antwoord terugvindt. TCP én UDP (DNS, QUIC). natOutbound
//     (slot → extern) en de reply-tak in natInbound. Nooit TCP-terminatie op
//     core 0 — HOP herschrijft alleen headers en schuift het frame door.
//
// De L2-next-hop (dst-MAC de NIC op) komt uit een neighbor-cache die passief
// leert uit inbound frames: srcIP→srcMAC, en een frame van búíten ons subnet
// is via de gateway gerelayed → dat is de gateway-MAC (de fallback voor een
// nog-niet-geziene bestemming). HOP's eigen boot-verkeer (SNTP off-subnet, DNS
// on-subnet) vult beide vóór de eerste app draait.
//
// Bewust niet gedekt (KISS, pas bij behoefte): hairpin (interne client naar
// het node-IP — gebruik het slot-IP) en een bestemming op HOP's eigen subnet
// die HOP zelf nog nooit sprak (geen neighbor → drop, de retransmit leert 'm).
package hopswitch

import (
	"encoding/binary"
	"fmt"
	"net"
	"runtime"
	"sync"
	"time"

	gnet "github.com/usbarmory/go-net"

	"hop-os/metal/abi/layout"
)

const (
	ethLen   = 14
	etIPv4   = 0x0800
	protoTCP = 6
	protoUDP = 17

	// Masquerade-poortbereik (PAT) en conntrack-grenzen. MasqBase/MasqEnd is
	// bewust disjunct van het efemere bereik van HOP's eigen externe stack
	// (hopnet begrenst die op [16000, MasqBase)): anders zou een inbound
	// antwoord op HOP's eigen DNS/S3-poort per ongeluk een masquerade-flow naar
	// dezelfde peer kunnen matchen. Het plafond maxFlows is de anti-DoS-grens
	// (zoals bij de neighbor-cache): een app kan HOP's heap op core 0 nooit
	// laten vollopen. Idle-timeouts ruimen dode flows op — geen TCP-toestand
	// (FIN/RST) volgen, alleen inactiviteit; keepalives van een langlopende
	// tunnel (cloudflared ~30-90s) blijven ruim binnen tcpIdle.
	MasqBase = 20000
	MasqEnd  = 60000
	maxFlows = 4096
	tcpIdle  = 300 * time.Second
	udpIdle  = 60 * time.Second

	// maxNeigh begrenst de neighbor-cache (spoofbare srcIP als key): bij het
	// plafond legen en herleren, net als de oude next-hop-tabel.
	maxNeigh = 4096

	// claimYield: elke zoveel achter elkaar door de NAT geclaimde frames geeft
	// Receive de core even af (runtime.Gosched) — op tamago is er geen async-
	// preëmptie, dus een sustained-line-rate-drain zou anders de andere
	// node-goroutines (switch-lus, agent, watchdog-aai) verhongeren.
	claimYield = 32
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

// flow is één uitgaande masquerade-verbinding.
type flow struct {
	proto    byte
	slot     int
	slotIP   uint32
	slotPort uint16
	dstIP    uint32
	dstPort  uint16
	nodePort uint16
	seen     time.Time
}

// fkey/rkey: forward-lookup (slot → nieuw/bestaand flow) en reverse-lookup
// (inbound antwoord → flow, op node-poort + peer).
type fkey struct {
	proto        byte
	sIP, dIP     uint32
	sPort, dPort uint16
}
type rkey struct {
	proto byte
	nPort uint16
	pIP   uint32
	pPort uint16
}

// Alle NAT-state onder het switch-mutex (mu, hopswitch.go): het uitgaande pad
// loopt toch al door de switch-lus (onder mu), Publish/Unpublish zijn zeldzaam
// en natInbound (uplink-RX-goroutine) neemt mu zelf.
var (
	pubs   []pub
	uplink *Uplink

	neigh   = map[uint32][6]byte{} // IP → L2-next-hop (passief geleerd)
	gwMAC   [6]byte                // gateway-MAC (van off-subnet inbound)
	gwKnown bool

	flowsFwd  = map[fkey]*flow{}
	flowsRev  = map[rkey]*flow{}
	masqNext  = uint16(MasqBase)
	flowsFull bool // eenmalig loggen bij een volle pool
)

// Uplink omhult de externe NIC: inkomende frames voor gepubliceerde poorten of
// masquerade-antwoorden worden vóór de gvisor-stack afgevangen (→ interne
// switch); de zendkant krijgt een mutex omdat gvisor én de NAT er allebei op
// zenden (de NIC-Transmit is zelf niet goroutine-veilig).
type Uplink struct {
	nic  gnet.NetworkDevice
	ip   uint32
	mask uint32
	mac  [6]byte
}

// uplinkTxMu serialiseert de zendkant: gvisor (hopnet) én de NAT zenden
// allebei op de externe NIC, en NIC-Transmit is niet goroutine-veilig.
var uplinkTxMu sync.Mutex

// WrapUplink registreert de externe NIC bij de NAT en geeft de wrapper terug
// die hopnet in zijn go-net-Interface hangt. cidr is het externe node-adres
// mét prefix (bv. "10.0.2.15/24") — het masker bepaalt wat "off-subnet" is.
func WrapUplink(nic gnet.NetworkDevice, cidr string, mac net.HardwareAddr) (*Uplink, error) {
	ip, ipnet, err := net.ParseCIDR(cidr)
	ip4 := ip.To4()
	if err != nil || ip4 == nil || len(mac) != 6 {
		return nil, fmt.Errorf("uplink: ongeldige CIDR %q of MAC %v", cidr, mac)
	}
	u := &Uplink{
		nic:  nic,
		ip:   binary.BigEndian.Uint32(ip4),
		mask: binary.BigEndian.Uint32(ipnet.Mask),
	}
	copy(u.mac[:], mac)
	mu.Lock()
	uplink = u
	mu.Unlock()
	return u, nil
}

// Receive levert het eerste frame dat NIET door de NAT geclaimd wordt terug
// voor HOP's eigen stack; geclaimde frames (app-downloads, masquerade-antwoorden,
// DNAT-inbound) gaan via de switch naar hun slot en tellen niet als "niks".
//
// NETDOORVOER (16-07): dit draait nu dóór tot de NIC-ring leeg is (n==0) of er
// een frame voor HOP zelf ligt. Vóórheen meldde elke geclaimde frame (0, nil),
// waarna de rxLoop (hopnet) 300µs sliep — dus ~1 slaap PER gedownloade frame:
// ~3300 frames/s ≈ ~3,6MB/s dak op álle app-downloads (precies het gemeten dak).
// De rxLoop-comment "onder last wordt er nooit geslapen" klopte daardoor niet;
// dit herstelt de bedoeling: pas slapen als de NIC-ring écht leeg is. De node
// draait op één core (GOMAXPROCS=1), dus geven we tussen batches af zodat de
// switch-lus de inject-queue en de app zijn rx-ring bijbenen — coöperatief
// afgeven ís hier de concurrency (het Go-idee).
func (u *Uplink) Receive(buf []byte) (int, error) {
	for claimed := 0; ; claimed++ {
		n, err := u.nic.Receive(buf)
		if n == 0 || err != nil {
			return n, err // NIC-ring leeg (of fout): pas hier mag de rxLoop slapen
		}
		// ARP eerst (niet claimen — gvisor wil replies óók zien voor de eigen
		// node-stack): de reply op onze first-contact-request leert de neighbor.
		arpLearn(buf[:n])
		if !natInbound(buf[:n]) {
			return n, err // frame voor HOP's eigen stack
		}
		// Geclaimd en al in de slot-ring bezorgd (deliverLocked kopieert het
		// frame de ring in, dus buf mag meteen hergebruikt): direct de
		// volgende halen, niet slapen. Af en toe afgeven zodat de andere
		// node-goroutines kunnen bijbenen.
		if claimed%claimYield == claimYield-1 {
			runtime.Gosched()
		}
	}
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
		return fmt.Errorf("publish: slot %d out of range", slot)
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

// UnpublishSlot trekt alle publicaties van slot i in en ruimt zijn
// masquerade-flows op (poorten meteen vrij; de core is toch uit).
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
	for k, fl := range flowsFwd {
		if fl.slot == i {
			delete(flowsRev, rkey{fl.proto, fl.nodePort, fl.dstIP, fl.dstPort})
			delete(flowsFwd, k)
		}
	}
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
	if ihl < 20 || binary.BigEndian.Uint16(ip[6:])&0x1fff != 0 {
		return 0, 0, false
	}
	// rewriteL4 raakt bij TCP l4[16:18] (volledige 20-byte header) en bij UDP
	// l4[6:8] (8-byte header) aan; een te korte header hier weigeren i.p.v.
	// straks een slice buiten bereik paniek (dat velt de hele node).
	switch proto {
	case protoTCP:
		if len(ip) < ihl+20 {
			return 0, 0, false
		}
	case protoUDP:
		if len(ip) < ihl+8 {
			return 0, 0, false
		}
	default:
		return 0, 0, false
	}
	return ihl, proto, true
}

// onSubnet meldt of ip op HOP's externe subnet ligt (dan is de neighbor-MAC
// de echte host; anders is het verkeer via de gateway gerelayed).
func onSubnet(ip uint32) bool { return uplink != nil && ip&uplink.mask == uplink.ip&uplink.mask }

// learnLocked leert de L2-next-hop uit een inbound frame (mu vast): srcIP →
// srcMAC, en een off-subnet bron betekent dat srcMAC de gateway is.
func learnLocked(srcIP uint32, mac []byte) {
	if _, known := neigh[srcIP]; !known && len(neigh) >= maxNeigh {
		neigh = map[uint32][6]byte{} // plafond: legen en herleren
	}
	neigh[srcIP] = [6]byte(mac)
	if !onSubnet(srcIP) {
		gwMAC, gwKnown = [6]byte(mac), true
	}
}

// arpLast rate-limit de eigen ARP-requests (per bestemming max 1/s): een
// storm van 127 loaders naar dezelfde onbekende host mag geen ARP-storm
// worden. Zelfde plafond-tactiek als neigh: legen en herleren.
var arpLast = map[uint32]time.Time{}

// arpForLocked stuurt een ARP-request voor een on-subnet bestemming (mu
// vast). Off-subnet gaat via de gateway en die leert passief (elk inbound
// pakket van buiten draagt zijn MAC); on-subnet first-contact niet — daar is
// dit de enige weg.
func arpForLocked(dstIP uint32) {
	if uplink == nil || !onSubnet(dstIP) {
		return
	}
	if t, ok := arpLast[dstIP]; ok && time.Since(t) < time.Second {
		return
	}
	if len(arpLast) >= maxNeigh {
		arpLast = map[uint32]time.Time{}
	}
	arpLast[dstIP] = time.Now()
	var f [42]byte
	for i := range 6 {
		f[i] = 0xFF // broadcast
	}
	copy(f[6:12], uplink.mac[:])
	f[12], f[13] = 0x08, 0x06 // ARP
	a := f[14:]
	a[0], a[1], a[2], a[3], a[4], a[5] = 0, 1, 0x08, 0, 6, 4 // eth/IPv4
	a[7] = 1                                                 // oper = request
	copy(a[8:14], uplink.mac[:])
	binary.BigEndian.PutUint32(a[14:], uplink.ip)
	// tha (a[18:24]) blijft 0 — onbekend, dat is de vraag.
	binary.BigEndian.PutUint32(a[24:], dstIP)
	uplink.Transmit(f[:])
}

// arpLearn leert spa→sha uit een inbound ARP (reply én request dragen beide
// een geldig sender-paar). Alleen on-subnet en spa ≠ 0: een ARP-probe
// (spa 0.0.0.0, RFC 5227) draagt geen bruikbaar adres — en zou via
// learnLocked's off-subnet-tak zelfs het gateway-MAC vergiftigen.
func arpLearn(f []byte) {
	if len(f) < 42 || f[12] != 0x08 || f[13] != 0x06 {
		return
	}
	a := f[14:]
	if binary.BigEndian.Uint16(a[0:]) != 1 || a[2] != 0x08 || a[3] != 0 || a[4] != 6 || a[5] != 4 {
		return
	}
	if op := binary.BigEndian.Uint16(a[6:]); op != 1 && op != 2 {
		return
	}
	spa := binary.BigEndian.Uint32(a[14:])
	if spa == 0 {
		return
	}
	mu.Lock()
	if onSubnet(spa) {
		learnLocked(spa, a[8:14])
	}
	mu.Unlock()
}

// l2For geeft de dst-MAC om dstIP te bereiken (mu vast): de geleerde neighbor,
// of de gateway voor een off-subnet/onbekende bestemming.
func l2For(dstIP uint32) ([6]byte, bool) {
	if onSubnet(dstIP) {
		if m, ok := neigh[dstIP]; ok {
			return m, true
		}
	}
	return gwMAC, gwKnown
}

// natInbound: frame van de externe NIC; true = geclaimd. Leert de neighbor,
// probeert dan een masquerade-antwoord (lopende uitgaande flow) en anders
// DNAT (gepubliceerde poort).
func natInbound(f []byte) bool {
	ihl, proto, ok := ipv4L4(f)
	if !ok {
		return false
	}
	ip := f[ethLen:]
	l4 := ip[ihl:]
	srcIP := binary.BigEndian.Uint32(ip[12:])

	mu.Lock()
	defer mu.Unlock()
	if uplink == nil {
		return false
	}
	// srcIP 0.0.0.0 (DHCP-discover/request van een buurman, broadcast) of een
	// multicast-bron-MAC niet leren: 0.0.0.0 is off-subnet en zou via
	// learnLocked het gateway-MAC vergiftigen — elk apparaat op het LAN dat
	// DHCP't werd dan even "de gateway" (review-kruimel #10).
	if srcIP != 0 && f[6]&1 == 0 {
		learnLocked(srcIP, f[6:12])
	}
	if replyInLocked(f, ip, l4, proto) {
		return true
	}
	return dnatInLocked(f, ip, l4, proto)
}

// replyInLocked vertaalt een inbound antwoord op een masquerade-flow terug en
// legt het rechtstreeks in de slot-ring (deliverLocked, mu vast); true = geclaimd.
func replyInLocked(f, ip, l4 []byte, proto byte) bool {
	if binary.BigEndian.Uint32(ip[16:]) != uplink.ip {
		return false
	}
	peerIP := binary.BigEndian.Uint32(ip[12:])
	peerPort := binary.BigEndian.Uint16(l4[0:])
	nodePort := binary.BigEndian.Uint16(l4[2:])
	fl := flowsRev[rkey{proto, nodePort, peerIP, peerPort}]
	if fl == nil {
		return false
	}
	fl.seen = time.Now()
	binary.BigEndian.PutUint32(ip[16:], fl.slotIP)
	fixCsum32(ip[10:], uplink.ip, fl.slotIP) // IP-header checksum
	rewriteL4(l4, proto, 2, uplink.ip, fl.slotIP, nodePort, fl.slotPort)
	mac := layout.SlotMAC(fl.slot)
	copy(f[0:6], mac[:])
	copy(f[6:12], hostMAC[:])
	deliverLocked(fl.slot, f)
	return true
}

// dnatInLocked: DNAT van node-IP:nodePort → slot-IP:slotPort (mu vast).
func dnatInLocked(f, ip, l4 []byte, proto byte) bool {
	if binary.BigEndian.Uint32(ip[16:]) != uplink.ip {
		return false
	}
	dport := binary.BigEndian.Uint16(l4[2:])
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
	oldIP := binary.BigEndian.Uint32(ip[16:])
	newIP := layout.SlotIP4(m.slot)
	binary.BigEndian.PutUint32(ip[16:], newIP)
	fixCsum32(ip[10:], oldIP, newIP)
	rewriteL4(l4, proto, 2, oldIP, newIP, dport, m.slotPort)
	mac := layout.SlotMAC(m.slot)
	copy(f[0:6], mac[:])
	copy(f[6:12], hostMAC[:])
	deliverLocked(m.slot, f)
	return true
}

// natFromSlot (mu vast, vanuit de switch-lus): frame van slot src richting de
// gateway; true = geclaimd. Het antwoordpad van een gepubliceerde poort: SNAT
// slot-IP:slotPort → node-IP:nodePort en de externe NIC uit.
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
	nextHop, known := l2For(binary.BigEndian.Uint32(ip[16:]))
	if !known {
		return true // next-hop onbekend: drop, de retransmit leert 'm
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

// natOutbound (mu vast, vanuit de switch-lus): een app dialt naar buiten.
// Masquerade: bron slot-IP:slotPort → node-IP:node-poort (uit de conntrack),
// dan de externe NIC uit. true = afgehandeld (ook als het gedropt is).
func natOutbound(src int, f []byte) bool {
	ihl, proto, ok := ipv4L4(f)
	if !ok || uplink == nil {
		return false
	}
	ip := f[ethLen:]
	l4 := ip[ihl:]
	dstIP := binary.BigEndian.Uint32(ip[16:])
	slotIP := layout.SlotIP4(src)
	sport := binary.BigEndian.Uint16(l4[0:])
	dport := binary.BigEndian.Uint16(l4[2:])

	nextHop, known := l2For(dstIP)
	if !known {
		// First-contact (Altra 14-07): een on-subnet bestemming die ons nooit
		// eerder iets stuurde is onbekend — passief leren komt dan nooit. Vraag
		// het net (ARP-request, rate-limited); de reply leert de neighbor
		// (arpLearn) en de TCP-retransmit van de app vindt 'm daarna.
		arpForLocked(dstIP)
		return true // next-hop (nog) niet geleerd: drop, retransmit volgt
	}
	fl := flowFor(proto, src, slotIP, sport, dstIP, dport)
	if fl == nil {
		return true // pool vol: drop
	}
	fl.seen = time.Now()
	binary.BigEndian.PutUint32(ip[12:], uplink.ip)
	fixCsum32(ip[10:], slotIP, uplink.ip)
	rewriteL4(l4, proto, 0, slotIP, uplink.ip, sport, fl.nodePort)
	copy(f[0:6], nextHop[:])
	copy(f[6:12], uplink.mac[:])
	uplink.Transmit(f)
	return true
}

// flowFor vindt of maakt de conntrack-entry voor een uitgaande flow (mu vast);
// nil als de pool vol is (na een sweep van verlopen flows).
func flowFor(proto byte, slot int, slotIP uint32, slotPort uint16, dstIP uint32, dstPort uint16) *flow {
	k := fkey{proto, slotIP, dstIP, slotPort, dstPort}
	if fl := flowsFwd[k]; fl != nil {
		return fl
	}
	if len(flowsFwd) >= maxFlows {
		sweepExpired()
		if len(flowsFwd) >= maxFlows {
			if !flowsFull {
				flowsFull = true
				fmt.Printf("HOPOS_MASQ_FULL: conntrack vol (%d) — nieuwe uitgaande flows gedropt\n", maxFlows)
			}
			return nil
		}
	}
	flowsFull = false
	np, ok := allocPort(proto, dstIP, dstPort)
	if !ok {
		return nil
	}
	fl := &flow{proto: proto, slot: slot, slotIP: slotIP, slotPort: slotPort,
		dstIP: dstIP, dstPort: dstPort, nodePort: np}
	flowsFwd[k] = fl
	flowsRev[rkey{proto, np, dstIP, dstPort}] = fl
	return fl
}

// allocPort kiest een vrij node-poortnummer voor een nieuwe flow: rollend door
// [masqBase, masqEnd), en het mag niet botsen met een lopende flow naar dezelfde
// peer, noch met een gepubliceerde poort (die is voor DNAT gereserveerd).
func allocPort(proto byte, dstIP uint32, dstPort uint16) (uint16, bool) {
	for range MasqEnd - MasqBase {
		p := masqNext
		if masqNext++; masqNext >= MasqEnd {
			masqNext = MasqBase
		}
		if _, busy := flowsRev[rkey{proto, p, dstIP, dstPort}]; busy {
			continue
		}
		if publishedLocked(proto, p) {
			continue
		}
		return p, true
	}
	return 0, false
}

// publishedLocked meldt of node-poort p (proto) een gepubliceerde poort is.
func publishedLocked(proto byte, p uint16) bool {
	for _, e := range pubs {
		if e.proto == proto && e.nodePort == p {
			return true
		}
	}
	return false
}

// sweepExpired verwijdert flows die langer dan hun idle-timeout stil waren.
func sweepExpired() {
	now := time.Now()
	for k, fl := range flowsFwd {
		idle := udpIdle
		if fl.proto == protoTCP {
			idle = tcpIdle
		}
		if now.Sub(fl.seen) > idle {
			delete(flowsRev, rkey{fl.proto, fl.nodePort, fl.dstIP, fl.dstPort})
			delete(flowsFwd, k)
		}
	}
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
	// RFC 768: een berekende UDP-checksum van 0x0000 betekent "geen checksum"
	// en moet als 0xFFFF verzonden worden. De incrementele update kan op 0
	// uitkomen; corrigeer dat (TCP en IP mogen 0x0000 wél houden).
	if proto == protoUDP && binary.BigEndian.Uint16(l4[csumOff:]) == 0 {
		binary.BigEndian.PutUint16(l4[csumOff:], 0xFFFF)
	}
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
