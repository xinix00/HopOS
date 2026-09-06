package smpro

import (
	"github.com/xinix00/HopOS/metal/v2/fw/acpi"
	"runtime"
	"testing"
	"unsafe"
)

func TestPendingFirmwareOwnsBuffer(t *testing.T) {
	mem := []uint32{0x1234, 0, 0x5678, 0, 0}
	p := uintptr(unsafe.Pointer(&mem[0]))
	defer runtime.KeepAlive(mem)
	d := New(14, acpi.PCCSubspace{ShmemBase: uint64(p), ShmemLen: 20})
	d.pending = true
	if _, ok := d.SoCTemp(); ok {
		t.Fatal("unfinished call accepted")
	}
	if mem[0] != 0x1234 || mem[2] != 0x5678 || !d.pending {
		t.Fatal("firmware-owned buffer overwritten")
	}
}
