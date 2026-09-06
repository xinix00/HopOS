//go:build !tamago

package virtionet

import (
	"bytes"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"runtime"
	"testing"
	"unsafe"
)

func TestReceiveValidatesDeviceRecordBeforeCopy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		id, length uint32
		want       int
		recycled   bool
	}{
		{"valid", 0, 76, 64, true},
		{"largest", 0, bufSize, bufSize - hdrLen, true},
		{"oversize", 0, bufSize + 1, 0, true},
		{"short", 0, hdrLen - 1, 0, true},
		{"id-outside-ring", 2, 76, 0, false},
		{"id-truncated-to-valid", 1 << 16, 76, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := make([]uint64, 2048)
			regs := make([]uint64, 128)
			base := uintptr(unsafe.Pointer(&mem[0]))
			n := &Net{Base: uintptr(unsafe.Pointer(&regs[0])), qsize: 2,
				rx: vq{desc: base, avail: base + 64, used: base + 128, bufs: base + 256}}
			dev.Write16(n.rx.used+2, 1)
			dev.Write32(n.rx.used+4, tc.id)
			dev.Write32(n.rx.used+8, tc.length)
			dev.Copy(n.rx.bufs+hdrLen, bytes.Repeat([]byte{0x5a}, bufSize))
			out := bytes.Repeat([]byte{0xcc}, 8192)
			got, err := n.Receive(out)
			if err != nil || got != tc.want {
				t.Fatalf("Receive = %d, %v; want %d", got, err, tc.want)
			}
			if !bytes.Equal(out[:got], bytes.Repeat([]byte{0x5a}, got)) {
				t.Fatal("packet differs")
			}
			if !bytes.Equal(out[got:], bytes.Repeat([]byte{0xcc}, len(out)-got)) {
				t.Fatal("destination changed beyond packet")
			}
			if n.rx.lastUsed != 1 || (n.rx.availIdx == 1) != tc.recycled {
				t.Fatal("bad completion/recycle state")
			}
			runtime.KeepAlive(mem)
			runtime.KeepAlive(regs)
		})
	}
}

func TestTransmitRejectsOversizeBeforeTouchingDMA(t *testing.T) {
	for _, size := range []int{0, bufSize - hdrLen + 1} {
		mem := make([]uint64, 1024)
		regs := make([]uint64, 128)
		base := uintptr(unsafe.Pointer(&mem[0]))
		n := &Net{Base: uintptr(unsafe.Pointer(&regs[0])), qsize: 2,
			tx: vq{desc: base, avail: base + 64, used: base + 128, bufs: base + 256}}
		if err := n.Transmit(make([]byte, size)); err == nil {
			t.Errorf("accepted %d bytes", size)
		}
		if n.tx.availIdx != 0 {
			t.Error("invalid frame published to DMA")
		}
		runtime.KeepAlive(mem)
		runtime.KeepAlive(regs)
	}
}
