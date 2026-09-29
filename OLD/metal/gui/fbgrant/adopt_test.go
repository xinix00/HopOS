package fbgrant

import (
	"github.com/xinix00/HopOS/metal/v2/driver/fb"
	"runtime"
	"testing"
	"unsafe"
)

func TestAdoptHolderIsExclusive(t *testing.T) {
	oldH, oldB, oldS := holder, base, size
	t.Cleanup(func() { holder, base, size = oldH, oldB, oldS })
	holder, base, size = 0, 0, 0
	pixels := make([]uint32, 64*16)
	defer runtime.KeepAlive(pixels)
	fb.Init(fb.Desc{Base: uintptr(unsafe.Pointer(&pixels[0])), Width: 64, Height: 16, Stride: 256, BPP: 32})
	if !fb.Active() {
		t.Fatal("test console not initialized")
	}
	if err := adoptHolder(3, 0x40000000, 8<<20); err != nil {
		t.Fatal(err)
	}
	if Holder() != 3 || base != 0x40000000 || size != 8<<20 || fb.Active() {
		t.Fatal("ownership or console not restored")
	}
	if err := adoptHolder(3, base, size); err != nil {
		t.Fatal(err)
	}
	if err := adoptHolder(4, base, size); err == nil {
		t.Fatal("duplicate owner accepted")
	}
	if Holder() != 3 {
		t.Fatal("duplicate replaced owner")
	}
}
