package hopswitch

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/xinix00/HopOS/metal/abi/layout"
)

type queuedNIC struct {
	recv [][]byte
	sent [][]byte
}

func (n *queuedNIC) Receive(buf []byte) (int, error) {
	if len(n.recv) == 0 {
		return 0, nil
	}
	f := n.recv[0]
	n.recv = n.recv[1:]
	return copy(buf, f), nil
}

func (n *queuedNIC) Transmit(buf []byte) error {
	n.sent = append(n.sent, append([]byte(nil), buf...))
	return nil
}

func etherFrame(dst, src [6]byte, etherType uint16) []byte {
	f := make([]byte, ethLen)
	copy(f[0:6], dst[:])
	copy(f[6:12], src[:])
	binary.BigEndian.PutUint16(f[12:14], etherType)
	return f
}

func TestIPv6OutboundBranches(t *testing.T) {
	t.Run("unicast only bridges IPv6", func(t *testing.T) {
		resetNAT()
		nic := setUplink(t)
		dst := [6]byte{0x10, 0x20, 0x30, 0x40, 0x50, 0x60}
		v6 := etherFrame(dst, layout.SlotMAC(1), 0x86dd)

		mu.Lock()
		forward(1, v6)
		mu.Unlock()
		if len(nic.sent) != 1 || !bytes.Equal(nic.sent[0], v6) {
			t.Fatal("IPv6-unicast van slot ging niet ongewijzigd de uplink op")
		}

		v4 := etherFrame(dst, layout.SlotMAC(1), etIPv4)
		mu.Lock()
		forward(1, v4)
		mu.Unlock()
		if len(nic.sent) != 1 {
			t.Fatal("onbekende IPv4-unicast omzeilde de NAT-grens")
		}
	})

	t.Run("multicast reaches peers and uplink", func(t *testing.T) {
		resetNAT()
		nic := setUplink(t)
		read2 := testSlotRing(t, 2)
		dst := [6]byte{0x33, 0x33, 0, 0, 0, 0xfb}
		f := etherFrame(dst, layout.SlotMAC(1), 0x86dd)

		mu.Lock()
		forward(1, f)
		mu.Unlock()
		if got := read2(); !bytes.Equal(got, f) {
			t.Fatal("IPv6-multicast bereikte het buurslot niet")
		}
		if len(nic.sent) != 1 || !bytes.Equal(nic.sent[0], f) {
			t.Fatal("IPv6-multicast bereikte de uplink niet")
		}
	})
}

func TestIPv6InboundBranches(t *testing.T) {
	t.Run("slot unicast accepts only IPv6", func(t *testing.T) {
		resetNAT()
		read1 := testSlotRing(t, 1)
		src := [6]byte{0x10, 0x20, 0x30, 0x40, 0x50, 0x60}
		v4 := etherFrame(layout.SlotMAC(1), src, etIPv4)
		v6 := etherFrame(layout.SlotMAC(1), src, 0x86dd)
		nic := &queuedNIC{recv: [][]byte{v4, v6}}
		u, err := WrapUplink(nic, "10.0.2.15/24", nicMAC[:])
		if err != nil {
			t.Fatal(err)
		}

		buf := make([]byte, 1500)
		if n, err := u.Receive(buf); err != nil || n != len(v4) {
			t.Fatalf("IPv4-frame werd niet aan de nodestack teruggegeven: n=%d err=%v", n, err)
		}
		if got := read1(); got != nil {
			t.Fatal("LAN-IPv4 op een slot-MAC omzeilde de NAT/publicatiegrens")
		}
		if n, err := u.Receive(buf); err != nil || n != 0 {
			t.Fatalf("geclaimde IPv6-unicast: n=%d err=%v", n, err)
		}
		if got := read1(); !bytes.Equal(got, v6) {
			t.Fatal("IPv6-unicast bereikte het doelslot niet")
		}
	})

	t.Run("multicast floods connected slots", func(t *testing.T) {
		resetNAT()
		read1 := testSlotRing(t, 1)
		read2 := testSlotRing(t, 2)
		dst := [6]byte{0x33, 0x33, 0, 0, 0, 0xfb}
		src := [6]byte{0x10, 0x20, 0x30, 0x40, 0x50, 0x60}
		f := etherFrame(dst, src, 0x86dd)
		nic := &queuedNIC{recv: [][]byte{f}}
		u, err := WrapUplink(nic, "10.0.2.15/24", nicMAC[:])
		if err != nil {
			t.Fatal(err)
		}

		if n, err := u.Receive(make([]byte, 1500)); err != nil || n != 0 {
			t.Fatalf("geclaimde IPv6-multicast: n=%d err=%v", n, err)
		}
		if got := read1(); !bytes.Equal(got, f) {
			t.Fatal("IPv6-multicast bereikte slot 1 niet")
		}
		if got := read2(); !bytes.Equal(got, f) {
			t.Fatal("IPv6-multicast bereikte slot 2 niet")
		}
	})
}
