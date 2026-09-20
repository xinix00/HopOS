//go:build gui

package xhci

import (
	"errors"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"runtime"
	"testing"
	"unsafe"
)

func TestArenaRejectsWrappedAllocation(t *testing.T) {
	for _, a := range []arena{{cur: ^uintptr(0) - 8, end: ^uintptr(0)}, {cur: 16, end: 32}} {
		if _, err := a.alloc(^uintptr(0), 4096); err == nil {
			t.Fatal("wrapped allocation accepted")
		}
	}
}
func TestControlRefusesUnconfirmedOwnerBeforeTouchingDMA(t *testing.T) {
	h := &HC{poisoned: errors.New("unknown completion")}
	d := Device{hc: h}
	if _, err := d.control(0, 0, 0, 0, 0); err == nil {
		t.Fatal("control accepted")
	}
	if _, err := h.command(0, 0, 0, 0, "test"); err == nil {
		t.Fatal("command accepted")
	}
	h.poisoned = nil
	if _, err := d.control(0, 0, 0, 0, bufCtrlSize+1); err == nil {
		t.Fatal("oversized control accepted")
	}
}
func TestControllerTimeoutKeepsDMAOwned(t *testing.T) {
	regs := make([]uint64, 8)
	p := uintptr(unsafe.Pointer(&regs[0]))
	defer runtime.KeepAlive(regs)
	h := &HC{op: p, evt: &evring{base: p, n: 1, cycle: 1}}
	if _, err := h.waitEvent(func(event) bool { return false }, 0, "test"); err == nil || h.poisoned == nil {
		t.Fatal("timeout lost ownership")
	}
	dev.Write32(p+opUSBSts, 0)
	h.running = true
	if err := h.Start(1, 4096); err == nil {
		t.Fatal("running controller can overwrite DMA")
	}
}
func TestEP0DescriptorPacketSize(t *testing.T) {
	if n, err := descriptorMPS0(SpeedSuper, 9); err != nil || n != 512 {
		t.Fatalf("SuperSpeed: %d %v", n, err)
	}
	if _, err := descriptorMPS0(SpeedFull, 9); err == nil {
		t.Fatal("invalid full-speed packet size")
	}
}

// Exercise real Start allocations in ten unaligned slices of one DMA region.
// Root-only hosts need at most one device slot per physical port.
func TestTenRootHostsFitExistingDMAWindow(t *testing.T) {
	const size = 2 << 20
	memory := make([]byte, size)
	var pin runtime.Pinner
	pin.Pin(&memory[0])
	defer pin.Unpin()
	base := uintptr(unsafe.Pointer(&memory[0]))
	span := uintptr(size / 10)
	for i := 0; i < 10; i++ {
		regs := make([]uint64, 1024)
		pin.Pin(&regs[0])
		p := uintptr(unsafe.Pointer(&regs[0]))
		h := &HC{Base: p, op: p + 0x40, rt: p + 0x200, slots: 16, ports: 1 + i%2, ctx64: true, ac64: true}
		dev.Write32(h.op+opPageSize, 1)
		start := base + uintptr(i)*span
		dev.Write8(start+span-1, 0xab)
		if err := h.Start(start, span); err != nil {
			t.Fatalf("host%d: %v", i, err)
		}
		if h.nSlots != h.ports || len(h.res) != h.ports+1 {
			t.Fatalf("host%d slots=%d ports=%d", i, h.nSlots, h.ports)
		}
		if h.arena.cur > start+span || dev.Read8(start+span-1) != 0xab {
			t.Fatalf("host%d crossed DMA slice", i)
		}
		runtime.KeepAlive(regs)
	}
	runtime.KeepAlive(memory)
}
