// Host-tests voor HOP als LAN-poort 0: dezelfde SPSC-ringen als app-poorten,
// geen heap-backed gatewayqueue en geen synchrone teruglus in de switchlock.
package hopswitch

import (
	"bytes"
	"testing"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/abi/ring"
)

func testHostPort(t *testing.T) *hostDevice {
	t.Helper()
	// Ringen van 256 KiB: een record mag hoogstens de halve ring zijn, en de
	// grootste LAN-frame is sinds de jumbo-MTU 65553 bytes (layout.NetMTU).
	txMem := testDeviceMemory(t, 512<<10)
	rxMem := testDeviceMemory(t, 512<<10)
	txBase, rxBase := testDeviceAddress(txMem), testDeviceAddress(rxMem)
	ring.Init(txBase, 256<<10)
	ring.Init(rxBase, 256<<10)
	d := &hostDevice{tx: ring.Open(txBase), rx: ring.Open(rxBase)}
	mu.Lock()
	if len(ports) == 0 {
		ports = make([]*port, layout.MaxSlots+1)
	}
	ports[0] = &port{tx: d.tx, rx: d.rx}
	host = d
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		if len(ports) > 0 && host == d {
			ports[0], host = nil, nil
		}
		mu.Unlock()
	})
	return d
}

func forwardOnce(src int, p []byte) {
	mu.Lock()
	forward(src, p)
	mu.Unlock()
}

func receiveFrame(t *testing.T, d *hostDevice) []byte {
	t.Helper()
	b := make([]byte, maxFrameLen)
	n, err := d.Receive(b)
	if err != nil {
		t.Fatalf("host Receive: %v", err)
	}
	if n == 0 {
		return nil
	}
	return b[:n]
}

func TestGatewayIPGaatLANPoortNulIn(t *testing.T) {
	resetNAT()
	nic := setUplink(t)
	leerGateway(t)
	h := testHostPort(t)

	f := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), layout.HostIP4(), 5555, 9080, nil)
	forwardOnce(1, f)
	got := receiveFrame(t, h)
	if !bytes.Equal(got, f) {
		t.Fatalf("poort 0 kreeg %d bytes, wil het ongewijzigde frame (%d)", len(got), len(f))
	}
	if len(nic.sent) != 0 {
		t.Fatal("frame voor de gateway lekte de fysieke NIC uit")
	}
}

func TestIngressFramegrensVoorLANRingen(t *testing.T) {
	resetNAT()
	victim := attachTestSlot(t, 2)
	h := testHostPort(t)
	srcMAC, dstMAC := layout.SlotMAC(1), layout.SlotMAC(2)

	exact := make([]byte, maxFrameLen)
	copy(exact[0:6], dstMAC[:])
	copy(exact[6:12], srcMAC[:])
	forwardOnce(1, exact)
	buf := make([]byte, maxFrameLen)
	if n, err := victim.Receive(buf); err != nil || n != maxFrameLen {
		t.Fatalf("frame op de grens: n=%d err=%v, wil %d", n, err, maxFrameLen)
	}

	oversized := append(append([]byte(nil), exact...), 0)
	forwardOnce(1, oversized)
	if n, err := victim.Receive(buf); err != nil || n != 0 {
		t.Fatalf("oversized frame bereikte buurring: n=%d err=%v", n, err)
	}
	if victim.rx.Corrupt() {
		t.Fatalf("oversized frame maakte buurring corrupt: %s", victim.rx.CorruptWhy())
	}

	toGateway := mkFrame(protoTCP, hostMAC, srcMAC, layout.SlotIP4(1), layout.HostIP4(), 5555, 9080, nil)
	toGateway = append(toGateway, make([]byte, maxFrameLen+1-len(toGateway))...)
	forwardOnce(1, toGateway)
	if got := receiveFrame(t, h); got != nil {
		t.Fatal("oversized frame werd in poort 0 geschreven")
	}

	small := exact[:ethLen]
	forwardOnce(1, small)
	if n, err := victim.Receive(buf); err != nil || n != len(small) {
		t.Fatalf("ring werkte niet meer na reject: n=%d err=%v", n, err)
	}
}

func TestExternBlijftMasquerade(t *testing.T) {
	resetNAT()
	nic := setUplink(t)
	leerGateway(t)
	h := testHostPort(t)
	f := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), extIP, 5555, 443, nil)
	forwardOnce(1, f)
	if got := receiveFrame(t, h); got != nil {
		t.Fatal("extern verkeer belandde op poort 0")
	}
	if len(nic.sent) != 1 {
		t.Fatalf("extern verkeer niet gemasqueradeerd (%d frames op de NIC)", len(nic.sent))
	}
}

func TestHostPoortAntwoordWordtInDezelfdeSwitchrondeBezorgd(t *testing.T) {
	resetNAT()
	h := testHostPort(t)
	read := testSlotRing(t, 1)
	f := mkFrame(protoTCP, layout.SlotMAC(1), hostMAC, layout.HostIP4(), layout.SlotIP4(1), 9080, 5555, []byte("hoi"))
	if err := h.Transmit(f); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, layout.NetRingDataCap)
	if !switchPass(buf) {
		t.Fatal("switch zag poort-0-frame niet")
	}
	if got := read(); !bytes.Equal(got, f) {
		t.Fatalf("antwoord beschadigd: got=%x want=%x", got, f)
	}
}

func TestRXWakeAlleenOpLeegNaarNietLeeg(t *testing.T) {
	resetNAT()
	read := testSlotRing(t, 2)
	mu.Lock()
	wakes := 0
	rxWake = func(slot int) {
		if slot == 2 {
			wakes++
		}
	}
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		rxWake = nil
		mu.Unlock()
	})

	f := mkFrame(protoTCP, layout.SlotMAC(2), layout.SlotMAC(1), layout.SlotIP4(1), layout.SlotIP4(2), 1111, 2222, nil)
	forwardOnce(1, f)
	forwardOnce(1, f)
	if wakes != 1 {
		t.Fatalf("twee writes zonder drain gaven %d wakes, wil 1", wakes)
	}
	if read() == nil || read() == nil {
		t.Fatal("frames ontbreken")
	}
	forwardOnce(1, f)
	if wakes != 2 {
		t.Fatalf("write na drain gaf totaal %d wakes, wil 2", wakes)
	}
}

// switchPending leest sinds 04-09 zónder slot: een TryLock is een CAS, en
// een exclusive zet op de M4 het event-register, waarna HOP's WFE meteen
// terugkeert (1,7M rondes/s). Onder lock-contention belt hij dus niet meer
// conservatief, maar meldt hij wat er werkelijk in de TX-ringen ligt.
func TestSwitchPendingLeestOnderLockContention(t *testing.T) {
	testSlotRing(t, 1)
	mu.Lock()
	defer mu.Unlock()
	if switchPending() {
		t.Fatal("niets in de TX-ringen, maar de bel gaat")
	}
	if !ports[1].tx.Write(ring.TypeFrame, make([]byte, 64)) {
		t.Fatal("testframe past niet in de TX-ring")
	}
	if !switchPending() {
		t.Fatal("frame in de TX-ring, maar de bel zwijgt onder lock-contention")
	}
}

func TestSlotMagGeenVreemdeBronMACOfIPGebruiken(t *testing.T) {
	resetNAT()
	setUplink(t)
	leerGateway(t)
	h := testHostPort(t)

	vervalsteMAC := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), layout.HostIP4(), 5555, 9080, nil)
	forwardOnce(2, vervalsteMAC)
	if receiveFrame(t, h) != nil {
		t.Fatal("vervalste bron-MAC kwam bij HOP")
	}

	vervalstIP := mkFrame(protoTCP, hostMAC, layout.SlotMAC(2), layout.SlotIP4(1), layout.HostIP4(), 5555, 9080, nil)
	forwardOnce(2, vervalstIP)
	if receiveFrame(t, h) != nil {
		t.Fatal("vervalst bron-IP kwam bij HOP")
	}

	eigen := mkFrame(protoTCP, hostMAC, layout.SlotMAC(1), layout.SlotIP4(1), layout.HostIP4(), 5555, 9080, nil)
	forwardOnce(1, eigen)
	if got := receiveFrame(t, h); !bytes.Equal(got, eigen) {
		t.Fatal("eigen frame kwam niet aan")
	}
}
