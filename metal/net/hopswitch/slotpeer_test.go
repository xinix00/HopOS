// Buur-tests: twee ÉCHTE app-stacks (leannet) door de ÉCHTE switch, met echte
// ringen ertussen. Alle andere tests hier gooien een frame in forward() en
// kijken waar hij landt; die vorm bewijst niet dat een app zijn buur ook
// bereikt, want een dial doet meer dan één frame: ARP heen, reply terug,
// handshake, data.
//
// De reden dat dit bestaat: GEMETEN 12-08 op een LicheeRV lukte precies dat
// niet. Een app-slot dat de attach-poort van een ander slot belde (10.100.0.2)
// kreeg "i/o timeout", terwijl diezelfde poort van buiten de node openstond.
// De frame-tests waren groen — dus zat het gat tussen de lagen, en dan is de
// enige eerlijke test er een die beide lagen echt draait.
package hopswitch

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/xinix00/lean/leannet"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/abi/ring"
)

// slotRingCap is de datacapaciteit per richting in deze test: ruim voor een
// handshake plus wat data, klein genoeg om per test te alloceren.
const slotRingCap = 32 << 10

// slotNIC is de app-kant van de frame-ringen: schrijven in TX (die de switch
// leest), lezen uit RX (waar de switch bezorgt). Dit is dezelfde vorm als de
// nic in metal/app/applib/appnet.
type slotNIC struct {
	tx *ring.Ring
	rx *ring.Ring
}

func (n *slotNIC) Transmit(p []byte) error {
	n.tx.Write(ring.TypeFrame, p) // vol = drop, zoals echt ethernet
	return nil
}

func (n *slotNIC) Receive(buf []byte) (int, error) {
	typ, got, ok := n.rx.ReadInto(buf)
	if !ok || typ != ring.TypeFrame {
		return 0, nil
	}
	return got, nil
}

// attachTestSlot hangt slot i aan de switch met verse ringen in host-mmap en
// geeft de app-kant terug. De switch bewaart net als op het board alleen het
// fysieke adres; de test-cleanup koppelt eerst de poort los en ruimt dan op.
func attachTestSlot(t *testing.T, i int) *slotNIC {
	t.Helper()
	buf := testDeviceMemory(t, 2*(slotRingCap+dataOffTest))
	txBase := testDeviceAddress(buf)
	rxBase := txBase + uintptr(slotRingCap+dataOffTest)
	ring.Init(txBase, slotRingCap)
	ring.Init(rxBase, slotRingCap)

	mu.Lock()
	if len(ports) == 0 {
		ports = make([]*port, layout.MaxSlots+1)
	}
	ports[i] = &port{tx: ring.Open(txBase), rx: ring.Open(rxBase)}
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		ports[i] = nil
		mu.Unlock()
	})
	// De app leest/schrijft dezelfde ringen vanaf de andere kant.
	return &slotNIC{tx: ring.Open(txBase), rx: ring.Open(rxBase)}
}

// dataOffTest is de ringkop-maat uit de ABI (ring.dataOff is intern): head,
// tail en size elk in hun eigen cacheline.
const dataOffTest = 0x80

// runSwitch draait de switch-lus zolang de test loopt — dezelfde rondes als
// loop(), maar met een stop.
func runSwitch(t *testing.T) {
	t.Helper()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, layout.NetRingDataCap)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if !switchPass(buf) {
				time.Sleep(100 * time.Microsecond)
			}
		}
	}()
	t.Cleanup(func() { close(stop); <-done })
}

// appStack bouwt de netstack van slot i precies zoals appnet.Up dat doet: het
// net-plan uit layout, de gateway als statische buur, en een RX-lus die frames
// de stack in pompt. Wat hier anders is dan de echte app, is een bug in deze
// test — niet in de app.
func appStack(t *testing.T, i int) *leannet.Stack {
	t.Helper()
	nic := attachTestSlot(t, i)
	ip := layout.SlotIP4(i)
	st := leannet.NewStack(nic, leannet.Config{
		IP:     ip4(ip),
		Prefix: layout.NetPrefix,
		MAC:    layout.SlotMAC(i),
		GW:     ip4(layout.HostIP4()),
		Budget: 1 << 20,
	}, uint32(i)<<16|1)
	t.Cleanup(st.Close)
	if err := st.SeedNeighbor(ip4(layout.HostIP4()), layout.SlotMAC(0)); err != nil {
		t.Fatalf("seed gateway: %v", err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, leannet.MTU+leannet.EthernetMaximumSize)
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, err := nic.Receive(buf)
			if err != nil {
				return
			}
			if n == 0 {
				time.Sleep(100 * time.Microsecond)
				continue
			}
			st.RecvInboundPacket(buf[:n])
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	return st
}

func ip4(v uint32) [4]byte {
	return [4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

// TestSlotBereiktBuurslot is de test die op ijzer faalde: slot 2 belt de
// luisterende poort van slot 1 op diens slot-IP, en dat hoort te werken zonder
// dat iemand iets van tevoren heeft geleerd — de ARP loopt over het floodpad
// van de switch.
func TestSlotBereiktBuurslot(t *testing.T) {
	resetNAT()
	setUplink(t)
	runSwitch(t)

	server := appStack(t, 1)
	client := appStack(t, 2)

	listener, err := server.Listen(7000)
	if err != nil {
		t.Fatalf("listen op slot 1: %v", err)
	}
	defer listener.Close()
	go func() {
		c, err := listener.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c) // echo, zoals stulp's attach-poort een antwoord geeft
	}()

	conn, err := client.DialTCP(ip4(layout.SlotIP4(1)), 7000, time.Now().Add(10*time.Second))
	if err != nil {
		t.Fatalf("slot 2 → slot 1 (%s:7000): %v", SlotIP(1), err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("hallo buur")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "hallo buur" {
		t.Fatalf("terug: %q, wil %q", got, "hallo buur")
	}
}

// TestSlotBereiktGepubliceerdePoortVanBuur is de vorm die de app ECHT gebruikt
// als hij het node-adres met de gepubliceerde poort krijgt: het verkeer gaat de
// gateway in, komt via de hairpin bij de buur terug, en het antwoord moet de
// hele weg terug vinden. Zonder deze test is "gebruik HOPOS_HOST" een advies
// zonder bewijs.
func TestSlotBereiktGepubliceerdePoortVanBuur(t *testing.T) {
	resetNAT()
	setUplink(t)
	leerGateway(t)
	runSwitch(t)

	server := appStack(t, 1)
	client := appStack(t, 2)
	if err := Publish("tcp", 7000, 1, 7000); err != nil {
		t.Fatalf("publish: %v", err)
	}

	listener, err := server.Listen(7000)
	if err != nil {
		t.Fatalf("listen op slot 1: %v", err)
	}
	defer listener.Close()
	go func() {
		c, err := listener.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()

	// Het node-IP van de uplink (setUplink: 10.0.2.15) met de gepubliceerde poort.
	node := net.ParseIP("10.0.2.15").To4()
	conn, err := client.DialTCP([4]byte(node), 7000, time.Now().Add(10*time.Second))
	if err != nil {
		t.Fatalf("slot 2 → node-IP:7000 (hairpin): %v", err)
	}
	defer conn.Close()

	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("hallo hairpin")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 32)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(buf[:n]); got != "hallo hairpin" {
		t.Fatalf("terug: %q, wil %q", got, "hallo hairpin")
	}
}
