// Host-tests voor de hairpin (hairpinOutLocked/hairpinBackLocked): een app
// die het node-IP belt komt bij de gepubliceerde poort van zijn buurslot uit,
// en de reply draagt de 4-tupel die de beller verwacht — zonder dat er één
// byte de uplink-NIC uit gaat.
package hopswitch

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
)

// TestHairpinHeenEnTerug: slot 2 belt nodeIP:7878 (gepubliceerd door slot 1).
// Heen: frame landt bij slot 1 als externe-client-verkeer (src = node-IP:masq,
// dst = slot-IP:7878). Terug: de reply van slot 1 naar node-IP:masq landt bij
// slot 2 met src = node-IP:7878 — het adres dat hij belde.
func TestHairpinHeenEnTerug(t *testing.T) {
	resetNAT()
	nic := setUplink(t)
	rdSrv := testSlotRing(t, 1)
	rdCli := testSlotRing(t, 2)
	if err := Publish("tcp", 7878, 1, 7878); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	cliIP, srvIP := layout.SlotIP4(2), layout.SlotIP4(1)

	// Heen: slot 2 → node-IP:7878 (off-subnet vanuit het slot-net, dus via de
	// gateway-MAC — precies hoe zo'n frame in natOutbound belandt).
	heen := mkFrame(protoTCP, hostMAC, layout.SlotMAC(2), cliIP, nodeIP, 5555, 7878, []byte("hoi"))
	mu.Lock()
	claimed := natOutbound(2, heen)
	mu.Unlock()
	if !claimed {
		t.Fatal("heen: natOutbound heeft het hairpin-frame niet geclaimd")
	}
	if len(nic.sent) != 0 {
		t.Fatalf("heen: hairpin-frame ging de uplink-NIC uit (%d frames)", len(nic.sent))
	}
	got := rdSrv()
	if got == nil {
		t.Fatal("heen: niets bezorgd in de ring van de dienst")
	}
	checkFrame(t, got, "heen")
	ip := got[ethLen:]
	l4 := ip[20:]
	if binary.BigEndian.Uint32(ip[16:]) != srvIP || binary.BigEndian.Uint16(l4[2:]) != 7878 {
		t.Fatalf("heen: dst niet naar de dienst herschreven (dst %x:%d)",
			binary.BigEndian.Uint32(ip[16:]), binary.BigEndian.Uint16(l4[2:]))
	}
	if binary.BigEndian.Uint32(ip[12:]) != nodeIP {
		t.Fatalf("heen: src niet gemasqueradet (src %x)", binary.BigEndian.Uint32(ip[12:]))
	}
	masq := binary.BigEndian.Uint16(l4[0:])
	if masq < MasqBase || masq >= MasqEnd {
		t.Fatalf("heen: masq-poort %d buiten [%d,%d)", masq, MasqBase, MasqEnd)
	}
	if !bytes.HasSuffix(got, []byte("hoi")) {
		t.Fatal("heen: payload beschadigd")
	}

	// Terug: de dienst antwoordt naar node-IP:masq vanaf zijn published poort.
	terug := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), srvIP, nodeIP, 7878, masq, []byte("dag"))
	mu.Lock()
	claimed = natFromSlot(1, terug)
	mu.Unlock()
	if !claimed {
		t.Fatal("terug: natFromSlot heeft de hairpin-reply niet geclaimd")
	}
	if len(nic.sent) != 0 {
		t.Fatalf("terug: reply ging de uplink-NIC uit (%d frames)", len(nic.sent))
	}
	got = rdCli()
	if got == nil {
		t.Fatal("terug: niets bezorgd in de ring van de beller")
	}
	checkFrame(t, got, "terug")
	ip = got[ethLen:]
	l4 = ip[20:]
	if binary.BigEndian.Uint32(ip[12:]) != nodeIP || binary.BigEndian.Uint16(l4[0:]) != 7878 {
		t.Fatalf("terug: src is niet node-IP:7878 (het gebelde adres) maar %x:%d",
			binary.BigEndian.Uint32(ip[12:]), binary.BigEndian.Uint16(l4[0:]))
	}
	if binary.BigEndian.Uint32(ip[16:]) != cliIP || binary.BigEndian.Uint16(l4[2:]) != 5555 {
		t.Fatalf("terug: dst is niet de beller (dst %x:%d)",
			binary.BigEndian.Uint32(ip[16:]), binary.BigEndian.Uint16(l4[2:]))
	}
	if !bytes.HasSuffix(got, []byte("dag")) {
		t.Fatal("terug: payload beschadigd")
	}
}

// TestHairpinUDP: hetzelfde pad voor UDP (geen SYN-guard, wél de poort-match
// in natFromSlot) — één rondje heen en terug.
func TestHairpinUDP(t *testing.T) {
	resetNAT()
	nic := setUplink(t)
	rdSrv := testSlotRing(t, 1)
	rdCli := testSlotRing(t, 2)
	if err := Publish("udp", 5353, 1, 5353); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	heen := mkFrame(protoUDP, hostMAC, layout.SlotMAC(2), layout.SlotIP4(2), nodeIP, 6666, 5353, []byte("vraag"))
	mu.Lock()
	natOutbound(2, heen)
	mu.Unlock()
	got := rdSrv()
	if got == nil {
		t.Fatal("heen: niets bezorgd bij de dienst")
	}
	checkFrame(t, got, "udp heen")
	masq := binary.BigEndian.Uint16(got[ethLen+20:])

	terug := mkFrame(protoUDP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), nodeIP, 5353, masq, []byte("antwoord"))
	mu.Lock()
	natFromSlot(1, terug)
	mu.Unlock()
	got = rdCli()
	if got == nil {
		t.Fatal("terug: niets bezorgd bij de beller")
	}
	checkFrame(t, got, "udp terug")
	if len(nic.sent) != 0 {
		t.Fatalf("udp-hairpin raakte de uplink-NIC (%d frames)", len(nic.sent))
	}
}

// TestHairpinOngepubliceerdDropt: het node-IP bellen op een poort zonder
// publicatie is een dichte deur — claimen en droppen, niet de NIC uit
// (dáár zou het frame naar ons eigen IP het LAN op lekken).
func TestHairpinOngepubliceerdDropt(t *testing.T) {
	resetNAT()
	nic := setUplink(t)
	rdSrv := testSlotRing(t, 1)
	f := mkFrame(protoTCP, hostMAC, layout.SlotMAC(2), layout.SlotIP4(2), nodeIP, 5555, 8080, nil)
	mu.Lock()
	claimed := natOutbound(2, f)
	mu.Unlock()
	if !claimed {
		t.Fatal("frame naar het node-IP niet geclaimd")
	}
	if len(nic.sent) != 0 || rdSrv() != nil {
		t.Fatal("frame naar ongepubliceerde node-poort is toch ergens bezorgd")
	}
}

// TestHairpinReplyZonderFlowDropt: een reply naar node-IP:poort zonder
// bijbehorende conntrack-entry (verlopen, of spontaan verkeer vanaf een
// publicatie-poort) wordt gedropt.
func TestHairpinReplyZonderFlowDropt(t *testing.T) {
	resetNAT()
	nic := setUplink(t)
	rdCli := testSlotRing(t, 2)
	if err := Publish("tcp", 7878, 1, 7878); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	f := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), nodeIP, 7878, 20001, nil)
	mu.Lock()
	claimed := natFromSlot(1, f)
	mu.Unlock()
	if !claimed {
		t.Fatal("flow-loze reply niet geclaimd")
	}
	if len(nic.sent) != 0 || rdCli() != nil {
		t.Fatal("flow-loze reply is toch ergens bezorgd")
	}
}
