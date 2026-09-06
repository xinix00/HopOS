package nvme

import (
	"errors"
	"runtime"
	"testing"
	"unsafe"
)

func TestInvalidTransferBeforeDMA(t *testing.T) {
	c := &Controller{BlockSize: 512, Blocks: 128, MaxTransfer: maxTransferSize}
	for _, lba := range []uint64{128, ^uint64(0)} {
		if err := c.Write(lba, make([]byte, 512)); err == nil {
			t.Fatalf("lba %d accepted", lba)
		}
	}
	c.BlockSize = 0
	if err := c.Read(0, make([]byte, 512)); err == nil {
		t.Fatal("uninitialized controller accepted")
	}
}

func TestTimeoutRetainsDMA(t *testing.T) {
	regs := make([]uint64, 0x2000/8)
	sq := make([]uint64, qEntries*sqeSize/8)
	cq := make([]uint64, qEntries*cqeSize/8)
	data := make([]byte, 512)
	c := &Controller{Base: uintptr(unsafe.Pointer(&regs[0])), BlockSize: 512, Blocks: 128, MaxTransfer: 512, buf: uintptr(unsafe.Pointer(&data[0]))}
	c.io = queue{sq: uintptr(unsafe.Pointer(&sq[0])), cq: uintptr(unsafe.Pointer(&cq[0])), phase: 1, id: 1}
	err := c.Read(0, make([]byte, 512))
	if err == nil || c.failed == nil {
		t.Fatal("missing completion did not fence controller")
	}
	// A late completion does not authorize a new write to the old DMA buffer.
	cq[1] = uint64(1) << 48
	payload := make([]byte, 512)
	payload[0] = 99
	if got := c.Write(1, payload); !errors.Is(got, err) {
		t.Fatalf("lost retained error: %v", got)
	}
	if data[0] != 0 || c.io.tail != 1 {
		t.Fatal("DMA/queue reused after timeout")
	}
	runtime.KeepAlive(regs)
	runtime.KeepAlive(sq)
	runtime.KeepAlive(cq)
	runtime.KeepAlive(data)
}
