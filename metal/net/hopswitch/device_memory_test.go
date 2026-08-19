package hopswitch

import (
	"syscall"
	"testing"
	"unsafe"
)

// testDeviceMemory gives ring tests memory outside the Go heap. Production
// addresses device/partition memory through uintptr too; passing a Go-heap
// slice through uintptr is therefore not an honest host model and trips
// checkptr as soon as dev dereferences that physical address. An anonymous
// mmap preserves that boundary while the race detector remains enabled for
// HopSwitch's Go state. Ring ordering itself is covered by the ring tests and
// the hardware gate because neither the detector nor Go owns device memory.
func testDeviceMemory(t *testing.T, size int) []byte {
	t.Helper()
	b, err := syscall.Mmap(-1, 0, size, syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap test-ring: %v", err)
	}
	t.Cleanup(func() {
		if err := syscall.Munmap(b); err != nil {
			t.Errorf("munmap test-ring: %v", err)
		}
	})
	return b
}

func testDeviceAddress(b []byte) uintptr {
	return uintptr(unsafe.Pointer(&b[0]))
}
