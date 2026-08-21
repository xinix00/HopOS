// Host-tests voor de gateway-poort (gateway.go): frames naar 10.100.0.1
// gaan de interne NIC in (niet de masquerade), extern verkeer blijft
// masqueraden, ARP-replies voor de gateway-MAC bereiken de interne NIC, en
// verkeer van de interne NIC terug wordt gewoon op dst-MAC bezorgd.
package hopswitch

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/xinix00/HopOS/metal/abi/layout"
)

// captureGateway registreert een vangnet-gatewayRx en geeft de vangst terug.
func captureGateway(t *testing.T) *[][]byte {
	t.Helper()
	var got [][]byte
	SetGatewayRx(func(p []byte) { got = append(got, append([]byte(nil), p...)) })
	t.Cleanup(func() { SetGatewayRx(nil) })
	return &got
}

// forwardEnDrain doet één switch-ronde zoals switchPass dat doet: forward onder
// mu (dáár wordt een gateway-frame alleen gequeued) en de aflevering aan de
// interne NIC erna, búiten mu. Die scheiding ís het contract — zie gateway.go.
func forwardEnDrain(src int, p []byte) {
	mu.Lock()
	forward(src, p)
	mu.Unlock()
	drainGateway()
}

func TestGatewayIPGaatInterneNICIn(t *testing.T) {
	resetNAT()
	nic := setUplink(t)
	leerGateway(t)
	got := captureGateway(t)

	// Slot 1 → 10.100.0.1:9080 (de leader): interne NIC, géén masquerade.
	f := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), layout.HostIP4(), 5555, 9080, nil)
	forwardEnDrain(1, f)
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

// De framegrens staat vóór elke forward-tak: een slot kan dus noch een
// buurring met een record groter dan diens MTU-buffer vergiftigen, noch zo'n
// record door de heap-backed gateway-wachtrij laten kopiëren.
func TestIngressFramegrensVoorForwardEnGatewayKopie(t *testing.T) {
	resetNAT()
	victim := attachTestSlot(t, 2)
	gotGateway := captureGateway(t)
	srcMAC, dstMAC := layout.SlotMAC(1), layout.SlotMAC(2)

	exact := make([]byte, maxFrameLen)
	copy(exact[0:6], dstMAC[:])
	copy(exact[6:12], srcMAC[:])
	mu.Lock()
	forward(1, exact)
	mu.Unlock()
	buf := make([]byte, maxFrameLen)
	if n, err := victim.Receive(buf); err != nil || n != maxFrameLen {
		t.Fatalf("frame op de grens: n=%d err=%v, wil %d", n, err, maxFrameLen)
	}

	oversized := append(append([]byte(nil), exact...), 0)
	mu.Lock()
	forward(1, oversized)
	mu.Unlock()
	if n, err := victim.Receive(buf); err != nil || n != 0 {
		t.Fatalf("oversized frame bereikte buurring: n=%d err=%v", n, err)
	}
	if victim.rx.Corrupt() {
		t.Fatalf("oversized frame maakte buurring corrupt: %s", victim.rx.CorruptWhy())
	}

	// Dezelfde grens moet vóór gwEnqueueLocked's heap-kopie staan.
	toGateway := mkFrame(protoTCP, hostMAC, srcMAC, layout.SlotIP4(1), layout.HostIP4(), 5555, 9080, nil)
	toGateway = append(toGateway, make([]byte, maxFrameLen+1-len(toGateway))...)
	forwardEnDrain(1, toGateway)
	if len(*gotGateway) != 0 {
		t.Fatal("oversized frame werd naar de heap-backed gateway-queue gekopieerd")
	}

	// Een reject mag de ring niet vergiftigen: het eerstvolgende gewone frame
	// moet nog steeds door dezelfde poort kunnen.
	small := exact[:ethLen]
	mu.Lock()
	forward(1, small)
	mu.Unlock()
	if n, err := victim.Receive(buf); err != nil || n != len(small) {
		t.Fatalf("ring werkte niet meer na reject: n=%d err=%v", n, err)
	}
}

func TestExternBlijftMasquerade(t *testing.T) {
	resetNAT()
	nic := setUplink(t)
	leerGateway(t)
	got := captureGateway(t)

	f := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), extIP, 5555, 443, nil)
	forwardEnDrain(1, f)
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

	forwardEnDrain(3, f[:])
	if len(*got) != 1 {
		t.Fatalf("ARP-reply bereikte de interne NIC niet (%d frames)", len(*got))
	}
}

// De teruglus-regressie: gvisor antwoordt SYNCHROON binnen InjectInbound (een
// SYN naar een gesloten node-poort levert direct een RST), en dat antwoord komt
// via internalTx.WriteNotify → FromGateway de switch weer in — op dezelfde
// goroutine. Werd gatewayRx onder mu aangeroepen, dan pakte FromGateway
// diezelfde niet-reentrante mutex en stond de switch permanent stil: één SYN van
// een willekeurige app naar een dichte poort velde het netwerk van de hele node.
// Deze test bootst precies die teruglus na (de echte gvisor-stack past niet in
// een host-test) en moet gewoon aflopen.
func TestGatewayTeruglusDeadlocktNiet(t *testing.T) {
	resetNAT()
	setUplink(t)
	leerGateway(t)
	read := testSlotRing(t, 1)
	mu.Lock()
	up = true // FromGateway eist een draaiende switch
	mu.Unlock()
	t.Cleanup(func() { mu.Lock(); up = false; mu.Unlock() })

	// De "stack": elk ontvangen frame lokt onmiddellijk een antwoord uit dat
	// terug de switch in gaat — zoals een RST op een gesloten poort.
	var beantwoord int
	SetGatewayRx(func(p []byte) {
		beantwoord++
		rst := mkFrame(protoTCP, layout.SlotMAC(1), hostMAC, layout.HostIP4(), layout.SlotIP4(1), 9080, 5555, nil)
		FromGateway(rst) // ← dit pakte vroeger mu terwijl switchPass hem vasthield
	})
	t.Cleanup(func() { SetGatewayRx(nil) })

	// Slot 1 → 10.100.0.1:9080 (dichte poort): de switch-ronde moet aflopen.
	f := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), layout.HostIP4(), 5555, 9080, nil)
	klaar := make(chan struct{})
	go func() {
		defer close(klaar)
		forwardEnDrain(1, f)
	}()
	select {
	case <-klaar:
	case <-time.After(5 * time.Second):
		t.Fatal("switch-ronde liep vast op de gateway-teruglus (deadlock)")
	}
	if beantwoord != 1 {
		t.Fatalf("interne NIC kreeg %d frames, wil 1", beantwoord)
	}
	if got := read(); got == nil {
		t.Fatal("het antwoord van de interne NIC kwam niet in de ring van slot 1")
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

// Een slot mag zich niet als een ánder slot voordoen. Dit is de regel die
// ARP-vergiftiging tussen buren onmogelijk maakt: zonder hem kan slot 2 een
// ARP-reply namens slot 1 sturen en daarna diens verkeer ontvangen — en sinds
// HOP toetsaanslagen naar de display stuurt (gui/usbin) is dat meeluisteren
// met wat de gebruiker typt.
func TestSlotMagGeenVreemdeBronMACGebruiken(t *testing.T) {
	resetNAT()
	setUplink(t)
	leerGateway(t)
	got := captureGateway(t)

	// Slot 2 stuurt een frame met de MAC én het IP van slot 1.
	f := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), layout.HostIP4(), 5555, 9080, nil)
	forwardEnDrain(2, f)
	if len(*got) != 0 {
		t.Fatalf("vervalst frame kwam tóch bij de interne NIC (%d)", len(*got))
	}

	// Hetzelfde frame vanaf zijn eigen slot gaat wél door — de regel mag geen
	// gewoon verkeer breken.
	eigen := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), layout.HostIP4(), 5555, 9080, nil)
	forwardEnDrain(1, eigen)
	if len(*got) != 1 {
		t.Fatalf("eigen frame kwam niet aan (%d)", len(*got))
	}
}

// Slot-naar-slot loopt langs dezelfde controle: een vervalst frame bereikt de
// ring van het doelslot niet.
func TestVervalstFrameBereiktBuurslotNiet(t *testing.T) {
	resetNAT()
	lees2 := testSlotRing(t, 2)

	f := mkFrame(protoTCP, layout.SlotMAC(2), layout.SlotMAC(1), layout.SlotIP4(1), layout.SlotIP4(2), 1234, 80, nil)
	mu.Lock()
	forward(3, f) // slot 3 doet alsof hij slot 1 is
	mu.Unlock()
	if got := lees2(); got != nil {
		t.Fatal("buurslot kreeg een vervalst frame")
	}

	// Vanaf slot 1 zelf komt hij wél aan.
	mu.Lock()
	forward(1, f)
	mu.Unlock()
	if got := lees2(); got == nil {
		t.Fatal("echt frame kwam niet bij het buurslot aan")
	}
}
