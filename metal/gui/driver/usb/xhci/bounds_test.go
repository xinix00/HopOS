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
