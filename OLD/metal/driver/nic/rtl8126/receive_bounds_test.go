//go:build !tamago

package rtl8126

import (
	"bytes"
	"runtime"
	"strconv"
	"testing"
	"unsafe"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// Exercise the real Receive path with a large caller buffer: a device's
// length must never expose bytes from the neighbouring DMA allocation, and
// the descriptor must be handed back to the NIC (DescOwn) afterwards.
func TestReceiveBoundsAndRecycle(t *testing.T) {
	for _, size := range []int{64, bufSize, bufSize + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			mem := make([]uint64, 2048)
			regs := make([]uint64, 16384)
			base := uintptr(unsafe.Pointer(&mem[0]))
			n := &Net{Base: uintptr(unsafe.Pointer(&regs[0])), rxRing: base, rxBufs: base + 256}
			// Hardware-writeback: Own weg, First+Last, lengte incl. 4 bytes FCS.
			dev.Write32(base, uint32(firstFrag|lastFrag)|uint32(size+4)&rxLenMask)
			payload := bytes.Repeat([]byte{0x5a}, size)
			dev.Copy(base+256, payload)
			out := bytes.Repeat([]byte{0xcc}, 8192)
			got, err := n.Receive(out)
			want := size
			if size > bufSize {
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
			if n.rxHead != 1 {
				t.Fatal("descriptor not recycled")
			}
			if dev.Read32(base)&descOwn == 0 {
				t.Fatal("descriptor not handed back to the NIC")
			}
			runtime.KeepAlive(mem)
			runtime.KeepAlive(regs)
		})
	}
}

// An error summary or a fragmented frame yields nothing but still recycles.
func TestReceiveDropsErrors(t *testing.T) {
	mem := make([]uint64, 2048)
	base := uintptr(unsafe.Pointer(&mem[0]))
	n := &Net{rxRing: base, rxBufs: base + 256}
	dev.Write32(base, uint32(firstFrag|lastFrag|rxRES)|100)
	if got, _ := n.Receive(make([]byte, 4096)); got != 0 || n.rxHead != 1 {
		t.Fatalf("error frame: got %d head %d", got, n.rxHead)
	}
	dev.Write32(base+16, uint32(firstFrag)|100) // geen LastFrag
	if got, _ := n.Receive(make([]byte, 4096)); got != 0 || n.rxHead != 2 {
		t.Fatalf("fragment: got %d head %d", got, n.rxHead)
	}
	runtime.KeepAlive(mem)
}
