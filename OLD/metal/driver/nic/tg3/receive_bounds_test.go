//go:build !tamago

package tg3

import (
	"bytes"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"runtime"
	"testing"
	"unsafe"
)

func TestReceiveBoundsPayloadAndRecycle(t *testing.T) {
	for _, tc := range []struct {
		name        string
		index, size uint32
		want        int
	}{
		{"valid", 0, 68, 64},
		{"last-buffer", rxStdRing - 1, 68, 64},
		{"largest", 0, rxDMASize, rxDMASize - 4},
		{"oversize", 0, rxDMASize + 1, 0},
		{"invalid-index", rxStdRing, 68, 0},
		{"short", 0, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := make([]uint64, NeedBytes/8)
			regs := make([]uint64, 16384)
			base := uintptr(unsafe.Pointer(&mem[0]))
			n := &Net{Base: uintptr(unsafe.Pointer(&regs[0])), r: rings{dma: base}}
			dev.Write32(base+offStatus+16, 1)
			dev.Write32(base+offRxRet+8, tc.size)
			dev.Write32(base+offRxRet+28, tc.index|0x10000)
			if tc.index < rxStdRing {
				dev.Copy(base+offRxBuf+uintptr(tc.index)*rxBufSize, bytes.Repeat([]byte{0x5a}, rxDMASize))
			}
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
			if n.r.rxRetIdx != 1 || n.r.rxStdIdx != 1 || dev.Read32(n.Base+mbRxStdProd) != 1 {
				t.Fatal("descriptor not recycled")
			}
			runtime.KeepAlive(mem)
			runtime.KeepAlive(regs)
		})
	}
}
