// Host-tests voor de NAT: checksums (RFC 1624 incrementeel vs. volledige
// herberekening), frame-validatie, conntrack-lifecycle en de vier
// herschrijfpaden. De switch-lus draait hier niet; paden die "mu vast"
// eisen worden onder mu aangeroepen, geïnjecteerde frames landen op het
// inject-kanaal.
package hopswitch

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
	"time"

	"hop-os/metal/abi/layout"
)

// resetNAT zet alle package-state terug (de tests delen één proces).
func resetNAT() {
	mu.Lock()
	defer mu.Unlock()
	pubs = nil
	uplink = nil
	neigh = map[uint32][6]byte{}
	gwMAC = [6]byte{}
	gwKnown = false
	flowsFwd = map[fkey]*flow{}
	flowsRev = map[rkey]*flow{}
	masqNext = uint16(MasqBase)
	flowsFull = false
	arpLast = map[uint32]time.Time{}
	inject = make(chan []byte, 256)
}

type fakeNIC struct{ sent [][]byte }

func (n *fakeNIC) Transmit(b []byte) error {
	n.sent = append(n.sent, append([]byte(nil), b...))
	return nil
}
func (n *fakeNIC) Receive(buf []byte) (int, error) { return 0, nil }

const (
	nodeIP = uint32(0x0A00020F) // 10.0.2.15/24
	extIP  = uint32(0x5DB8D822) // 93.184.216.34 (off-subnet)
	lanIP  = uint32(0x0A000263) // 10.0.2.99 (on-subnet)
)

var (
	gwMAC0  = [6]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x01}
	lanMAC0 = [6]byte{0x66, 0x77, 0x88, 0x99, 0xAA, 0xBB}
	nicMAC  = [6]byte{0x52, 0x54, 0x00, 0x12, 0x34, 0x56}
)

func setUplink(t *testing.T) *fakeNIC {
	t.Helper()
	nic := &fakeNIC{}
	if _, err := WrapUplink(nic, "10.0.2.15/24", nicMAC[:]); err != nil {
		t.Fatalf("WrapUplink: %v", err)
	}
	return nic
}

// fold16 vouwt een 32-bit som naar de 16-bit one's-complement-som.
func fold16(sum uint32) uint16 {
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return uint16(sum)
}

func sumWords(b []byte) uint32 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		s += uint32(b[len(b)-1]) << 8
	}
	return s
}

// ipValid is de ontvanger-check: de som over de héle IP-header (checksum
// meegeteld) vouwt naar 0xFFFF. Accepteert beide one's-complement-
// representanten (0x0000/0xFFFF) — precies wat echte peers doen.
func ipValid(ip []byte) bool {
	ihl := int(ip[0]&0xf) * 4
	return fold16(sumWords(ip[:ihl])) == 0xFFFF
}

// l4Valid: idem voor TCP/UDP inclusief pseudo-header. UDP-checksum 0 = "geen".
func l4Valid(ip []byte) bool {
	ihl := int(ip[0]&0xf) * 4
	proto := ip[9]
	l4 := ip[ihl:binary.BigEndian.Uint16(ip[2:])]
	if proto == protoUDP && binary.BigEndian.Uint16(l4[6:]) == 0 {
		return true
	}
	sum := sumWords(ip[12:20]) + uint32(proto) + uint32(len(l4)) + sumWords(l4)
	return fold16(sum) == 0xFFFF
}

// mkFrame bouwt een geldig Ethernet+IPv4+TCP/UDP-frame met kloppende checksums.
func mkFrame(proto byte, dstMAC, srcMAC [6]byte, srcIP, dstIP uint32, sport, dport uint16, payload []byte) []byte {
	l4Len := 20
	if proto == protoUDP {
		l4Len = 8
	}
	f := make([]byte, ethLen+20+l4Len+len(payload))
	copy(f[0:6], dstMAC[:])
	copy(f[6:12], srcMAC[:])
	binary.BigEndian.PutUint16(f[12:], etIPv4)
	ip := f[ethLen:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(20+l4Len+len(payload)))
	ip[8] = 64
	ip[9] = proto
	binary.BigEndian.PutUint32(ip[12:], srcIP)
	binary.BigEndian.PutUint32(ip[16:], dstIP)
	binary.BigEndian.PutUint16(ip[10:], ^fold16(sumWords(ip[:20])))
	l4 := ip[20:]
	binary.BigEndian.PutUint16(l4[0:], sport)
	binary.BigEndian.PutUint16(l4[2:], dport)
	csumOff := 16
	if proto == protoTCP {
		l4[12] = 5 << 4 // data-offset: 20 bytes
	} else {
		binary.BigEndian.PutUint16(l4[4:], uint16(l4Len+len(payload)))
		csumOff = 6
	}
	copy(l4[l4Len:], payload)
	sum := sumWords(ip[12:20]) + uint32(proto) + uint32(len(l4)) + sumWords(l4)
	c := ^fold16(sum)
	if proto == protoUDP && c == 0 {
		c = 0xFFFF
	}
	binary.BigEndian.PutUint16(l4[csumOff:], c)
	return f
}

func checkFrame(t *testing.T, f []byte, wat string) {
	t.Helper()
	ip := f[ethLen:]
	if !ipValid(ip) {
		t.Fatalf("%s: IP-checksum klopt niet na herschrijven", wat)
	}
	if !l4Valid(ip) {
		t.Fatalf("%s: L4-checksum klopt niet na herschrijven", wat)
	}
}

// De incrementele checksum-update (RFC 1624) moet voor élke uitgangssituatie
// hetzelfde opleveren als volledig herrekenen — de ontvanger-check blijft waar.
func TestFixCsumTegenHerberekening(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	for i := 0; i < 5000; i++ {
		h := make([]byte, 20)
		rnd.Read(h)
		h[0] = 0x45
		binary.BigEndian.PutUint16(h[10:], 0)
		binary.BigEndian.PutUint16(h[10:], ^fold16(sumWords(h)))
		old := binary.BigEndian.Uint32(h[12:])
		nw := rnd.Uint32()
		if i%17 == 0 {
			nw = old // ongewijzigd woord mag de som niet breken
		}
		binary.BigEndian.PutUint32(h[12:], nw)
		fixCsum32(h[10:], old, nw)
		if !ipValid(h) {
			t.Fatalf("iteratie %d: header ongeldig na fixCsum32(%#x→%#x), csum=%#x",
				i, old, nw, binary.BigEndian.Uint16(h[10:]))
		}
	}
}

// RFC 768: een incrementele update die op 0x0000 uitkomt moet bij UDP als
// 0xFFFF de lijn op ("geen checksum" is gereserveerd voor letterlijk 0).
func TestRewriteL4UDPNulWordtFFFF(t *testing.T) {
	l4 := make([]byte, 8)
	binary.BigEndian.PutUint16(l4[0:], 5555)
	binary.BigEndian.PutUint16(l4[6:], 0xFFFF) // update met m==m' landt op 0x0000
	rewriteL4(l4, protoUDP, 0, nodeIP, nodeIP, 5555, 5555)
	if got := binary.BigEndian.Uint16(l4[6:]); got != 0xFFFF {
		t.Fatalf("UDP-checksum werd %#04x, verwacht 0xFFFF", got)
	}
}

// UDP zonder checksum (0) blijft zonder checksum — niet "gerepareerd".
func TestRewriteL4UDPNulBlijftNul(t *testing.T) {
	l4 := make([]byte, 8)
	binary.BigEndian.PutUint16(l4[0:], 5555)
	rewriteL4(l4, protoUDP, 0, 0x0A640002, nodeIP, 5555, 20001)
	if got := binary.BigEndian.Uint16(l4[6:]); got != 0 {
		t.Fatalf("UDP-checksum 0 werd %#04x", got)
	}
	if got := binary.BigEndian.Uint16(l4[0:]); got != 20001 {
		t.Fatalf("poort niet herschreven: %d", got)
	}
}

func TestIpv4L4Validatie(t *testing.T) {
	valid := mkFrame(protoTCP, gwMAC0, lanMAC0, extIP, nodeIP, 443, 5555, nil)
	cases := []struct {
		naam string
		mut  func(f []byte) []byte
		ok   bool
	}{
		{"geldig TCP", func(f []byte) []byte { return f }, true},
		{"te kort", func(f []byte) []byte { return f[:ethLen+10] }, false},
		{"geen IPv4-ethertype", func(f []byte) []byte { binary.BigEndian.PutUint16(f[12:], 0x0806); return f }, false},
		{"IPv6-versie", func(f []byte) []byte { f[ethLen] = 0x65; return f }, false},
		{"ihl te klein", func(f []byte) []byte { f[ethLen] = 0x44; return f }, false},
		{"fragment", func(f []byte) []byte { binary.BigEndian.PutUint16(f[ethLen+6:], 0x00B9); return f }, false},
		{"ICMP", func(f []byte) []byte { f[ethLen+9] = 1; return f }, false},
		{"TCP-header afgekapt", func(f []byte) []byte { return f[:ethLen+20+12] }, false},
	}
	for _, c := range cases {
		f := c.mut(append([]byte(nil), valid...))
		if _, _, ok := ipv4L4(f); ok != c.ok {
			t.Errorf("%s: ok=%v, verwacht %v", c.naam, ok, c.ok)
		}
	}
	// UDP met precies een 8-byte header is genoeg; ihl met opties telt door.
	u := mkFrame(protoUDP, gwMAC0, lanMAC0, extIP, nodeIP, 53, 5555, nil)
	if ihl, proto, ok := ipv4L4(u); !ok || ihl != 20 || proto != protoUDP {
		t.Errorf("UDP: ihl=%d proto=%d ok=%v", ihl, proto, ok)
	}
}

func TestPublishValidatie(t *testing.T) {
	resetNAT()
	if err := Publish("icmp", 80, 1, 80); err == nil {
		t.Error("proto icmp geaccepteerd")
	}
	if err := Publish("tcp", 80, 0, 80); err == nil {
		t.Error("slot 0 geaccepteerd")
	}
	if err := Publish("tcp", 80, layout.MaxSlots+1, 80); err == nil {
		t.Error("slot buiten bereik geaccepteerd")
	}
	if err := Publish("tcp", 0, 1, 80); err == nil {
		t.Error("poort 0 geaccepteerd")
	}
	if err := Publish("tcp", 80, 1, 80); err != nil {
		t.Fatalf("geldige publicatie geweigerd: %v", err)
	}
	if err := Publish("tcp", 80, 2, 80); err == nil {
		t.Error("dubbele tcp/80 geaccepteerd")
	}
	if err := Publish("udp", 80, 2, 80); err != nil {
		t.Errorf("udp/80 naast tcp/80 geweigerd: %v", err)
	}
}

// leerGateway laat de NAT de gateway-MAC leren zoals in het echt: een inbound
// frame van een off-subnet bron (dat verder nergens op matcht).
func leerGateway(t *testing.T) {
	t.Helper()
	f := mkFrame(protoTCP, nicMAC, gwMAC0, extIP, nodeIP, 443, 16001, nil)
	if natInbound(f) {
		t.Fatal("leer-frame geclaimd terwijl er geen flow of publicatie is")
	}
	mu.Lock()
	defer mu.Unlock()
	if !gwKnown || gwMAC != gwMAC0 {
		t.Fatal("gateway-MAC niet geleerd uit off-subnet inbound")
	}
}

// Het volledige masquerade-pad: app dialt uit, antwoord komt terug.
func TestMasqueradeUitEnTerug(t *testing.T) {
	resetNAT()
	nic := setUplink(t)
	leerGateway(t)

	payload := []byte("GET / HTTP/1.1")
	slotIP := layout.SlotIP4(1) // 10.100.0.2
	out := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), slotIP, extIP, 5555, 443, payload)
	mu.Lock()
	claimed := natOutbound(1, out)
	mu.Unlock()
	if !claimed || len(nic.sent) != 1 {
		t.Fatalf("uitgaand: claimed=%v verzonden=%d", claimed, len(nic.sent))
	}
	sent := nic.sent[0]
	ip := sent[ethLen:]
	if got := binary.BigEndian.Uint32(ip[12:]); got != nodeIP {
		t.Fatalf("bron-IP niet gemasqueradeerd: %#x", got)
	}
	masqPort := binary.BigEndian.Uint16(ip[20:])
	if masqPort < MasqBase || masqPort >= MasqEnd {
		t.Fatalf("masq-poort %d buiten [%d,%d)", masqPort, MasqBase, MasqEnd)
	}
	if !bytes.Equal(sent[0:6], gwMAC0[:]) || !bytes.Equal(sent[6:12], nicMAC[:]) {
		t.Fatal("L2: dst hoort de gateway te zijn, src de NIC")
	}
	if !bytes.Equal(sent[len(sent)-len(payload):], payload) {
		t.Fatal("payload beschadigd")
	}
	checkFrame(t, sent, "uitgaand")

	// Zelfde 5-tupel opnieuw → zelfde flow, zelfde masq-poort.
	out2 := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), slotIP, extIP, 5555, 443, nil)
	mu.Lock()
	natOutbound(1, out2)
	mu.Unlock()
	if p := binary.BigEndian.Uint16(nic.sent[1][ethLen+20:]); p != masqPort {
		t.Fatalf("herhaald pakket kreeg poort %d i.p.v. %d", p, masqPort)
	}

	// Het antwoord: ext peer → node-IP:masqPort, moet geclaimd en terugvertaald.
	reply := mkFrame(protoTCP, nicMAC, gwMAC0, extIP, nodeIP, 443, masqPort, []byte("HTTP/1.1 200 OK"))
	if !natInbound(reply) {
		t.Fatal("antwoord op lopende flow niet geclaimd")
	}
	var inj []byte
	select {
	case inj = <-inject:
	default:
		t.Fatal("antwoord niet geïnjecteerd")
	}
	iip := inj[ethLen:]
	if got := binary.BigEndian.Uint32(iip[16:]); got != slotIP {
		t.Fatalf("dst-IP niet terugvertaald: %#x", got)
	}
	if got := binary.BigEndian.Uint16(iip[22:]); got != 5555 {
		t.Fatalf("dst-poort niet terugvertaald: %d", got)
	}
	slotMAC := layout.SlotMAC(1)
	if !bytes.Equal(inj[0:6], slotMAC[:]) || !bytes.Equal(inj[6:12], hostMAC[:]) {
		t.Fatal("L2 van het geïnjecteerde frame klopt niet")
	}
	checkFrame(t, inj, "antwoord")
}

// Het DNAT-pad: gepubliceerde poort in, slot-antwoord uit (SNAT).
func TestDNATInEnSlotAntwoordUit(t *testing.T) {
	resetNAT()
	nic := setUplink(t)
	if err := Publish("tcp", 8080, 1, 9090); err != nil {
		t.Fatal(err)
	}

	in := mkFrame(protoTCP, nicMAC, lanMAC0, lanIP, nodeIP, 1234, 8080, []byte("hallo"))
	if !natInbound(in) {
		t.Fatal("inbound op gepubliceerde poort niet geclaimd")
	}
	inj := <-inject
	iip := inj[ethLen:]
	if got := binary.BigEndian.Uint32(iip[16:]); got != layout.SlotIP4(1) {
		t.Fatalf("DNAT dst-IP: %#x", got)
	}
	if got := binary.BigEndian.Uint16(iip[22:]); got != 9090 {
		t.Fatalf("DNAT dst-poort: %d", got)
	}
	checkFrame(t, inj, "DNAT-in")

	// Niet-gepubliceerde poort blijft voor de HOP-stack (niet geclaimd).
	los := mkFrame(protoTCP, nicMAC, lanMAC0, lanIP, nodeIP, 1234, 8081, nil)
	if natInbound(los) {
		t.Fatal("inbound op niet-gepubliceerde poort geclaimd")
	}

	// Slot-antwoord: SNAT terug naar node-IP:8080, dst-MAC = geleerde neighbor
	// (on-subnet peer), en de NIC uit.
	uit := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), lanIP, 9090, 1234, []byte("antwoord"))
	mu.Lock()
	claimed := natFromSlot(1, uit)
	mu.Unlock()
	if !claimed || len(nic.sent) != 1 {
		t.Fatalf("slot-antwoord: claimed=%v verzonden=%d", claimed, len(nic.sent))
	}
	sent := nic.sent[0]
	sip := sent[ethLen:]
	if got := binary.BigEndian.Uint32(sip[12:]); got != nodeIP {
		t.Fatalf("SNAT src-IP: %#x", got)
	}
	if got := binary.BigEndian.Uint16(sip[20:]); got != 8080 {
		t.Fatalf("SNAT src-poort: %d", got)
	}
	if !bytes.Equal(sent[0:6], lanMAC0[:]) {
		t.Fatal("dst-MAC hoort de geleerde on-subnet neighbor te zijn")
	}
	checkFrame(t, sent, "SNAT-uit")

	// Zonder matchende publicatie is het geen NAT-verkeer.
	vreemd := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), lanIP, 7777, 1234, nil)
	mu.Lock()
	claimed = natFromSlot(1, vreemd)
	mu.Unlock()
	if claimed {
		t.Fatal("slot-frame zonder publicatie geclaimd")
	}
}

func TestAllocPortSlaatBezetOver(t *testing.T) {
	resetNAT()
	setUplink(t)
	mu.Lock()
	defer mu.Unlock()
	// De eerstvolgende twee kandidaten bezet: één door een flow naar dezelfde
	// peer, één door een publicatie.
	flowsRev[rkey{protoTCP, MasqBase, extIP, 443}] = &flow{}
	pubs = append(pubs, pub{proto: protoTCP, nodePort: MasqBase + 1, slot: 1, slotPort: 80})
	p, ok := allocPort(protoTCP, extIP, 443)
	if !ok || p != MasqBase+2 {
		t.Fatalf("allocPort: %d ok=%v, verwacht %d", p, ok, MasqBase+2)
	}
	// Naar een ándere peer mag MasqBase gewoon (de rkey verschilt) — maar de
	// teller is al doorgeschoven, dus vraag alle poorten op tot hij rondgaat.
	for range MasqEnd - MasqBase {
		if _, ok := allocPort(protoTCP, extIP, 444); !ok {
			t.Fatal("allocPort raakte onterecht uitgeput")
		}
	}
}

func TestAllocPortUitputting(t *testing.T) {
	resetNAT()
	setUplink(t)
	mu.Lock()
	defer mu.Unlock()
	for p := uint16(MasqBase); p < MasqEnd; p++ {
		flowsRev[rkey{protoTCP, p, extIP, 443}] = &flow{}
	}
	if p, ok := allocPort(protoTCP, extIP, 443); ok {
		t.Fatalf("allocPort leverde %d terwijl alles bezet is", p)
	}
}

func TestSweepExpired(t *testing.T) {
	resetNAT()
	setUplink(t)
	mu.Lock()
	defer mu.Unlock()
	mk := func(proto byte, sport uint16, leeftijd time.Duration) {
		fl := flowFor(proto, 1, layout.SlotIP4(1), sport, extIP, 443)
		fl.seen = time.Now().Add(-leeftijd)
	}
	mk(protoTCP, 1001, tcpIdle+time.Second) // verlopen
	mk(protoTCP, 1002, tcpIdle-time.Second) // vers genoeg
	mk(protoUDP, 1003, udpIdle+time.Second) // verlopen (kortere timeout)
	mk(protoUDP, 1004, tcpIdle-time.Second) // ouder dan udpIdle → verlopen
	sweepExpired()
	if len(flowsFwd) != 1 || len(flowsRev) != 1 {
		t.Fatalf("na sweep: %d fwd / %d rev, verwacht 1/1", len(flowsFwd), len(flowsRev))
	}
	if flowsFwd[fkey{protoTCP, layout.SlotIP4(1), extIP, 1002, 443}] == nil {
		t.Fatal("de verse TCP-flow is weggeveegd")
	}
}

func TestUnpublishSlotRuimtOp(t *testing.T) {
	resetNAT()
	setUplink(t)
	Publish("tcp", 8080, 1, 8080)
	Publish("tcp", 8081, 2, 8081)
	mu.Lock()
	flowFor(protoTCP, 1, layout.SlotIP4(1), 1001, extIP, 443)
	flowFor(protoTCP, 2, layout.SlotIP4(2), 1002, extIP, 443)
	mu.Unlock()
	UnpublishSlot(1)
	mu.Lock()
	defer mu.Unlock()
	if len(pubs) != 1 || pubs[0].slot != 2 {
		t.Fatalf("publicaties na unpublish: %+v", pubs)
	}
	if len(flowsFwd) != 1 || len(flowsRev) != 1 {
		t.Fatalf("flows na unpublish: %d fwd / %d rev", len(flowsFwd), len(flowsRev))
	}
	for _, fl := range flowsFwd {
		if fl.slot != 2 {
			t.Fatalf("flow van slot %d overleefde", fl.slot)
		}
	}
}

func TestNeighborCacheEnPlafond(t *testing.T) {
	resetNAT()
	setUplink(t)
	mu.Lock()
	defer mu.Unlock()
	learnLocked(lanIP, lanMAC0[:])
	if m, ok := l2For(lanIP); !ok || m != lanMAC0 {
		t.Fatal("on-subnet neighbor niet geleerd")
	}
	learnLocked(extIP, gwMAC0[:]) // off-subnet ⇒ dit is de gateway
	if m, ok := l2For(extIP); !ok || m != gwMAC0 {
		t.Fatal("off-subnet bestemming hoort via de gateway te gaan")
	}
	if m, ok := l2For(nodeIP&^0xFF | 0x42); !ok || m != gwMAC0 {
		t.Fatal("onbekende on-subnet neighbor hoort op de gateway terug te vallen")
	}
	// Plafond: de cache loopt vol en wordt geleegd. De vuller-IP's zijn
	// off-subnet en schuiven dus (terecht) ook de gateway-MAC door; de
	// eigenschap die moet houden is dat de gateway-fallback ná de leging
	// blijft werken, met de laatst geleerde off-subnet-MAC.
	gw2 := [6]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x02}
	for i := uint32(0); len(neigh) < maxNeigh; i++ {
		learnLocked(0x0A000300+i, lanMAC0[:])
	}
	learnLocked(0x0B000001, gw2[:]) // onbekend IP op het plafond → leging
	if len(neigh) != 1 {
		t.Fatalf("cache na plafond: %d entries, verwacht 1", len(neigh))
	}
	if m, ok := l2For(extIP); !ok || m != gw2 {
		t.Fatal("gateway-fallback werkt niet meer na de cache-leging")
	}
}

// Zonder geleerde next-hop wordt uitgaand verkeer geclaimd maar gedropt
// (de retransmit leert 'm) — er mag níéts de NIC op.
func TestOutboundZonderNextHopDropt(t *testing.T) {
	resetNAT()
	nic := setUplink(t)
	f := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), extIP, 5555, 443, nil)
	mu.Lock()
	claimed := natOutbound(1, f)
	mu.Unlock()
	if !claimed {
		t.Fatal("hoort geclaimd (en gedropt) te zijn")
	}
	if len(nic.sent) != 0 {
		t.Fatal("frame verzonden zonder bekende next-hop")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(flowsFwd) != 0 {
		t.Fatal("drop hoort geen flow achter te laten")
	}
}

// First-contact naar een on-subnet host (Altra 14-07: de eerste loader-golf
// dropte al z'n SYNs — de Mac-mini was nooit geleerd en niemand vroeg het
// net): een onbekende on-subnet bestemming moet een ARP-request uitlokken,
// de reply leert de neighbor, en de retransmit gaat dan wél de deur uit.
func TestARPFirstContact(t *testing.T) {
	resetNAT()
	nic := setUplink(t)

	slotIP := layout.SlotIP4(1)
	syn := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), slotIP, lanIP, 5555, 8000, nil)
	mu.Lock()
	claimed := natOutbound(1, append([]byte(nil), syn...))
	mu.Unlock()
	if !claimed {
		t.Fatal("eerste SYN niet geclaimd (drop+ARP hoort het pad te zijn)")
	}
	if len(nic.sent) != 1 {
		t.Fatalf("verwacht 1 ARP-request op de uplink, kreeg %d frames", len(nic.sent))
	}
	arp := nic.sent[0]
	if arp[12] != 0x08 || arp[13] != 0x06 || !bytes.Equal(arp[0:6], []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) {
		t.Fatalf("geen broadcast-ARP: %x", arp[:14])
	}
	a := arp[14:]
	if binary.BigEndian.Uint16(a[6:]) != 1 || binary.BigEndian.Uint32(a[24:]) != lanIP {
		t.Fatal("ARP-request vraagt niet naar de bestemming")
	}
	if binary.BigEndian.Uint32(a[14:]) != nodeIP || !bytes.Equal(a[8:14], nicMAC[:]) {
		t.Fatal("ARP-request draagt niet ons eigen sender-paar")
	}

	// Rate-limit: een tweede SYN direct erna → géén tweede request.
	mu.Lock()
	natOutbound(1, append([]byte(nil), syn...))
	mu.Unlock()
	if len(nic.sent) != 1 {
		t.Fatalf("ARP-storm: %d requests binnen de rate-limit", len(nic.sent))
	}

	// De reply (zoals Receive hem aan arpLearn geeft) leert de neighbor.
	reply := make([]byte, 42)
	copy(reply[0:6], nicMAC[:])
	copy(reply[6:12], lanMAC0[:])
	reply[12], reply[13] = 0x08, 0x06
	r := reply[14:]
	r[0], r[1], r[2], r[3], r[4], r[5] = 0, 1, 0x08, 0, 6, 4
	r[7] = 2 // oper = reply
	copy(r[8:14], lanMAC0[:])
	binary.BigEndian.PutUint32(r[14:], lanIP)
	copy(r[18:24], nicMAC[:])
	binary.BigEndian.PutUint32(r[24:], nodeIP)
	arpLearn(reply)

	// Retransmit: nu wél verzonden, naar het geleerde MAC.
	mu.Lock()
	natOutbound(1, append([]byte(nil), syn...))
	mu.Unlock()
	if len(nic.sent) != 2 {
		t.Fatalf("retransmit niet verzonden (frames: %d)", len(nic.sent))
	}
	if !bytes.Equal(nic.sent[1][0:6], lanMAC0[:]) {
		t.Fatal("retransmit niet naar het geleerde MAC")
	}
}

// ARP-probes (spa 0.0.0.0, RFC 5227) en DHCP-broadcasts van buren (IPv4-src
// 0.0.0.0) mogen niets leren — zeker het gateway-MAC niet (kruimel #10:
// elk DHCP'end apparaat op het LAN werd anders even "de gateway").
func TestGeenPoisoningUitProbesEnDHCP(t *testing.T) {
	resetNAT()
	setUplink(t)
	leerGateway(t)
	rogue := [6]byte{0x02, 0xBA, 0xD0, 0x00, 0x00, 0x99}

	// ARP-probe: spa 0 → arpLearn negeert 'm volledig.
	probe := make([]byte, 42)
	copy(probe[6:12], rogue[:])
	probe[12], probe[13] = 0x08, 0x06
	p := probe[14:]
	p[0], p[1], p[2], p[3], p[4], p[5] = 0, 1, 0x08, 0, 6, 4
	p[7] = 1
	copy(p[8:14], rogue[:])
	binary.BigEndian.PutUint32(p[24:], nodeIP)
	arpLearn(probe)

	// DHCP-discover van een buurman: src-IP 0.0.0.0 → natInbound leert niks.
	dhcp := mkFrame(protoUDP, [6]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, rogue, 0, 0xFFFFFFFF, 68, 67, []byte{0x01})
	natInbound(dhcp)

	mu.Lock()
	defer mu.Unlock()
	if gwMAC != gwMAC0 {
		t.Fatalf("gateway-MAC vergiftigd: %x", gwMAC)
	}
	if _, ok := neigh[0]; ok {
		t.Fatal("IP 0.0.0.0 als neighbor geleerd")
	}
}
