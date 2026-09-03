package hopswitch

import (
	"testing"
	"time"

	"github.com/xinix00/HopOS/metal/v2/abi/ring"
)

func TestRXWachtKortOpLokaleConsumer(t *testing.T) {
	rx := ring.New(4096)
	frame := make([]byte, 1000)
	for rx.Write(ring.TypeFrame, frame) {
	}

	mu.Lock()
	oldPorts, oldWake, oldHostWake := ports, rxWake, hostWake
	ports = make([]*port, 2)
	ports[1] = &port{rx: rx}
	rxWake, hostWake = nil, nil
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		ports, rxWake, hostWake = oldPorts, oldWake, oldHostWake
		mu.Unlock()
	})

	drained := make(chan struct{})
	go func() {
		time.Sleep(time.Millisecond)
		buf := make([]byte, len(frame))
		if _, _, ok := rx.ReadInto(buf); !ok {
			t.Error("volle RX-ring bevatte geen frame om ruimte te maken")
		}
		close(drained)
	}()

	mu.Lock()
	writeRXLocked(1, frame)
	mu.Unlock()
	<-drained
	if _, got, ok := rx.ReadInto(make([]byte, len(frame))); !ok || got != len(frame) {
		t.Fatalf("frame na RX-backpressure: ok=%v bytes=%d", ok, got)
	}
}
