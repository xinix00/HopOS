package appnet

import (
	"net"
	"testing"
	"time"

	"github.com/xinix00/lean/leannet"

	"github.com/xinix00/HopOS/metal/v2/abi/ring"
)

type discardDevice struct{}

func (discardDevice) Receive([]byte) (int, error) { return 0, nil }
func (discardDevice) Transmit([]byte) error       { return nil }

func TestJoinMulticastDispatchesByIPFamily(t *testing.T) {
	st := leannet.NewStack(discardDevice{}, leannet.Config{
		Prefix: 24,
		MAC:    [6]byte{0x02, 0, 0, 0, 0, 1},
		Budget: 1 << 20,
	}, 1)
	previous := current
	current = st
	t.Cleanup(func() {
		current = previous
		st.Close()
	})

	for _, group := range []net.IP{
		net.IPv4(224, 0, 0, 251),
		net.ParseIP("ff02::fb"),
	} {
		if err := JoinMulticast(group); err != nil {
			t.Errorf("JoinMulticast(%s): %v", group, err)
		}
	}
}

func TestTransmitWachtKortOpRuimteInPlaatsVanDrop(t *testing.T) {
	tx := ring.New(4096)
	frame := make([]byte, 1000)
	filled := 0
	for tx.Write(ring.TypeFrame, frame) {
		filled++
	}
	if filled < 2 {
		t.Fatalf("testring vulde al na %d frames", filled)
	}

	drained := make(chan struct{})
	go func() {
		time.Sleep(time.Millisecond)
		buf := make([]byte, len(frame))
		for {
			_, _, ok := tx.ReadInto(buf)
			if !ok {
				break
			}
		}
		close(drained)
	}()

	n := &nic{tx: tx}
	if err := n.Transmit(frame); err != nil {
		t.Fatalf("Transmit gaf de lokale burst op: %v", err)
	}
	<-drained
	if _, got, ok := tx.ReadInto(make([]byte, len(frame))); !ok || got != len(frame) {
		t.Fatalf("frame na backpressure: ok=%v bytes=%d", ok, got)
	}
}
