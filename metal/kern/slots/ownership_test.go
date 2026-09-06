//go:build !tamago

// Ownership-only fault-injection fixture. All MMIO points at host-owned buffers.
package slots

import (
	"runtime"
	"testing"
	"unsafe"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/abi/ring"
	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

type ownershipBoard struct {
	board.Board
	kick func(int)
}

func (b ownershipBoard) Cores() board.Cores {
	k := b.kick
	if k == nil {
		k = func(int) {}
	}
	return board.Cores{App: func() []int { return []int{1, 2, 3, 4} }, Kick: k, Start: func(int, uint64, uint64) error { return nil }}
}

func ownershipFixture(t *testing.T) (uint64, uint64) {
	t.Helper()
	setCores(t, 4)
	oldMax := layout.MaxSlots
	t.Cleanup(func() { layout.SetMaxSlots(oldMax) })
	layout.SetMaxSlots(8)
	admin := make([]byte, (layout.SlotCap+3)*layout.CageStride)
	part := make([]byte, 20<<20)
	ab := (uintptr(unsafe.Pointer(&admin[0])) + 0xfff) &^ uintptr(0xfff)
	pb := (uintptr(unsafe.Pointer(&part[0])) + (2 << 20) - 1) &^ uintptr((2<<20)-1)
	size := uint64(16 << 20)
	layout.UsePlan(layout.Plan{NodeCtrlPA: uint64(ab), CagePA: uint64(ab), TrapVecPA: uint64(ab), BootScratchPA: uint64(ab), Pool: []layout.Region{{Base: uint64(pb), Size: size}}})
	poolReset(t, layout.Pool())
	hostCore = make([]int, layout.SlotCap+1)
	smpCores = make([]int, layout.SlotCap+1)
	vectorsOnce.Do(func() {}) // hardware vectors are outside this host fixture
	oldBoard := board.Current()
	board.Use(ownershipBoard{})
	t.Cleanup(func() { board.Use(oldBoard); runtime.KeepAlive(admin); runtime.KeepAlive(part) })
	ring.Init(layout.RingOutboxAt(uint64(pb), size-layout.AbiTail), layout.RingDataCap)
	return uint64(pb), size
}

func TestOwnershipQuarantineRetainsOwnership(t *testing.T) {
	base, size := ownershipFixture(t)
	if err := partAdopt(3, base, size); err != nil {
		t.Fatal(err)
	}
	hostCore[3] = 1
	smpCores[3] = 2
	residentReset(1, 3)
	dev.Write64(layout.ParkMboxPA(1), base)
	releaseSlot(3, false)
	_, _, reserved := partitionOf(3)
	t.Logf("after failed stop: reserved=%v core=%d span=%d ctx=%d", reserved, coreOf(3), coreCount(3), ctxState(3))
	if !reserved || coreOf(3) != 1 || coreCount(3) != 2 || !ctxLive(ctxState(3)) {
		t.Fatal("quarantine lost ownership/state while memory is still reserved")
	}
}

func TestOwnershipQuarantinedSMPSecondStop(t *testing.T) {
	base, size := ownershipFixture(t)
	if err := partAdopt(1, base, size); err != nil {
		t.Fatal(err)
	}
	smpCores[1] = 2
	residentReset(1, 1)
	residentReset(2, 2)
	dev.Write64(layout.ParkMboxPA(1), 1)    // primary parked
	dev.Write64(layout.ParkMboxPA(2), base) // secondary has not acknowledged stop
	releaseSlot(1, false)                   // same completion used after Stop times out
	err := Stop(1, 0)
	_, _, reserved := partitionOf(1)
	t.Logf("retry Stop: err=%v reserved=%v secondaryRunning=%v", err, reserved, coreRunning(2))
	if err == nil && !reserved && coreRunning(2) {
		t.Fatal("retry freed memory without observing the still-running secondary")
	}
}

func TestOwnershipFlipCannotOmitQuarantine(t *testing.T) {
	base, size := ownershipFixture(t)
	if err := partAdopt(3, base, size); err != nil {
		t.Fatal(err)
	}
	hostCore[3] = 1
	smpCores[3] = 1
	residentReset(1, 3)
	dev.Write64(layout.ParkMboxPA(1), base)
	releaseSlot(3, false)
	states, err := SnapshotForFlip()
	t.Logf("snapshot after quarantine: states=%d err=%v hardware-running=%v", len(states), err, coreRunning(1))
	if err == nil && len(states) == 0 {
		t.Fatal("flip omits quarantined partition while its core may still execute")
	}
}

func TestOwnershipStopSerializesSMPDispatch(t *testing.T) {
	base, size := ownershipFixture(t)
	if err := partAdopt(1, base, size); err != nil {
		t.Fatal(err)
	}
	smpCores[1] = 2
	residentReset(1, 1)
	cp, _ := CtrlPageOf(1)
	ctxWrite(1, layout.CtxCtrlPA, uint64(cp))
	dev.Write64(layout.ParkMboxPA(1), 1)
	ctrlWrite(1, layout.CtrlSMPReq, 2)
	s := &servicer{slot: 1, stop: make(chan struct{}), done: make(chan struct{})}
	close(s.done)
	// Deterministic interleaving: an already-running servicer finishes its
	// dispatch after Stop's core scan, when teardown first tries to cancel it.
	s.cancel = func() { s.dispatchSMP() }
	servicers[1] = s
	err := Stop(1, 0)
	_, _, reserved := partitionOf(1)
	t.Logf("Stop with in-flight dispatch: err=%v reserved=%v secondaryRunning=%v", err, reserved, coreRunning(2))
	if err == nil && !reserved && coreRunning(2) {
		t.Fatal("SMP dispatch can complete after the last stop scan and before free")
	}
}

func TestNewOwnerMemoryIsClean(t *testing.T) {
	// Cross the cooperative chunk boundary, including an unaligned tail.
	b := make([]byte, (4<<20)+37)
	for i := range b {
		b[i] = 0xa5
	}
	Scrub(uintptr(unsafe.Pointer(&b[1])), uintptr(len(b)-2), nil)
	for i, v := range b[1 : len(b)-1] {
		if v != 0 {
			t.Fatalf("old owner byte survives at %d", i+1)
		}
	}
	if b[0] != 0xa5 || b[len(b)-1] != 0xa5 {
		t.Fatal("initialization crossed the assigned range")
	}
	runtime.KeepAlive(b)
}

func TestEmptyStopDoesNotTouchAnotherCageOnSameNumberedCore(t *testing.T) {
	base, size := ownershipFixture(t)
	if err := partAdopt(3, base, size); err != nil {
		t.Fatal(err)
	}
	hostCore[3] = 1
	residentReset(1, 3)
	dev.Write64(layout.ParkMboxPA(1), base)
	if err := Stop(1, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, owned := partitionOf(3); !owned || !coreRunning(1) || !ctxLive(ctxState(3)) {
		t.Fatal("empty cage Stop touched a different owner")
	}
}

func TestStopWakeUsesCageContextAndPhysicalCore(t *testing.T) {
	base, size := ownershipFixture(t)
	if err := partAdopt(3, base, size); err != nil {
		t.Fatal(err)
	}
	hostCore[3] = 1
	residentReset(1, 3)
	ctxWrite(3, layout.CtxState, layout.CtxSaved)
	ctxWrite(3, layout.CtxWake, ^uint64(0))
	dev.Write64(layout.ParkMboxPA(1), base)
	kicks := 0
	board.Use(ownershipBoard{kick: func(core int) {
		if core != 1 {
			t.Fatalf("wake aimed at core %d", core)
		}
		kicks++
	}})
	wakeForStop(3)
	if kicks != 1 || ctxRead(3, layout.CtxWake) != 0 {
		t.Fatal("stop did not wake the last shared resident")
	}
}

func TestPendingBootRingsAfterPublishingResident(t *testing.T) {
	base, size := ownershipFixture(t)
	if err := partAdopt(3, base, size); err != nil {
		t.Fatal(err)
	}
	hostCore[3] = 1
	residentReset(1, 1)
	ctxWrite(1, layout.CtxState, layout.CtxSaved)
	dev.Write64(layout.ParkMboxPA(1), base)
	kicks := 0
	board.Use(ownershipBoard{kick: func(core int) {
		if core != 1 || !residentListed(1, 3) || ctxState(3) != layout.CtxBootPending || ctxRead(3, layout.CtxBootArg) != 1234 {
			t.Fatal("boot rang before publishing its complete context")
		}
		kicks++
		ctxWrite(3, layout.CtxState, layout.CtxRunning) // switch acknowledgement
	}})
	if err := bootPendingDispatch(1, 3, 5678, 1234); err != nil {
		t.Fatal(err)
	}
	if kicks != 1 {
		t.Fatal("sleeping core was not notified of its new resident")
	}
}
