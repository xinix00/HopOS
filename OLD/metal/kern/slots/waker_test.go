//go:build arm64 && !tamago

package slots

import (
	"runtime"
	"testing"
	"unsafe"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

type wakeTestBoard struct {
	board.Board
	kick func(int)
}

func (b wakeTestBoard) Cores() board.Cores {
	return board.Cores{App: func() []int { return []int{7} }, Kick: b.kick}
}

func TestSharedResidentWakeRoutes(t *testing.T) {
	oldMax := layout.MaxSlots
	t.Cleanup(func() { layout.SetMaxSlots(oldMax) })
	setCores(t, 1)
	layout.SetMaxSlots(8)
	admin := make([]byte, (layout.SlotCap+3)*layout.CageStride)
	ab := (uintptr(unsafe.Pointer(&admin[0])) + 4095) &^ uintptr(4095)
	layout.UsePlan(layout.Plan{NodeCtrlPA: uint64(ab), CagePA: uint64(ab), BootScratchPA: uint64(ab), Pool: []layout.Region{{Base: 0x80000000, Size: 32 << 20}}})
	oldBoard, oldHosts, oldSMP := board.Current(), hostCore, smpCores
	hostCore = make([]int, layout.SlotCap+1)
	smpCores = make([]int, layout.SlotCap+1)
	kicks := 0
	board.Use(wakeTestBoard{kick: func(phys int) {
		if phys != 7 {
			t.Fatalf("kicked physical core %d, want 7", phys)
		}
		kicks++
	}})
	t.Cleanup(func() {
		board.Use(oldBoard)
		hostCore, smpCores = oldHosts, oldSMP
		runtime.KeepAlive(admin)
	})
	// Cage IDs exceed the one available core; both residents sleep there.
	for _, i := range []int{4, 5} {
		hostCore[i] = 1
		ctxWrite(i, layout.CtxState, layout.CtxSaved)
		ctxWrite(i, layout.CtxWake, 1000)
	}
	dev.Write64(layout.ParkMboxPA(1), uint64(ab))
	ctxWrite(4, layout.CtxWake, 10)
	wakeSleeping(10)
	if kicks != 1 {
		t.Fatalf("due shared resident: %d kicks, want 1", kicks)
	}
	ctxWrite(4, layout.CtxWake, 1000)
	cp := ab + 0x20000
	head := cp + 0x1000
	ctxWrite(4, layout.CtxCtrlPA, uint64(cp))
	ctxWrite(4, layout.CtxRingHeadPA, uint64(head))
	dev.Write64(cp+layout.CtrlRXDoor, rxArmed|3)
	dev.Write64(head, 4)
	wakeRX(4)
	if kicks != 2 {
		t.Fatal("direct RX did not wake the shared core")
	}
	ctxWrite(4, layout.CtxWake, layout.CtxWakeNoPeek|1000)
	wakeSleeping(10)
	if kicks != 2 {
		t.Fatal("RX incorrectly made a no-peek runtime waiter due")
	}
	wakeSleeping(1000)
	if kicks != 4 { // both residents' deadlines are now due
		t.Fatal("no-peek waiter did not retain its deadline wake")
	}
}
