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
//   - Hairpin (Dereks model 27-07: poorten publiceren áltijd naar buiten, DNS
//     kiest de host, en is dat je eigen node dan is de switch de sluiproute):
//     een app die het node-IP belt op een gepubliceerde poort wordt intern
//     omgelegd — DNAT + masquerade in één (de twee tabellen hierboven, geen
//     nieuwe state) en ring-in bezorgd; er gaat geen byte de NIC uit.
//     hairpinOutLocked (heen) en hairpinBackLocked (reply).
//
// De L2-next-hop (dst-MAC de NIC op) komt uit een neighbor-cache die passief
// leert uit inbound frames: srcIP→srcMAC, en een frame van búíten ons subnet
// is via de gateway gerelayed → dat is de gateway-MAC (de fallback voor
// off-subnet bestemmingen; een on-subnet host die nog nooit sprak wordt
// actief ge-ARP't — zie l2For en arpForLocked). HOP's eigen boot-verkeer
// (SNTP off-subnet, DNS on-subnet) vult beide vóór de eerste app draait.
//
// Bewust niet gedekt (KISS, pas bij behoefte): het node-IP van binnenuit voor
// níet-gepubliceerde poorten (node-diensten als de agent wonen op 10.100.0.1)
// en een bestemming op HOP's eigen subnet die HOP zelf nog nooit sprak (geen
// neighbor → drop, de retransmit leert 'm).
package hopswitch

import (
	"encoding/binary"
	"fmt"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/xinix00/HopOS/metal/net/netdev"

	"github.com/xinix00/HopOS/metal/abi/layout"
)

const (
	ethLen   = 14
	etIPv4   = 0x0800
	protoTCP = 6
	protoUDP = 17

	// TCP-vlagbits: natFromSlot onderscheidt een kale SYN (uitgaand dialen)
	// van een antwoord op een gepubliceerde poort.
	tcpFlagSYN = 0x02
	tcpFlagACK = 0x10

	// Masquerade-poortbereik (PAT) en conntrack-grenzen. MasqBase/MasqEnd is
	// bewust disjunct van het efemere bereik van HOP's eigen externe stack —
	// lneto deelt zijn efemere poorten sequentieel uit in [49152, 65535]
	// (xnet stack-go), dus de masq stopt op 49152. Anders zou een inbound
	// antwoord op HOP's eigen DNS/S3-poort
	// per ongeluk een masquerade-flow naar dezelfde peer kunnen matchen.
	// 29k poorten blijft ruim boven maxFlows. Het plafond maxFlows is de
	// anti-DoS-grens (zoals bij de neighbor-cache): een app kan HOP's heap op
	// core 0 nooit laten vollopen. Idle-timeouts ruimen dode flows op — geen
	// TCP-toestand (FIN/RST) volgen, alleen inactiviteit; keepalives van een
	// langlopende tunnel (cloudflared ~30-90s) blijven ruim binnen tcpIdle.
	MasqBase = 20000
	MasqEnd  = 49152
	maxFlows = 4096
	tcpIdle  = 300 * time.Second
	udpIdle  = 60 * time.Second

	// maxFlowsPerSlot is het eerlijke deel per slot ónder het globale plafond.
	// Zonder dit is de conntrack één gedeelde pot: één app die 4096 uitgaande
	// verbindingen opent (of dat per ongeluk doet in een lus) laat élke buur
	// geen nieuwe verbinding meer maken — geen crash, maar wel de buren de
	// schuld van jouw gedrag. Met een quotum raakt de dader zijn eigen plafond
	// en blijven de anderen ongemoeid. Ruim gekozen: een normale app-workload
	// zit hier ver onder, en 128 slots × 512 overschrijdt het globale plafond
	// bewust (het is een eerlijkheidsgrens, geen reservering).
	maxFlowsPerSlot = 512

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

	neigh   = map[uint32]neighbor{} // IP → L2-next-hop (passief geleerd, verloopt)
	gwMAC   [6]byte                 // gateway-MAC (van off-subnet inbound)
	gwKnown bool

	flowsFwd  = map[fkey]*flow{}
	flowsRev  = map[rkey]*flow{}
	masqNext  = uint16(MasqBase)
	flowsFull bool // eenmalig loggen bij een volle pool
)

// Uplink omhult de externe NIC: inkomende frames voor gepubliceerde poorten of
// masquerade-antwoorden worden vóór de node-stack afgevangen (→ interne
// switch); de zendkant krijgt een mutex omdat de stack én de NAT er allebei op
// zenden (de NIC-Transmit is zelf niet goroutine-veilig).
type Uplink struct {
	nic  netdev.Device
	ip   uint32
	mask uint32
	mac  [6]byte
}

// uplinkTxMu serialiseert de zendkant: de node-stack (hopnet) én de NAT zenden
// allebei op de externe NIC, en NIC-Transmit is niet goroutine-veilig.
var uplinkTxMu sync.Mutex

// WrapUplink registreert de externe NIC bij de NAT en geeft de wrapper terug
// die hopnet in zijn go-net-Interface hangt. cidr is het externe node-adres
// mét prefix (bv. "10.0.2.15/24") — het masker bepaalt wat "off-subnet" is.
// Native app-IPv6 over deze uplink eist daarnaast dat de NIC alle 33:33-
// multicast en unicast voor de per-slot-MAC's 02:00:00:00:00:XX ontvangt.
// Alleen de LicheeRV/DWMAC1000 configureert dat filtercontract nu; op de andere
// boards blijft extern app-IPv6 buiten scope tot hun NIC-filter hetzelfde
// contract expliciet draagt en de hardwaregate slaagt.
func WrapUplink(nic netdev.Device, cidr string, mac net.HardwareAddr) (*Uplink, error) {
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
		// IP-multicast van het LAN (mDNS, matter, NDP): flood naar álle slots
		// en claim — HOP's eigen stack joint geen groepen en zou hem toch stil
		// negeren. 01:00:5e (IPv4) en 33:33 (IPv6); de slot-stacks filteren
		// zelf op lidmaatschap (leannet), dus dit is een kopie per aangesloten
		// slot en verder niets.
		if n >= 14 && ((buf[0] == 0x01 && buf[1] == 0x00 && buf[2] == 0x5e) ||
			(buf[0] == 0x33 && buf[1] == 0x33)) {
			multicastInbound(buf[:n])
			if claimed%claimYield == claimYield-1 {
				runtime.Gosched()
			}
			continue
		}
		// Unicast op een slot-MAC (02:00:00:00:00:XX): de terugweg van het
		// IPv6-L2-pad — een matter-apparaat of border router antwoordt de
		// slot rechtstreeks op de MAC die NDP adverteerde. Bezorgen en
		// claimen; de NIC staat hiervoor promiscuous (zie de driver).
		if n >= 14 && buf[12] == 0x86 && buf[13] == 0xdd &&
			buf[0] == 0x02 && buf[1]|buf[2]|buf[3]|buf[4] == 0 &&
			int(buf[5]) >= 1 && int(buf[5]) <= layout.MaxSlots {
			mu.Lock()
			deliverLocked(int(buf[5]), buf[:n])
			mu.Unlock()
			if claimed%claimYield == claimYield-1 {
				runtime.Gosched()
			}
			continue
		}
		// ARP eerst (niet claimen — de stack wil replies óók zien voor de eigen
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

// multicastInbound floodt één IPv4-multicast-frame van het LAN naar alle
// aangesloten slots (onder mu, zoals élke RX-ring-write). Geen aflevering
// terug de NIC op: ingress floodt alleen naar binnen.
func multicastInbound(p []byte) {
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i <= layout.MaxSlots; i++ {
		deliverLocked(i, p)
	}
}

// uplinkMulticastTx zet één slot-multicast-frame op de externe NIC (aanroepen
// met mu vast, vanuit forward — dezelfde volgorde als het NAT-zendpad; de
// omgekeerde lock-volgorde bestaat niet, dus mu→uplinkTxMu is veilig).
func uplinkMulticastTx(p []byte) {
	if uplink == nil {
		return
	}
	uplinkTxMu.Lock()
	defer uplinkTxMu.Unlock()
	uplink.nic.Transmit(p)
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
	if e := pubByNodePortLocked(p, nodePort); e != nil {
		return fmt.Errorf("publish: %s/%d al gepubliceerd (slot %d)", proto, nodePort, e.slot)
	}
	pubs = append(pubs, pub{proto: p, nodePort: nodePort, slot: slot, slotPort: slotPort})
	return nil
}

// pubByNodePortLocked vindt de publicatie op (proto, nodePort), of nil (mu
// vast) — dé lookup van elk pad dat een node-poort binnenkrijgt.
func pubByNodePortLocked(proto byte, nodePort uint16) *pub {
	for j := range pubs {
		if pubs[j].proto == proto && pubs[j].nodePort == nodePort {
			return &pubs[j]
		}
	}
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

// neighbor is één passief geleerde L2-next-hop. seen is de laatste keer dat de
// host het zélf bewees (een inbound frame); uitgaand gebruik telt bewust niet —
// naar een MAC zenden bewijst niet dat er iemand luistert.
type neighbor struct {
	mac  [6]byte
	seen time.Time
}

// neighTTL: hoe lang een geleerde neighbor geldt zonder nieuw levensteken.
// Dezelfde 120s als leannets eigen neighbor-tabel, om dezelfde reden: een
// wifi-host die slaapt of naar een ander mesh-punt zwerft wisselt van L2-pad,
// en een cache zonder veroudering maakt hem dan PERMANENT onbereikbaar voor
// uitgaand verkeer — gemeten 20-08 met de Brother op .201: dials stierven stil
// (SYN naar het oude pad) terwijl de Mac, met normale ARP-veroudering, hem
// gewoon kon pingen; printer uit-aan (gratuitous ARP) heelde het — precies de
// vingerafdruk van een verweesd MAC. Verlopen is niet erg: first-contact
// (ARP + alvast via de gateway) leert hem in één klap terug, en élk inbound
// frame ververst gratis via learnLocked.
const neighTTL = 120 * time.Second

// learnLocked leert de L2-next-hop uit een inbound frame (mu vast): srcIP →
// srcMAC, en een off-subnet bron betekent dat srcMAC de gateway is. Het
// gateway-paar veroudert bewust niet: élk pakket van buiten ververst het, en
// een router die stil van MAC wisselt zonder ooit nog iets door te geven is
// geen toestand die een TTL kan redden.
func learnLocked(srcIP uint32, mac []byte) {
	if _, known := neigh[srcIP]; !known && len(neigh) >= maxNeigh {
		neigh = map[uint32]neighbor{} // plafond: legen en herleren
	}
	neigh[srcIP] = neighbor{mac: [6]byte(mac), seen: time.Now()}
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

// l2For geeft de dst-MAC om dstIP te bereiken (mu vast): on-subnet alleen een
// écht geleerde, nog vérse neighbor, off-subnet de gateway. Een host die we
// nooit zagen — of langer dan neighTTL niets van hoorden — is bewust NIET
// known: dan neemt natOutbound het first-contact-pad, ARP-request én het frame
// alvast via de gateway (die combinatie dekt zowel hosts die netjes ARP
// beantwoorden als ARP-dove wifi/mesh-gevallen). Vóór 20-08 gaf l2For hier
// stil het gateway-MAC als known terug, waardoor er nooit ge-ARP't werd — de
// wifi-Brother-jacht; de TTL is deel twee van diezelfde jacht (zie neighTTL).
func l2For(dstIP uint32) ([6]byte, bool) {
	if onSubnet(dstIP) {
		m, ok := neigh[dstIP]
		if ok && time.Since(m.seen) > neighTTL {
			delete(neigh, dstIP) // verlopen: eruit, first-contact leert opnieuw
			return [6]byte{}, false
		}
		return m.mac, ok
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
	// Niet aan het node-IP gericht → niet van ons (de HOP-stack mag hem hebben).
	// Eén keer hier, voor béíde takken.
	if binary.BigEndian.Uint32(ip[16:]) != uplink.ip {
		return false
	}
	if replyInLocked(f, ip, l4, proto) {
		return true
	}
	return dnatInLocked(f, ip, l4, proto)
}

// dnatToSlotLocked is de gedeelde staart van élk NAT-pad richting een app
// (reply-in, DNAT-in en de twee hairpin-helften): bestemming herschrijven naar
// het slot — dst-IP, IP-checksum, L4-dst-poort (+checksum) — dan de MAC's en
// bezorging in de ring van dat slot (mu vast).
func dnatToSlotLocked(slot int, f, ip, l4 []byte, proto byte, oldIP, newIP uint32, oldPort, newPort uint16) {
	binary.BigEndian.PutUint32(ip[16:], newIP)
	fixCsum32(ip[10:], oldIP, newIP)
	rewriteL4(l4, proto, 2, oldIP, newIP, oldPort, newPort)
	mac := layout.SlotMAC(slot)
	copy(f[0:6], mac[:])
	copy(f[6:12], hostMAC[:])
	deliverLocked(slot, f)
}

// snatSrcLocked herschrijft de bron van een frame naar het node-IP (SNAT):
// src-IP, IP-checksum en L4-src-poort (+checksum) — de gedeelde eerste helft
// van elk pad dat namens een slot naar buiten of naar een buur praat (mu vast).
func snatSrcLocked(ip, l4 []byte, proto byte, oldIP uint32, oldPort, newPort uint16) {
	binary.BigEndian.PutUint32(ip[12:], uplink.ip)
	fixCsum32(ip[10:], oldIP, uplink.ip)
	rewriteL4(l4, proto, 0, oldIP, uplink.ip, oldPort, newPort)
}

// txUplinkLocked zet de MAC's en stuurt het frame de externe NIC uit (mu vast).
func txUplinkLocked(f []byte, nextHop [6]byte) {
	copy(f[0:6], nextHop[:])
	copy(f[6:12], uplink.mac[:])
	uplink.Transmit(f)
}

// replyInLocked vertaalt een inbound antwoord op een masquerade-flow terug en
// legt het rechtstreeks in de slot-ring (deliverLocked, mu vast); true = geclaimd.
// Aanroeper (natInbound) toetste al dat het frame aan het node-IP gericht is.
func replyInLocked(f, ip, l4 []byte, proto byte) bool {
	peerIP := binary.BigEndian.Uint32(ip[12:])
	peerPort := binary.BigEndian.Uint16(l4[0:])
	nodePort := binary.BigEndian.Uint16(l4[2:])
	fl := flowsRev[rkey{proto, nodePort, peerIP, peerPort}]
	if fl == nil {
		return false
	}
	fl.seen = time.Now()
	dnatToSlotLocked(fl.slot, f, ip, l4, proto, uplink.ip, fl.slotIP, nodePort, fl.slotPort)
	return true
}

// dnatInLocked: DNAT van node-IP:nodePort → slot-IP:slotPort (mu vast;
// zelfde node-IP-contract als replyInLocked).
func dnatInLocked(f, ip, l4 []byte, proto byte) bool {
	dport := binary.BigEndian.Uint16(l4[2:])
	m := pubByNodePortLocked(proto, dport)
	if m == nil {
		return false
	}
	dnatToSlotLocked(m.slot, f, ip, l4, proto, uplink.ip, layout.SlotIP4(m.slot), dport, m.slotPort)
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
	// Een échte reply op een gepubliceerde poort begint nooit met een kale SYN:
	// dit pad is stateloos en matcht alleen (proto, slot, poort), dus een app die
	// zélf naar buiten dialt en daarbij toevallig zijn eigen listen-poort als
	// BRONpoort krijgt, werd hier als "antwoord" geclaimd en stil naar nodePort
	// ge-SNAT — een kapotte uitgaande verbinding zonder spoor. SYN-zonder-ACK
	// hoort bij masquerade (natOutbound), dus die geven we door.
	// (UDP kent geen handshake en blijft dus op de poort-match staan — daar is de
	// publicatie de bedoelde bestemming; KISS, zoals de rest van deze DNAT.)
	if proto == protoTCP && l4[13]&tcpFlagSYN != 0 && l4[13]&tcpFlagACK == 0 {
		return false
	}
	// Draagt de reply het node-IP als bestemming, dan was de client een
	// búúrslot (hairpinOutLocked) — een externe client heeft dat IP nooit
	// (masquerade-antwoorden komen via de uplink binnen, niet hierlangs).
	// Terugvertalen via de conntrack en ring-in, niet de NIC uit.
	if binary.BigEndian.Uint32(ip[16:]) == uplink.ip {
		return hairpinBackLocked(f, ip, l4, proto, m)
	}
	nextHop, known := l2For(binary.BigEndian.Uint32(ip[16:]))
	if !known {
		return true // next-hop onbekend: drop, de retransmit leert 'm
	}
	snatSrcLocked(ip, l4, proto, binary.BigEndian.Uint32(ip[12:]), sport, m.nodePort)
	txUplinkLocked(f, nextHop)
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

	// Het node-IP zelf: hairpin (intern omleggen), nooit de NIC uit — vóór
	// l2For, anders zou first-contact een ARP-request naar ons eigen IP sturen.
	if dstIP == uplink.ip {
		return hairpinOutLocked(src, f, ip, l4, proto, slotIP, sport, dport)
	}

	nextHop, known := l2For(dstIP)
	if !known {
		// First-contact (Altra 14-07): een on-subnet bestemming die ons nooit
		// eerder iets stuurde is onbekend — passief leren komt dan nooit. Vraag
		// het net (ARP-request, rate-limited); de reply leert de neighbor
		// (arpLearn) en de TCP-retransmit van de app vindt 'm daarna.
		arpForLocked(dstIP)
		if !gwKnown {
			return true // geen enkel spoor: drop, retransmit volgt
		}
		// En stuur het frame alvast via de gateway mee (Brother-jacht 20-08):
		// er bestaan hosts die broadcast-ARP niet (betrouwbaar) beantwoorden —
		// wifi-powersave, mesh-punten die MAC's herschrijven. Relayt de router
		// het frame, dan antwoordt de host óns rechtstreeks (zelfde subnet) en
		// draagt dat antwoord zijn echte MAC: één pakket via de omweg en de
		// neighbor is alsnog geleerd. Relayt de router niet, dan is dit
		// hetzelfde als de drop van hierboven — de retransmit probeert het
		// opnieuw, tot ARP of router er één doorlaat.
		nextHop = gwMAC
	}
	fl := flowFor(proto, src, slotIP, sport, dstIP, dport)
	if fl == nil {
		return true // pool vol: drop
	}
	fl.seen = time.Now()
	snatSrcLocked(ip, l4, proto, slotIP, sport, fl.nodePort)
	txUplinkLocked(f, nextHop)
	return true
}

// hairpinOutLocked (mu vast, vanuit natOutbound): een app belt het node-IP —
// de gepubliceerde poort van een buurslot. DNAT en masquerade in één beweging:
// dst node-IP:nodePort → slot-IP:slotPort (de publicatie) en src
// slot-IP:poort → node-IP:masq-poort (de conntrack), dan ring-in bij de
// dienst. Door de bron te masqueraden ziet de dienst een gewone externe
// client en vindt zijn reply via hairpinBackLocked de weg terug — mét de
// 4-tupel die de beller verwacht (hij belde het node-IP, dus het antwoord
// moet dáárvandaan lijken te komen). Niets gepubliceerd op die poort = drop,
// zoals een dichte poort (de dialer merkt een timeout; node-diensten zoals
// de agent wonen binnen op 10.100.0.1, niet hier).
func hairpinOutLocked(src int, f, ip, l4 []byte, proto byte, slotIP uint32, sport, dport uint16) bool {
	m := pubByNodePortLocked(proto, dport)
	if m == nil {
		return true
	}
	srvIP := layout.SlotIP4(m.slot)
	fl := flowFor(proto, src, slotIP, sport, srvIP, m.slotPort)
	if fl == nil {
		return true // pool vol: drop
	}
	fl.seen = time.Now()
	snatSrcLocked(ip, l4, proto, slotIP, sport, fl.nodePort)
	dnatToSlotLocked(m.slot, f, ip, l4, proto, uplink.ip, srvIP, dport, m.slotPort)
	return true
}

// hairpinBackLocked (mu vast, vanuit natFromSlot): de reply van de dienst op
// een hairpin-verbinding — bestemming node-IP:masq-poort. De conntrack wijst
// de beller aan; beide kanten terugschrijven (dst → beller-slot, src →
// node-IP:nodePort, het adres dat de beller belde) en ring-in bezorgen.
// Geen flow (verlopen, of een dienst die spontaan het node-IP belt vanaf
// zijn eigen publicatie-poort) = drop.
func hairpinBackLocked(f, ip, l4 []byte, proto byte, m *pub) bool {
	srvIP := layout.SlotIP4(m.slot)
	np := binary.BigEndian.Uint16(l4[2:])
	fl := flowsRev[rkey{proto, np, srvIP, m.slotPort}]
	if fl == nil {
		return true
	}
	fl.seen = time.Now()
	// Eerst de src-helft, dan de dst-helft (die ook bezorgt): beide raken
	// disjuncte velden en de checksum-fixes zijn incrementeel, dus de volgorde
	// is vrij — deze laat de gedeelde staart het frame afleveren.
	snatSrcLocked(ip, l4, proto, srvIP, m.slotPort, m.nodePort)
	dnatToSlotLocked(fl.slot, f, ip, l4, proto, uplink.ip, fl.slotIP, np, fl.slotPort)
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
	// Eerlijk deel per slot (zie maxFlowsPerSlot): eerst vegen, dan pas de dader
	// afwijzen — een slot dat alleen verlopen flows had, mag gewoon door.
	if flowsForSlot(slot) >= maxFlowsPerSlot {
		sweepExpired()
		if n := flowsForSlot(slot); n >= maxFlowsPerSlot {
			fmt.Printf("HOPOS_MASQ_SLOT_FULL: slot %d has %d flows (max %d) — new outbound flow dropped\n", slot, n, maxFlowsPerSlot)
			return nil
		}
	}
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

// flowsForSlot telt de lopende flows van één slot (mu vast). Een lineaire telling
// over ≤ maxFlows entries, alleen op het pad "nieuwe flow" — geen aparte teller
// die uit de pas kan lopen met flowsFwd (die twee synchroon houden over sweep,
// UnpublishSlot en dood-detectie is precies waar zulke bugs zitten).
func flowsForSlot(slot int) int {
	n := 0
	for _, fl := range flowsFwd {
		if fl.slot == slot {
			n++
		}
	}
	return n
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
	return pubByNodePortLocked(proto, p) != nil
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
