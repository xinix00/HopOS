package fb

import (
	"runtime"
	"testing"
	"unsafe"
)

func TestInvalidFramebufferCannotRemainActive(t *testing.T) {
	mem := make([]uint32, 64*16)
	p := uintptr(unsafe.Pointer(&mem[0]))
	defer runtime.KeepAlive(mem)
	Init(Desc{Base: p, Width: 64, Height: 16, Stride: 256, BPP: 32})
	if !Active() {
		t.Fatal("valid framebuffer rejected")
	}
	before := append([]uint32(nil), mem...)
	Header("header")
	before = append(before[:0], mem...)
	HeaderStatus(-1, "bad")
	for i, v := range mem {
		if v != before[i] {
			t.Fatal("negative header row wrote pixels")
		}
	}
	for _, d := range []Desc{{}, {Base: 1, Width: 64, Height: 16, Stride: 4, BPP: 32}, {Base: ^uintptr(0) - 8, Width: 64, Height: 16, Stride: 256, BPP: 32}} {
		Init(d)
		if Active() {
			t.Fatalf("invalid framebuffer active: %+v", d)
		}
	}
}
