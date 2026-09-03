package appnet

import (
	"net"
	"testing"
	"time"
	"unsafe"

	"github.com/xinix00/lean/leannet"

	"github.com/xinix00/HopOS/metal/v2/abi/frameq"
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

func TestTransmitWachtKortOpVrijeDescriptorInPlaatsVanDrop(t *testing.T) {
	mem := make([]uint64, frameq.PageSize/8)
	base := uintptr(unsafe.Pointer(&mem[0]))
	frameq.Init(base)
	tx := frameq.Open(base)
	frame := make([]byte, 1000)

	drained := make(chan struct{})
	go func() {
		time.Sleep(time.Millisecond)
		tx.Complete(0, 0, frameq.StatusOK)
		close(drained)
	}()

	n := &nic{tx: tx}
	// Alle tokens bezet; completion 0 maakt er na 1ms precies één vrij.
	if err := n.Transmit(frame); err != nil {
		t.Fatalf("Transmit gaf de lokale burst op: %v", err)
	}
	<-drained
	d, ok := tx.Take()
	if !ok || d.Length != uint32(len(frame)) || d.Token != 0 {
		t.Fatalf("descriptor na backpressure: %#v ok=%v", d, ok)
	}
}
