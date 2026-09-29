package stage2

import (
	"runtime"
	"testing"
	"unsafe"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

func TestEmptyFlipPreservesPoweredOnParkedCores(t *testing.T) {
	n := layout.NumAppCores()
	if n < 1 {
		t.Fatal("test requires an app core")
	}
	const table = uint64(0x1234000)
	for c := 0; c <= n; c++ {
		mb := layout.ParkMboxPA(c)
		for off := uintptr(0); off < layout.ParkMboxLen; off += 8 {
			dev.Write64(mb+off, 0xfeedface)
		}
		dev.Write64(mb, uint64(c%2)) // cold and previously powered-on, parked cores
	}
	checkParkedCores()
	initCoreSchedules(table, true)
	for c := 0; c <= n; c++ {
		mb := layout.ParkMboxPA(c)
		if got := dev.Read64(mb); got != uint64(c%2) {
			t.Fatalf("core %d: mailbox changed to %d; would dispatch a parked core through CPU_ON", c, got)
		}
		for off := uintptr(8); off < layout.ParkMboxLen; off += 8 {
			want := uint64(0)
			if off == layout.SchedS2PA {
				want = table
			}
			if got := dev.Read64(mb + off); got != want {
				t.Fatalf("core %d scheduler offset %#x: got %#x, want %#x", c, off, got, want)
			}
		}
	}
	// A true cold boot still removes arbitrary old RAM contents.
	initCoreSchedules(table, false)
	for c := 0; c <= n; c++ {
		if got := dev.Read64(layout.ParkMboxPA(c)); got != 0 {
			t.Fatalf("cold boot core %d mailbox = %d", c, got)
		}
	}
}

func TestEmptyFlipRejectsUnparkedCoreBeforeWrites(t *testing.T) {
	initCoreSchedules(0x1234000, false)
	mb := layout.ParkMboxPA(1)
	dev.Write64(mb, 0x50001000)
	dev.Write64(mb+8, 0x76540000)
	defer func() {
		if recover() == nil {
			t.Error("empty flip accepted an executing or unconfirmed core")
		}
		if dev.Read64(mb) != 0x50001000 || dev.Read64(mb+8) != 0x76540000 {
			t.Error("validation modified the existing dispatch")
		}
		initCoreSchedules(0, false)
	}()
	checkParkedCores()
}

func TestEmptyFlipLeavesExecutingParkInstructionsUntouched(t *testing.T) {
	old := preserveParked
	preserveParked = true
	defer func() { preserveParked = old }()
	initCoreSchedules(0, false)
	dev.Write64(layout.ParkMboxPA(1), 1)
	pc := layout.ParkCodePA()
	for off := uintptr(0); off < 48; off += 4 {
		dev.Write32(pc+off, 0xfeed0000+uint32(off))
	}
	initAppCoreRegion(uint64(layout.VecBasePA()), 0x12345678)
	for off := uintptr(0); off < 48; off += 4 {
		if got := dev.Read32(pc + off); got != 0xfeed0000+uint32(off) {
			t.Fatalf("park instruction at +%d overwritten: %#x", off, got)
		}
	}
	if dev.Read64(layout.ParkMboxPA(1)) != 1 {
		t.Fatal("parked core lost its sentinel during app-core initialization")
	}
	if dev.Read32(layout.VecBasePA()) != 0xA9010FE2 {
		t.Fatal("new exception thunks were not installed")
	}
}

// Check the installed HVC instruction stream: cache maintenance in the new
// kernel is too late to protect its first instruction fetch.
func TestChainloadInvalidatesInstructionsBeforeEntry(t *testing.T) {
	vec := make([]byte, 0x1000)
	va := (uintptr(unsafe.Pointer(&vec[0])) + 0x7ff) &^ 0x7ff
	plan := layout.Plan{NodeCtrlPA: tCtrlPA, CagePA: uint64(layout.VecBasePA()),
		TrapVecPA: tRevokeVecPA, BootScratchPA: tBootScratchPA,
		Pool: []layout.Region{{Base: tPoolPA, Size: 1 << 30}}}
	defer runtime.KeepAlive(vec)
	defer layout.UsePlan(plan)
	plan.TrapVecPA = uint64(va)
	layout.UsePlan(plan)
	InitVectors()
	// HVC #2 branches to word 13; the final branch at word 23 must be
	// preceded by exactly the established I_HYGIENE sequence (word 19 is the
	// x1 = 0 for the UEFI stub, after mov x0, x1).
	want := []uint32{0xd508751f, 0xd5033f9f, 0xd5033fdf, 0xd61f0200}
	for i, ins := range want {
		if got := dev.Read32(va + 0x400 + uintptr(20+i)*4); got != ins {
			t.Fatalf("chain instruction %d: %#x, want %#x", 20+i, got, ins)
		}
	}
	if got := dev.Read32(va + 0x400 + 19*4); got != 0xd2800001 {
		t.Fatalf("chain instruction 19: %#x, want movz x1, #0", got)
	}
}
