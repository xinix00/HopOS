// Host-tests voor de RST-bij-slot-dood (rst.go): passieve seq-tracking op
// het switch-pad en de masquerade-flow, en de RST-injectie van ResetPeers —
// exact-verwachte seq (RFC 5961) en kloppende checksums, want de ontvangende
// netstack is onverbiddelijk.
package hopswitch

import (
	"bytes"
	"encoding/binary"
	"testing"

	"hop-os/metal/abi/layout"
)

// resetConns zet de rst.go-state terug (naast resetNAT).
func resetConns() {
	mu.Lock()
	conns = map[skey]*sconn{}
	connsFull = false
	mu.Unlock()
}

// mkTCPSeq bouwt een geldig TCP-frame met een expliciet seq en vlaggen.
func mkTCPSeq(dstMAC, srcMAC [6]byte, srcIP, dstIP uint32, sport, dport uint16, seq uint32, flags byte, payload []byte) []byte {
	f := mkFrame(protoTCP, dstMAC, srcMAC, srcIP, dstIP, sport, dport, payload)
	ip := f[ethLen:]
	l4 := ip[20:]
	binary.BigEndian.PutUint32(l4[4:], seq)
	l4[13] = flags
	// TCP-checksum opnieuw (volledig): seq en vlaggen zijn veranderd.
	binary.BigEndian.PutUint16(l4[16:], 0)
	sum := sumWords(ip[12:20]) + uint32(protoTCP) + uint32(len(l4)) + sumWords(l4)
	binary.BigEndian.PutUint16(l4[16:], ^fold16(sum))
	return f
}

func TestRstNaSlotDood(t *testing.T) {
	resetNAT()
	resetConns()
	readPeer := testSlotRing(t, 2) // de overlevende kant (de display)
	testSlotRing(t, 3)             // de straks-dode kant

	ipDead, ipPeer := layout.SlotIP4(3), layout.SlotIP4(2)

	// Verkeer meelezen: SYN van 3→2 (seq 999 → volgende 1000), dan 5 bytes
	// payload (seq 1000 → volgende 1005). De andere richting doet niet mee.
	mu.Lock()
	trackSlotTCP(3, 2, mkTCPSeq(layout.SlotMAC(2), layout.SlotMAC(3), ipDead, ipPeer, 5555, 7878, 999, tcpFlagSYN, nil))
	trackSlotTCP(3, 2, mkTCPSeq(layout.SlotMAC(2), layout.SlotMAC(3), ipDead, ipPeer, 5555, 7878, 1000, 0x10, []byte("hallo")))
	mu.Unlock()

	ResetPeers(3)

	f := readPeer()
	if f == nil {
		t.Fatal("geen RST in de ring van de peer")
	}
	ip := f[ethLen:]
	l4 := ip[20:]
	if ip[9] != protoTCP || l4[13]&tcpFlagRST == 0 {
		t.Fatalf("geen TCP-RST: proto %d vlaggen %#x", ip[9], l4[13])
	}
	if got := binary.BigEndian.Uint32(ip[12:]); got != ipDead {
		t.Fatalf("bron-IP: %#x, wil het dode slot %#x", got, ipDead)
	}
	if got := binary.BigEndian.Uint32(ip[16:]); got != ipPeer {
		t.Fatalf("dst-IP: %#x, wil de peer %#x", got, ipPeer)
	}
	if sp, dp := binary.BigEndian.Uint16(l4[0:]), binary.BigEndian.Uint16(l4[2:]); sp != 5555 || dp != 7878 {
		t.Fatalf("poorten %d→%d, wil 5555→7878", sp, dp)
	}
	if seq := binary.BigEndian.Uint32(l4[4:]); seq != 1005 {
		t.Fatalf("seq %d, wil het exact-verwachte 1005 (RFC 5961)", seq)
	}
	deadMAC, peerMAC := layout.SlotMAC(3), layout.SlotMAC(2)
	if !bytes.Equal(f[0:6], peerMAC[:]) || !bytes.Equal(f[6:12], deadMAC[:]) {
		t.Fatal("L2: dst hoort de peer te zijn, src het dode slot")
	}
	checkFrame(t, f, "slot-RST")

	// De entry is verbruikt: nóg een ResetPeers stuurt niets meer.
	if ResetPeers(3); readPeer() != nil {
		t.Fatal("tweede ResetPeers stuurde opnieuw een RST")
	}
}

func TestRstRuimtEntryOpBijAppRst(t *testing.T) {
	resetNAT()
	resetConns()
	readPeer := testSlotRing(t, 2)
	testSlotRing(t, 3)

	ipDead, ipPeer := layout.SlotIP4(3), layout.SlotIP4(2)
	mu.Lock()
	trackSlotTCP(3, 2, mkTCPSeq(layout.SlotMAC(2), layout.SlotMAC(3), ipDead, ipPeer, 5555, 7878, 999, tcpFlagSYN, nil))
	// De app zelf stuurt een RST (nette abort): de tabel moet 'm vergeten.
	trackSlotTCP(3, 2, mkTCPSeq(layout.SlotMAC(2), layout.SlotMAC(3), ipDead, ipPeer, 5555, 7878, 1000, tcpFlagRST, nil))
	mu.Unlock()

	if ResetPeers(3); readPeer() != nil {
		t.Fatal("RST verstuurd voor een al-afgebroken verbinding")
	}
}

func TestRstNaarExternePeer(t *testing.T) {
	resetNAT()
	resetConns()
	nic := setUplink(t)
	leerGateway(t)

	// Uitgaande flow van slot 3 met bekende seq: 10 bytes payload op seq 500
	// → volgende verwachte 510.
	slotIP := layout.SlotIP4(3)
	out := mkTCPSeq(hostMAC, layout.SlotMAC(3), slotIP, extIP, 6666, 443, 500, 0x18, []byte("0123456789"))
	mu.Lock()
	if !natOutbound(3, out) {
		mu.Unlock()
		t.Fatal("uitgaand frame niet geclaimd")
	}
	mu.Unlock()
	masqPort := binary.BigEndian.Uint16(nic.sent[0][ethLen+20:])

	ResetPeers(3)
	if len(nic.sent) != 2 {
		t.Fatalf("verwacht 1 RST op de uplink, kreeg %d frames", len(nic.sent)-1)
	}
	f := nic.sent[1]
	ip := f[ethLen:]
	l4 := ip[20:]
	if l4[13]&tcpFlagRST == 0 {
		t.Fatal("geen RST-vlag")
	}
	if got := binary.BigEndian.Uint32(ip[12:]); got != nodeIP {
		t.Fatalf("bron hoort het node-IP te zijn (masquerade): %#x", got)
	}
	if got := binary.BigEndian.Uint32(ip[16:]); got != extIP {
		t.Fatalf("dst hoort de externe peer te zijn: %#x", got)
	}
	if sp := binary.BigEndian.Uint16(l4[0:]); sp != masqPort {
		t.Fatalf("bronpoort %d, wil de masq-poort %d", sp, masqPort)
	}
	if seq := binary.BigEndian.Uint32(l4[4:]); seq != 510 {
		t.Fatalf("seq %d, wil 510", seq)
	}
	checkFrame(t, f, "uplink-RST")
}
