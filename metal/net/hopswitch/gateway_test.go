// Host-tests voor de gateway-poort (gateway.go): frames naar 10.100.0.1
// gaan de interne NIC in (niet de masquerade), extern verkeer blijft
// masqueraden, ARP-replies voor de gateway-MAC bereiken de interne NIC, en
// verkeer van de interne NIC terug wordt gewoon op dst-MAC bezorgd.
package hopswitch

import (
	"bytes"
	"encoding/binary"
	"testing"

	"hop-os/metal/abi/layout"
)

// captureGateway registreert een vangnet-gatewayRx en geeft de vangst terug.
func captureGateway(t *testing.T) *[][]byte {
	t.Helper()
	var got [][]byte
	SetGatewayRx(func(p []byte) { got = append(got, append([]byte(nil), p...)) })
	t.Cleanup(func() { SetGatewayRx(nil) })
	return &got
}

func TestGatewayIPGaatInterneNICIn(t *testing.T) {
	resetNAT()
	nic := setUplink(t)
	leerGateway(t)
	got := captureGateway(t)

	// Slot 1 → 10.100.0.1:9080 (de leader): interne NIC, géén masquerade.
	f := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), layout.HostIP4(), 5555, 9080, nil)
	mu.Lock()
	forward(1, f)
	mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("interne NIC kreeg %d frames, wil 1", len(*got))
	}
	if len(nic.sent) != 0 {
		t.Fatal("frame voor de gateway lekte de fysieke NIC uit")
	}
	// Ongewijzigd bezorgd: geen NAT op dit pad.
	if !bytes.Equal((*got)[0], f) {
		t.Fatal("gateway-frame is onderweg herschreven — dit pad hoort NAT-vrij te zijn")
	}
}

func TestExternBlijftMasquerade(t *testing.T) {
	resetNAT()
	nic := setUplink(t)
	leerGateway(t)
	got := captureGateway(t)

	f := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), extIP, 5555, 443, nil)
	mu.Lock()
	forward(1, f)
	mu.Unlock()
	if len(*got) != 0 {
		t.Fatal("extern verkeer belandde op de interne NIC")
	}
	if len(nic.sent) != 1 {
		t.Fatalf("extern verkeer niet gemasqueradeerd (%d frames op de NIC)", len(nic.sent))
	}
}

func TestArpReplyBereiktInterneNIC(t *testing.T) {
	resetNAT()
	setUplink(t)
	got := captureGateway(t)

	// Een ARP-reply (unicast naar de gateway-MAC) — het antwoord op een
	// who-has van de interne NIC. Geen IPv4, dus vroeger "viel dit weg".
	var f [42]byte
	copy(f[0:6], hostMAC[:])
	m := layout.SlotMAC(3)
	copy(f[6:12], m[:])
	f[12], f[13] = 0x08, 0x06
	a := f[ethLen:]
	a[0], a[1], a[2], a[3], a[4], a[5], a[7] = 0, 1, 8, 0, 6, 4, 2 // eth/IPv4, oper=reply
	copy(a[8:14], m[:])
	binary.BigEndian.PutUint32(a[14:], layout.SlotIP4(3))
	copy(a[18:24], hostMAC[:])
	binary.BigEndian.PutUint32(a[24:], layout.HostIP4())

	mu.Lock()
	forward(3, f[:])
	mu.Unlock()
	if len(*got) != 1 {
		t.Fatalf("ARP-reply bereikte de interne NIC niet (%d frames)", len(*got))
	}
}

func TestFromGatewayBezorgtOpSlot(t *testing.T) {
	resetNAT()
	read := testSlotRing(t, 2)
	mu.Lock()
	up = true // FromGateway eist een draaiende switch
	mu.Unlock()
	t.Cleanup(func() { mu.Lock(); up = false; mu.Unlock() })

	// Antwoord van de interne NIC (leader → app in slot 2).
	f := mkFrame(protoTCP, layout.SlotMAC(2), hostMAC, layout.HostIP4(), layout.SlotIP4(2), 9080, 5555, []byte("hoi"))
	FromGateway(f)
	got := read()
	if got == nil {
		t.Fatal("niets bezorgd in de ring van slot 2")
	}
	if !bytes.Equal(got, f) {
		t.Fatal("frame beschadigd onderweg")
	}
}
