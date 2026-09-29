package vcmail

import (
	"runtime"
	"testing"
	"unsafe"
)

func TestPendingMailboxCannotOverwriteBuffer(t *testing.T) {
	regs := make([]uint32, 16)
	regs[mbox0Status/4] = statusEmpty
	p := uintptr(unsafe.Pointer(&regs[0]))
	defer runtime.KeepAlive(regs)
	pendingAddr = 0x1008
	t.Cleanup(func() { pendingAddr = 0 })
	m := Mbox{Base: p, Buf: 0x1000} // unmapped buffer: no write is permitted
	if m.Call(1, []uint32{1}) {
		t.Fatal("unconfirmed request accepted")
	}
	if pendingAddr != 0x1008 || regs[mbox1Write/4] != 0 {
		t.Fatal("new request published over old owner")
	}
}
func TestMailboxBoundsBeforeMMIO(t *testing.T) {
	for _, m := range []Mbox{{Buf: 1}, {Buf: 1 << 32}, {Buf: 0x1000}} {
		if m.Call(1, make([]uint32, bufferSize)) {
			t.Fatal("invalid buffer accepted")
		}
	}
}
