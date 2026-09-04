// Host-tests voor de NAT: checksums (RFC 1624 incrementeel vs. volledige
// herberekening), frame-validatie, conntrack-lifecycle en de vier
// herschrijfpaden. De switch-lus draait hier niet; paden die "mu vast"
// eisen worden onder mu aangeroepen, bezorgde inbound frames landen via
// deliverLocked in een heap-gebackte slot-ring (testSlotRing).
package hopswitch

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"testing"
	"time"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/abi/ring"
)

// resetNAT zet alle package-state terug (de tests delen één proces).
func resetNAT() {
	mu.Lock()
	defer mu.Unlock()
	pubs = nil
	uplink = nil
	neigh = map[uint32]neighbor{}
	gwMAC = [6]byte{}
	gwKnown = false
	flowsFwd = map[fkey]*flow{}
	flowsRev = map[rkey]*flow{}
	masqNext = uint16(MasqBase)
	flowCountBySlot = [layout.SlotCap + 1]int{}
	flowMapHighWater = 0
	nextFlowSweep = time.Time{}
	nextFlowsFullLog = time.Time{}
	nextSlotFullLog = [layout.SlotCap + 1]time.Time{}
	arpLast = map[uint32]time.Time{}
	ports = nil // deliverLocked dropt dan (slot niet aangesloten)
}

// testSlotRing hangt een host-mmap als RX-ring aan slot i — het bezorgdoel van
// deliverLocked — en geeft een leesfunctie terug (nil = niets bezorgd). De mmap
// modelleert device-geheugen en wordt na het loskoppelen automatisch opgeruimd.
func testSlotRing(t *testing.T, i int) func() []byte {
	t.Helper()
	buf := testDeviceMemory(t, 512<<10)
	base := testDeviceAddress(buf)
	ring.Init(base, 256<<10)
	ring.Init(base+256<<10, 256<<10)
	mu.Lock()
	if len(ports) == 0 {
		ports = make([]*port, layout.MaxSlots+1)
	}
	// Ook een TX-ring: zonder loopt de switch-pas op nil en slikt zijn
	// recover dat — de test slaagde dan met een PANIC-regel (review 05-09).
	pt := &port{rx: ring.Open(base), tx: ring.Open(base + 256<<10)}
	ports[i] = pt
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		if i < len(ports) && ports[i] == pt {
			ports[i] = nil
		}
		mu.Unlock()
	})
	rd := ring.Open(base)
	out := make([]byte, 32<<10)
	return func() []byte {
		typ, n, ok := rd.ReadInto(out)
		if !ok || typ != ring.TypeFrame {
			return nil
		}
		return out[:n]
	}
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

// setTCPFlags wijzigt de vlaggen van een testframe en herberekent daarna de
// volledige TCP-checksum. mkFrame maakt standaard een vlagloos TCP-segment.
func setTCPFlags(f []byte, flags byte) {
	ip := f[ethLen:]
	ihl := int(ip[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(ip[2:]))
	l4 := ip[ihl:total]
	l4[13] = flags
	binary.BigEndian.PutUint16(l4[16:], 0)
	sum := sumWords(ip[12:20]) + uint32(protoTCP) + uint32(len(l4)) + sumWords(l4)
	binary.BigEndian.PutUint16(l4[16:], ^fold16(sum))
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
		{"eerste fragment met MF", func(f []byte) []byte { binary.BigEndian.PutUint16(f[ethLen+6:], 0x2000); return f }, false},
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

	// Het antwoord: ext peer → node-IP:masqPort, moet geclaimd en rechtstreeks
	// in de RX-ring van slot 1 bezorgd (deliverLocked).
	read := testSlotRing(t, 1)
	reply := mkFrame(protoTCP, nicMAC, gwMAC0, extIP, nodeIP, 443, masqPort, []byte("HTTP/1.1 200 OK"))
	if !natInbound(reply) {
		t.Fatal("antwoord op lopende flow niet geclaimd")
	}
	inj := read()
	if inj == nil {
		t.Fatal("antwoord niet in de slot-ring bezorgd")
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

	read := testSlotRing(t, 1)
	in := mkFrame(protoTCP, nicMAC, lanMAC0, lanIP, nodeIP, 1234, 8080, []byte("hallo"))
	if !natInbound(in) {
		t.Fatal("inbound op gepubliceerde poort niet geclaimd")
	}
	inj := read()
	if inj == nil {
		t.Fatal("DNAT-frame niet in de slot-ring bezorgd")
	}
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
	now := time.Now()
	mk := func(proto byte, sport uint16, leeftijd time.Duration) {
		fl := flowFor(proto, 1, layout.SlotIP4(1), sport, extIP, 443, now)
		fl.seen = now.Add(-leeftijd)
	}
	mk(protoTCP, 1001, tcpIdle+time.Second) // verlopen
	mk(protoTCP, 1002, tcpIdle-time.Second) // vers genoeg
	mk(protoUDP, 1003, udpIdle+time.Second) // verlopen (kortere timeout)
	mk(protoUDP, 1004, tcpIdle-time.Second) // ouder dan udpIdle → verlopen
	sweepExpiredAt(now)
	if len(flowsFwd) != 1 || len(flowsRev) != 1 {
		t.Fatalf("na sweep: %d fwd / %d rev, verwacht 1/1", len(flowsFwd), len(flowsRev))
	}
	if flowsFwd[fkey{protoTCP, layout.SlotIP4(1), extIP, 1002, 443}] == nil {
		t.Fatal("de verse TCP-flow is weggeveegd")
	}
}

func TestSweepCompacteertMapNaVerkeerspiek(t *testing.T) {
	resetNAT()
	setUplink(t)
	mu.Lock()
	defer mu.Unlock()

	now := time.Now()
	var keep *flow
	for i := 0; i < 128; i++ {
		fl := flowFor(protoTCP, 1, layout.SlotIP4(1), uint16(1000+i), extIP, 443, now)
		if i == 127 {
			fl.seen = now
			keep = fl
		} else {
			fl.seen = now.Add(-tcpIdle - time.Second)
		}
	}
	if flowMapHighWater != 128 {
		t.Fatalf("high-water vóór sweep=%d, wil 128", flowMapHighWater)
	}
	sweepExpiredAt(now)
	if len(flowsFwd) != 1 || len(flowsRev) != 1 || flowMapHighWater != 1 || flowCountBySlot[1] != 1 {
		t.Fatalf("na compactie: fwd=%d rev=%d high=%d count=%d, wil 1/1/1/1",
			len(flowsFwd), len(flowsRev), flowMapHighWater, flowCountBySlot[1])
	}
	if flowsRev[rkey{keep.proto, keep.nodePort, keep.dstIP, keep.dstPort}] != keep {
		t.Fatal("reverse mapping van de overlevende flow ging bij compactie verloren")
	}
}

func TestFlowLookupVervangtVerlopenEntry(t *testing.T) {
	resetNAT()
	setUplink(t)
	mu.Lock()
	defer mu.Unlock()
	now := time.Now()

	old := flowFor(protoUDP, 1, layout.SlotIP4(1), 1001, extIP, 443, now)
	if old == nil {
		t.Fatal("eerste flow niet aangemaakt")
	}
	oldPort := old.nodePort
	old.seen = now.Add(-udpIdle - time.Second)

	replacement := flowFor(protoUDP, 1, layout.SlotIP4(1), 1001, extIP, 443, now)
	if replacement == nil || replacement == old {
		t.Fatal("exacte lookup gaf een verlopen flow terug")
	}
	if replacement.nodePort == oldPort {
		t.Fatal("vervangende flow hergebruikte onverwacht dezelfde allocatie")
	}
	if flowsRev[rkey{protoUDP, oldPort, extIP, 443}] != nil {
		t.Fatal("reverse mapping van de verlopen flow bleef staan")
	}
	if len(flowsFwd) != 1 || len(flowsRev) != 1 || flowCountBySlot[1] != 1 {
		t.Fatalf("na vervanging: fwd=%d rev=%d count=%d, wil 1/1/1",
			len(flowsFwd), len(flowsRev), flowCountBySlot[1])
	}
}

// Een verlopen reverse match mag de node-poort niet blijven kapen. Nadat de
// stale conntrack is verwijderd moet hetzelfde pakket nog DNAT kunnen matchen.
func TestReverseLookupVerwijdertExpiredEnValtTerugOpDNAT(t *testing.T) {
	resetNAT()
	setUplink(t)
	mu.Lock()
	now := time.Now()
	fl := flowFor(protoTCP, 1, layout.SlotIP4(1), 1001, extIP, 443, now)
	fl.seen = now.Add(-tcpIdle - time.Second)
	nodePort := fl.nodePort
	mu.Unlock()

	if err := Publish("tcp", nodePort, 2, 8080); err != nil {
		t.Fatalf("Publish op oude masq-poort: %v", err)
	}
	read := testSlotRing(t, 2)
	in := mkFrame(protoTCP, nicMAC, gwMAC0, extIP, nodeIP, 443, nodePort, []byte("nieuw"))
	if !natInbound(in) {
		t.Fatal("pakket viel na stale reverse lookup niet terug op DNAT")
	}
	got := read()
	if got == nil {
		t.Fatal("DNAT-pakket niet bij het gepubliceerde slot bezorgd")
	}
	ip := got[ethLen:]
	if binary.BigEndian.Uint32(ip[16:]) != layout.SlotIP4(2) || binary.BigEndian.Uint16(ip[22:]) != 8080 {
		t.Fatalf("verkeerde DNAT-bestemming: %08x:%d",
			binary.BigEndian.Uint32(ip[16:]), binary.BigEndian.Uint16(ip[22:]))
	}
	checkFrame(t, got, "DNAT na stale reverse lookup")
	mu.Lock()
	defer mu.Unlock()
	if len(flowsFwd) != 0 || len(flowsRev) != 0 || flowCountBySlot[1] != 0 {
		t.Fatalf("stale flow niet volledig verwijderd: fwd=%d rev=%d count=%d",
			len(flowsFwd), len(flowsRev), flowCountBySlot[1])
	}
}

func TestTCPFINVerkortTimeoutPasNaBeideRichtingen(t *testing.T) {
	resetNAT()
	setUplink(t)
	mu.Lock()
	defer mu.Unlock()

	base := time.Unix(1_000, 0)
	fl := flowFor(protoTCP, 1, layout.SlotIP4(1), 1001, extIP, 443, base)
	l4 := make([]byte, 20)
	l4[13] = tcpFlagFIN
	noteTCPFlags(fl, l4, false)
	sweepExpiredAt(base.Add(tcpClosingIdle + time.Nanosecond))
	if len(flowsFwd) != 1 {
		t.Fatal("eenzijdige FIN ruimde een legitieme TCP-half-close op")
	}

	noteTCPFlags(fl, l4, true)
	sweepExpiredAt(base.Add(tcpClosingIdle + time.Nanosecond))
	if len(flowsFwd) != 0 || len(flowsRev) != 0 || flowCountBySlot[1] != 0 {
		t.Fatal("flow met FIN in beide richtingen overleefde de closing-timeout")
	}
}

func TestTCPRSTBezorgingEnVeiligeReclaim(t *testing.T) {
	t.Run("outbound zonder flow", func(t *testing.T) {
		resetNAT()
		nic := setUplink(t)
		leerGateway(t)
		before := masqNext
		rst := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), extIP, 5555, 443, nil)
		setTCPFlags(rst, tcpFlagRST|tcpFlagACK)
		mu.Lock()
		claimed := natOutbound(1, rst)
		mu.Unlock()
		if !claimed {
			t.Fatal("flow-loze outbound RST niet als NAT-verkeer afgehandeld")
		}
		if len(nic.sent) != 0 {
			t.Fatal("flow-loze RST kreeg zonder bekend node-poortnummer toch een vertaling")
		}
		mu.Lock()
		defer mu.Unlock()
		if len(flowsFwd) != 0 || len(flowsRev) != 0 || flowMapHighWater != 0 || masqNext != before {
			t.Fatalf("flow-loze RST veroorzaakte allocatiechurn: fwd=%d rev=%d high=%d masq=%d→%d",
				len(flowsFwd), len(flowsRev), flowMapHighWater, before, masqNext)
		}
	})

	t.Run("inbound", func(t *testing.T) {
		resetNAT()
		nic := setUplink(t)
		leerGateway(t)
		slotIP := layout.SlotIP4(1)
		out := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), slotIP, extIP, 5555, 443, nil)
		mu.Lock()
		natOutbound(1, out)
		fl := flowsFwd[fkey{protoTCP, slotIP, extIP, 5555, 443}]
		mu.Unlock()
		if fl == nil || len(nic.sent) != 1 {
			t.Fatal("voorbereidende TCP-flow ontbreekt")
		}

		read := testSlotRing(t, 1)
		rst := mkFrame(protoTCP, nicMAC, gwMAC0, extIP, nodeIP, 443, fl.nodePort, nil)
		setTCPFlags(rst, tcpFlagRST|tcpFlagACK)
		if !natInbound(rst) {
			t.Fatal("inbound RST niet geclaimd")
		}
		got := read()
		if got == nil || got[ethLen+20+13]&tcpFlagRST == 0 {
			t.Fatal("RST werd vóór aflevering weggegooid")
		}
		checkFrame(t, got, "inbound RST")
		mu.Lock()
		defer mu.Unlock()
		if len(flowsFwd) != 1 || len(flowsRev) != 1 || flowCountBySlot[1] != 1 {
			t.Fatal("inbound RST ruimde zonder TCP-sequencevalidatie onveilig vroeg op")
		}
	})

	t.Run("outbound", func(t *testing.T) {
		resetNAT()
		nic := setUplink(t)
		leerGateway(t)
		slotIP := layout.SlotIP4(1)
		first := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), slotIP, extIP, 5555, 443, nil)
		mu.Lock()
		natOutbound(1, first)
		mu.Unlock()
		rst := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), slotIP, extIP, 5555, 443, nil)
		setTCPFlags(rst, tcpFlagRST|tcpFlagACK)
		mu.Lock()
		natOutbound(1, rst)
		mu.Unlock()
		if len(nic.sent) != 2 || nic.sent[1][ethLen+20+13]&tcpFlagRST == 0 {
			t.Fatal("outbound RST werd niet verzonden")
		}
		checkFrame(t, nic.sent[1], "outbound RST")
		mu.Lock()
		defer mu.Unlock()
		if len(flowsFwd) != 0 || len(flowsRev) != 0 || flowCountBySlot[1] != 0 {
			t.Fatal("outbound RST gaf de conntrack-lease niet vrij")
		}
	})
}

func TestVolSlotRejectpadIsRateLimited(t *testing.T) {
	resetNAT()
	setUplink(t)
	mu.Lock()
	defer mu.Unlock()
	now := time.Now()

	var oldest *flow
	for i := 0; i < maxFlowsPerSlot; i++ {
		fl := flowFor(protoUDP, 1, layout.SlotIP4(1), uint16(1000+i), extIP, 443, now)
		if fl == nil {
			t.Fatalf("flow %d vóór het slotquotum geweigerd", i)
		}
		if i == 0 {
			oldest = fl
		}
	}
	if flowFor(protoUDP, 1, layout.SlotIP4(1), 60000, extIP, 443, now) != nil {
		t.Fatal("flow boven het slotquotum geaccepteerd")
	}
	firstSweep, firstLog := nextFlowSweep, nextSlotFullLog[1]
	if firstSweep.IsZero() || firstLog.IsZero() {
		t.Fatal("eerste reject zette sweep/log-cadans niet")
	}

	oldest.seen = now.Add(-udpIdle - time.Second)
	if flowFor(protoUDP, 1, layout.SlotIP4(1), 60001, extIP, 443, now) != nil {
		t.Fatal("tweede flow boven het slotquotum geaccepteerd")
	}
	if flowsFwd[fkey{protoUDP, layout.SlotIP4(1), extIP, 1000, 443}] == nil {
		t.Fatal("tweede reject scande opnieuw vóór de cadence")
	}
	if nextFlowSweep != firstSweep || nextSlotFullLog[1] != firstLog {
		t.Fatal("herhaalde reject schoof sweep- of logdeadline opnieuw door")
	}

	nextFlowSweep = time.Time{} // simuleer de volgende goedkope cadence
	if flowFor(protoUDP, 1, layout.SlotIP4(1), 60002, extIP, 443, now) == nil {
		t.Fatal("cadence-sweep maakte geen plek voor een nieuwe flow")
	}
	if len(flowsFwd) != maxFlowsPerSlot || len(flowsRev) != maxFlowsPerSlot || flowCountBySlot[1] != maxFlowsPerSlot {
		t.Fatalf("cardinaliteit na sweep/refill: fwd=%d rev=%d count=%d",
			len(flowsFwd), len(flowsRev), flowCountBySlot[1])
	}
}

func TestUnpublishSlotRuimtOp(t *testing.T) {
	resetNAT()
	setUplink(t)
	Publish("tcp", 8080, 1, 8080)
	Publish("tcp", 8081, 2, 8081)
	mu.Lock()
	now := time.Now()
	flowFor(protoTCP, 1, layout.SlotIP4(1), 1001, extIP, 443, now)
	flowFor(protoTCP, 2, layout.SlotIP4(2), 1002, extIP, 443, now)
	flowFor(protoTCP, 2, layout.SlotIP4(2), 1003, layout.SlotIP4(1), 8080, now) // hairpin naar slot 1
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
		if fl.slot != 2 || fl.dstIP == layout.SlotIP4(1) {
			t.Fatalf("verkeerde flow overleefde: slot=%d dst=%08x", fl.slot, fl.dstIP)
		}
	}
	if flowCountBySlot[1] != 0 || flowCountBySlot[2] != 1 {
		t.Fatalf("flowtellers na unpublish: slot1=%d slot2=%d", flowCountBySlot[1], flowCountBySlot[2])
	}
}

func TestUnpublishSlotCompacteertPublicatiepiek(t *testing.T) {
	resetNAT()
	for p := uint16(1000); p < 1128; p++ {
		if err := Publish("tcp", p, 1, p); err != nil {
			t.Fatalf("Publish tcp/%d: %v", p, err)
		}
	}
	if err := Publish("tcp", 9000, 2, 9000); err != nil {
		t.Fatalf("Publish overlever: %v", err)
	}
	if cap(pubs) < 64 {
		t.Fatalf("test bouwde geen publicatiepiek: cap=%d", cap(pubs))
	}
	UnpublishSlot(1)
	mu.Lock()
	defer mu.Unlock()
	if len(pubs) != 1 || pubs[0].slot != 2 {
		t.Fatalf("verkeerde publicaties over: %+v", pubs)
	}
	if cap(pubs) > 4 {
		t.Fatalf("backing-array van publicatiepiek bleef hangen: len=%d cap=%d", len(pubs), cap(pubs))
	}
}

func TestNeighborCacheEnPlafond(t *testing.T) {
	resetNAT()
	setUplink(t)
	mu.Lock()
	defer mu.Unlock()
	now := time.Now()
	learnLocked(lanIP, lanMAC0[:], now)
	if m, ok := l2For(lanIP, now); !ok || m != lanMAC0 {
		t.Fatal("on-subnet neighbor niet geleerd")
	}
	learnLocked(extIP, gwMAC0[:], now) // off-subnet ⇒ dit is de gateway
	if m, ok := l2For(extIP, now); !ok || m != gwMAC0 {
		t.Fatal("off-subnet bestemming hoort via de gateway te gaan")
	}
	if _, ok := l2For(nodeIP&^0xFF|0x42, now); ok {
		t.Fatal("onbekende on-subnet neighbor hoort NIET known te zijn: de gateway " +
			"stuurt een LAN-frame niet terug het LAN op — first-contact moet ARP'en")
	}
	// Plafond: de cache loopt vol en wordt geleegd. De vuller-IP's zijn
	// off-subnet en schuiven dus (terecht) ook de gateway-MAC door; de
	// eigenschap die moet houden is dat de gateway-fallback ná de leging
	// blijft werken, met de laatst geleerde off-subnet-MAC.
	gw2 := [6]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x02}
	for i := uint32(0); len(neigh) < maxNeigh; i++ {
		learnLocked(0x0A000300+i, lanMAC0[:], now)
	}
	learnLocked(0x0B000001, gw2[:], now) // onbekend IP op het plafond → leging
	if len(neigh) != 1 {
		t.Fatalf("cache na plafond: %d entries, verwacht 1", len(neigh))
	}
	if m, ok := l2For(extIP, now); !ok || m != gw2 {
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

	// De gateway is al bekend — zoals seconden na élke echte boot. Vóór 20-08
	// maakte precies dat het ARP-pad onbereikbaar: l2For viel voor een
	// onbekende on-subnet host op het gateway-MAC terug (known=true) en de
	// SYN verdween stil bij de router. De wifi-Brother-jacht van die dag.
	mu.Lock()
	learnLocked(extIP, gwMAC0[:], time.Now())
	mu.Unlock()

	slotIP := layout.SlotIP4(1)
	syn := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), slotIP, lanIP, 5555, 8000, nil)
	setTCPFlags(syn, tcpFlagSYN)
	mu.Lock()
	claimed := natOutbound(1, append([]byte(nil), syn...))
	mu.Unlock()
	if !claimed {
		t.Fatal("eerste SYN niet geclaimd (ARP + via-gateway hoort het pad te zijn)")
	}
	// Twee frames: het ARP-request én de SYN alvast via de gateway — de
	// fallback voor hosts die broadcast-ARP niet beantwoorden (Brother 20-08).
	if len(nic.sent) != 2 {
		t.Fatalf("verwacht ARP-request + SYN-via-gateway, kreeg %d frames", len(nic.sent))
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
	if !bytes.Equal(nic.sent[1][0:6], gwMAC0[:]) {
		t.Fatal("de meegestuurde SYN hoort via het gateway-MAC te gaan")
	}

	// Rate-limit: een tweede SYN direct erna → géén tweede ARP-request, wel
	// opnieuw de SYN via de gateway (retransmits blijven het proberen).
	mu.Lock()
	natOutbound(1, append([]byte(nil), syn...))
	mu.Unlock()
	if len(nic.sent) != 3 || nic.sent[2][12] == 0x08 && nic.sent[2][13] == 0x06 {
		t.Fatalf("verwacht 1 extra SYN-via-gateway zonder ARP-storm (frames: %d)", len(nic.sent))
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

	// Retransmit: nu rechtstreeks naar het geleerde MAC, niet meer via de
	// gateway.
	mu.Lock()
	natOutbound(1, append([]byte(nil), syn...))
	mu.Unlock()
	if len(nic.sent) != 4 {
		t.Fatalf("retransmit niet verzonden (frames: %d)", len(nic.sent))
	}
	if !bytes.Equal(nic.sent[3][0:6], lanMAC0[:]) {
		t.Fatal("retransmit niet naar het geleerde MAC")
	}
}

// Een verse maar verweesde neighbor-entry mag een dial niet tot neighTTL
// (120s) stilhouden. De eerste SYN is al de unicast-probe; ziet NAT dezelfde
// kale SYN nogmaals op dezelfde flow, dan heeft TCP zelf na zijn RTO de retry
// geleverd en voegen wij alleen broadcast-ARP toe. Geen tweede timer of
// volledige Linux-NUD-machine nodig.
func TestKnownNeighborSynRetryProbesARP(t *testing.T) {
	resetNAT()
	nic := setUplink(t)
	mu.Lock()
	learnLocked(lanIP, lanMAC0[:], time.Now())
	mu.Unlock()

	slotIP := layout.SlotIP4(1)
	syn := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), slotIP, lanIP, 5555, 631, nil)
	setTCPFlags(syn, tcpFlagSYN)

	// Eerste poging: de verse cache-entry gebruiken, nog geen extra werk.
	mu.Lock()
	natOutbound(1, append([]byte(nil), syn...))
	mu.Unlock()
	if len(nic.sent) != 1 || !bytes.Equal(nic.sent[0][0:6], lanMAC0[:]) {
		t.Fatalf("eerste SYN hoort alleen rechtstreeks te gaan (frames: %d)", len(nic.sent))
	}

	// Dezelfde SYN/5-tupel opnieuw: ARP parallel aan de gewone TCP-retry.
	mu.Lock()
	natOutbound(1, append([]byte(nil), syn...))
	mu.Unlock()
	if len(nic.sent) != 3 {
		t.Fatalf("retry hoort ARP + SYN te sturen, kreeg %d extra frame(s)", len(nic.sent)-1)
	}
	arp := nic.sent[1]
	if arp[12] != 0x08 || arp[13] != 0x06 ||
		!bytes.Equal(arp[0:6], []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) ||
		binary.BigEndian.Uint32(arp[38:]) != lanIP {
		t.Fatal("retry stuurde geen broadcast-ARP voor de neighbor")
	}
	if !bytes.Equal(nic.sent[2][0:6], lanMAC0[:]) {
		t.Fatal("TCP-retry zelf hoort de nog bekende next-hop te blijven proberen")
	}

	// Nog een onmiddellijke retry: de bestaande ARP-rate-limit voorkomt een
	// storm; TCP zelf blijft wel retransmitten.
	mu.Lock()
	natOutbound(1, append([]byte(nil), syn...))
	mu.Unlock()
	if len(nic.sent) != 4 || nic.sent[3][12] != 0x08 || nic.sent[3][13] != 0x00 {
		t.Fatalf("ARP-rate-limit liet onverwachte frames door: %d", len(nic.sent))
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

// Een geleerde neighbor is geen eeuwige waarheid: een wifi-host die slaapt of
// naar een ander mesh-punt zwerft wisselt van L2-pad, en zonder veroudering
// bleef het oude MAC voor altijd "known" — dials stierven stil terwijl elke
// andere machine (met normale ARP-veroudering) de host gewoon zag. De Brother
// op .201, 20-08: pas printer-uit-aan (gratuitous ARP) heelde het. Na neighTTL
// zonder levensteken hoort first-contact het opnieuw te vragen; élk inbound
// frame ververst.
func TestNeighborExpiresAndRelearns(t *testing.T) {
	resetNAT()
	nic := setUplink(t)
	mu.Lock()
	now := time.Now()
	learnLocked(extIP, gwMAC0[:], now) // gateway bekend, zoals op elke echte node
	learnLocked(lanIP, lanMAC0[:], now)
	mu.Unlock()

	// Vers: rechtstreeks naar het geleerde MAC.
	slotIP := layout.SlotIP4(1)
	syn := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), slotIP, lanIP, 5555, 631, nil)
	mu.Lock()
	natOutbound(1, append([]byte(nil), syn...))
	mu.Unlock()
	if len(nic.sent) != 1 || !bytes.Equal(nic.sent[0][0:6], lanMAC0[:]) {
		t.Fatalf("verse neighbor hoort rechtstreeks te gaan (frames: %d)", len(nic.sent))
	}

	// De printer zwijgt langer dan neighTTL (slaapt, zwerft): de entry is
	// verlopen en de volgende dial hoort first-contact te nemen — ARP-request
	// plus de SYN alvast via de gateway, niet meer het oude MAC.
	mu.Lock()
	entry := neigh[lanIP]
	entry.seen = time.Now().Add(-neighTTL - time.Second)
	neigh[lanIP] = entry
	natOutbound(1, append([]byte(nil), syn...))
	mu.Unlock()
	if len(nic.sent) != 3 {
		t.Fatalf("verlopen neighbor hoort ARP + via-gateway te geven, kreeg %d extra frame(s)", len(nic.sent)-1)
	}
	arp := nic.sent[1]
	if arp[12] != 0x08 || arp[13] != 0x06 || !bytes.Equal(arp[0:6], []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) {
		t.Fatal("geen broadcast-ARP voor de verlopen neighbor")
	}
	if !bytes.Equal(nic.sent[2][0:6], gwMAC0[:]) {
		t.Fatal("de meegestuurde SYN hoort via het gateway-MAC te gaan")
	}

	// Elk levensteken ververst: een inbound frame van de host (zijn nieuwe
	// MAC) en de volgende dial gaat weer rechtstreeks — naar het NIEUWE pad.
	newMAC := [6]byte{0x66, 0x77, 0x88, 0x99, 0xAA, 0xCC}
	mu.Lock()
	learnLocked(lanIP, newMAC[:], time.Now())
	natOutbound(1, append([]byte(nil), syn...))
	mu.Unlock()
	if last := nic.sent[len(nic.sent)-1]; !bytes.Equal(last[0:6], newMAC[:]) {
		t.Fatalf("na her-leren hoort het verkeer naar het nieuwe MAC te gaan, ging naar %x", last[0:6])
	}
}

func BenchmarkVolSlotRejectpad(b *testing.B) {
	resetNAT()
	now := time.Unix(1_000, 0)
	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < maxFlowsPerSlot; i++ {
		if flowFor(protoUDP, 1, layout.SlotIP4(1), uint16(1000+i), extIP, 443, now) == nil {
			b.Fatal("setup raakte vóór het slotquotum vol")
		}
	}
	// Meet alleen de O(1)-reject; sweep en log hebben hun eigen cadence.
	nextFlowSweep = now.Add(time.Hour)
	nextSlotFullLog[1] = now.Add(time.Hour)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if flowFor(protoUDP, 1, layout.SlotIP4(1), 60000, extIP, 443, now) != nil {
			b.Fatal("flow boven quotum geaccepteerd")
		}
	}
}

func BenchmarkKorteFlowReclaim(b *testing.B) {
	resetNAT()
	now := time.Unix(1_000, 0)
	mu.Lock()
	defer mu.Unlock()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		fl := flowFor(protoTCP, 1, layout.SlotIP4(1), 5555, extIP, 443, now)
		if fl == nil || !removeFlowLocked(fl, true) {
			b.Fatal("korte flow kon niet worden gemaakt/opgeruimd")
		}
	}
}
