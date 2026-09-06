package el2

import (
	"runtime"
	"testing"
	"unsafe"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

func TestSMPHandoffOwnsPrivilegesAndFreezesRequest(t *testing.T) {
	var request, handoff [32]uint64
	src := uintptr(unsafe.Pointer(&request[0]))
	dst := uintptr(unsafe.Pointer(&handoff[0]))
	dev.Write64(src+layout.CtrlSMPSp, 0x50012000)
	PrepareSMP(dst, src, 0x80000000, 3, 0x81000000, 0x82000000)
	dev.Write64(src+layout.CtrlSMPSp, 0x50022000)
	for off, want := range map[uintptr]uint64{
		layout.CtrlSMPSp: 0x50012000, layout.CtrlS2Table: 0x80000000,
		layout.CtrlSlot: 3, layout.CtrlSMPMbox: 0x81000000,
		layout.CtrlVecPA: 0x82000000,
	} {
		if got := dev.Read64(dst + off); got != want {
			t.Errorf("handoff[%#x] = %#x, want %#x", off, got, want)
		}
	}
	// Node cores use the same mechanism in their already trusted page.
	PrepareSMP(src, src, 0, 0, 0, 0x83000000)
	if dev.Read64(src+layout.CtrlS2Table) != 0 || dev.Read64(src+layout.CtrlSMPSp) != 0x50022000 {
		t.Fatal("node handoff lost its profile or EL1 context")
	}
	if layout.CtxSMP%64 != 0 || layout.CtxSMP+len(handoff)*8 > layout.CtxLen {
		t.Fatal("SMP handoff does not fit its own context cachelines")
	}
	runtime.KeepAlive(&request)
	runtime.KeepAlive(&handoff)
}
