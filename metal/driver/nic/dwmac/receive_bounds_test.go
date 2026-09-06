//go:build !tamago

package dwmac

import (
	"bytes"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"runtime"
	"strconv"
	"testing"
	"unsafe"
)

// Exercise the real Receive path with a large caller buffer: a device's
// length must never expose bytes from the neighbouring DMA allocation.
func TestReceiveBoundsAndRecycle(t *testing.T) {
	for _, size := range []int{64, maxFrame - 4, maxFrame - 4 + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			mem := make([]uint64, 2048)
			regs := make([]uint64, 16384)
			base := uintptr(unsafe.Pointer(&mem[0]))
			regsBase := uintptr(unsafe.Pointer(&regs[0]))
			_ = regsBase
			n := &Net{Base: regsBase, rxDesc: base, rxBuf: base + 256}
			dev.Write32(base+descStatus, uint32(size+4)<<rxLenShift|rxStsFirst|rxStsLast)
			payload := bytes.Repeat([]byte{0x5a}, size)
			dev.Copy(base+256, payload)
			out := bytes.Repeat([]byte{0xcc}, 8192)
			got, err := n.Receive(out)
			want := size
			if size > maxFrame-4 {
				want = 0
			}
			if err != nil || got != want {
				t.Fatalf("Receive = %d, %v; want %d", got, err, want)
			}
			if got > 0 && !bytes.Equal(out[:got], payload) {
				t.Fatal("packet differs")
			}
			if !bytes.Equal(out[got:], bytes.Repeat([]byte{0xcc}, len(out)-got)) {
				t.Fatal("destination modified beyond returned packet")
			}
			if n.rxCur != 1 || dev.Read32(base+descStatus)&descOwn == 0 {
				t.Fatal("descriptor not recycled")
			}
			runtime.KeepAlive(mem)
			runtime.KeepAlive(regs)
		})
	}
}
