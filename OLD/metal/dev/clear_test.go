package dev

import (
	"runtime"
	"testing"
	"unsafe"
)

func TestClearNormalAndScalarBoundaries(t *testing.T) {
	for _, normal := range []bool{false, true} {
		for offset := range 8 {
			b := make([]byte, 1031)
			for i := range b {
				b[i] = 0x5a
			}
			base := uintptr(unsafe.Pointer(&b[0]))
			normalMu.Lock()
			old := normalWindows
			normalWindows = nil
			normalMu.Unlock()
			if normal {
				MarkNormal(base, uintptr(len(b)))
			}
			Clear(base+uintptr(offset), 1019)
			for i, v := range b {
				want := byte(0x5a)
				if i >= offset && i < offset+1019 {
					want = 0
				}
				if v != want {
					t.Fatalf("normal=%v offset=%d byte=%d got=%x", normal, offset, i, v)
				}
			}
			normalMu.Lock()
			normalWindows = old
			normalMu.Unlock()
			runtime.KeepAlive(b)
		}
	}
}
