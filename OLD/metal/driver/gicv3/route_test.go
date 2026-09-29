package gicv3

import (
	"github.com/xinix00/HopOS/metal/v2/dev"
	"runtime"
	"testing"
	"unsafe"
)

func TestSPIRoutesItsOwnINTID(t *testing.T) {
	mem := make([]uint64, 0x8000/8)
	p := uintptr(unsafe.Pointer(&mem[0]))
	defer runtime.KeepAlive(mem)
	for _, id := range []int{32, 48, 1019} {
		if err := enableLine(p, 0, id, 0x1280000003); err != nil {
			t.Fatal(err)
		}
		if got := dev.Read64(p + 0x6000 + uintptr(id)*8); got != 0x1200000003 {
			t.Fatalf("id%d route=%#x", id, got)
		}
	}
	if got := dev.Read64(p + 0x6100 + 48*8); got != 0 {
		t.Fatalf("unrelated SPI80 route modified: %#x", got)
	}
	// Group 1: IGROUPR-bit gezet, ISENABLER-bit gezet (INTID 48 = reg 1, bit 16).
	if got := dev.Read32(p + 0x80 + 4); got&(1<<16) == 0 {
		t.Fatalf("INTID 48 not in Group 1: IGROUPR1=%#x", got)
	}
	if got := dev.Read32(p + 0x100 + 4); got&(1<<16) == 0 {
		t.Fatalf("INTID 48 not enabled: ISENABLER1=%#x", got)
	}
	if enableLine(0, 0, 1020, 0) == nil || enableLine(0, 0, -1, 0) == nil {
		t.Fatal("invalid INTID accepted")
	}
}
