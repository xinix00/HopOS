package acpi

import (
	"encoding/binary"
	"runtime"
	"testing"
	"unsafe"
)

var usbTestWords []uint64

func TestDSDTFirmwareRAMAndChecksum(t *testing.T) {
	words := make([]uint64, 8)
	usbTestWords = words
	defer func() { usbTestWords = nil }()
	b := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), 64)
	copy(b, "DSDT")
	binary.LittleEndian.PutUint32(b[4:], 64)
	b[9] -= checksum(b)
	pa := uint64(uintptr(unsafe.Pointer(&words[0])))
	f := make([]byte, 148)
	binary.LittleEndian.PutUint32(f[40:], 0xdead0000)
	binary.LittleEndian.PutUint64(f[140:], pa)
	tables := &Tables{cache: map[string][]byte{"FACP": f}}
	if got := tables.DSDT(func(uint64, uint64) bool { return false }); got != nil {
		t.Fatal("unmapped pointer accepted")
	}
	readable := func(p, n uint64) bool { return p == pa && n <= 64 }
	if got := tables.DSDT(readable); len(got) != 64 {
		t.Fatal(len(got))
	}
	b[63]++
	if got := tables.DSDT(readable); got != nil {
		t.Fatal("bad checksum accepted")
	}
	runtime.KeepAlive(words)
}
