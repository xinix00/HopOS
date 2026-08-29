// Host-test voor de boot_args-lezer: het blok wordt hier in heap-geheugen
// gebouwd met de getallen die deze mini écht meldt (gemeten 29-08 via m1n1),
// en via zijn adres gelezen — metal/dev doet op de host gewone memory-access.
package xnuboot

import (
	"encoding/binary"
	"testing"
	"unsafe"
)

func TestRead(t *testing.T) {
	b := make([]byte, 0x800)
	put16 := func(off int, v uint16) { binary.LittleEndian.PutUint16(b[off:], v) }
	put32 := func(off int, v uint32) { binary.LittleEndian.PutUint32(b[off:], v) }
	put64 := func(off int, v uint64) { binary.LittleEndian.PutUint64(b[off:], v) }

	put16(offRevision, 3)
	put16(offVersion, 2)
	put64(offVirtBase, 0x1d374000)
	put64(offPhysBase, 0x10001374000)
	put64(offMemSize, 0x5df56c000)
	put64(offTopOfKern, 0x10003b14000)
	put64(offVideoBase, 0x105e5304000)
	put64(offVideoStride, 2560)
	put64(offVideoW, 640)
	put64(offVideoH, 1136)
	put64(offVideoDepth, 30)
	put64(offDevTree, 0x1f27c000)
	put32(offDevTreeSize, 0x70000)
	put64(offCmdline+1024+8, 0x600000000)

	a, ok := Read(uintptr(unsafe.Pointer(&b[0])))
	if !ok {
		t.Fatal("geldige boot_args geweigerd")
	}
	if a.Revision != 3 || a.Version != 2 {
		t.Fatalf("revisie/versie %d/%d", a.Revision, a.Version)
	}
	if a.PhysBase != 0x10001374000 || a.MemSize != 0x5df56c000 {
		t.Fatalf("RAM %#x+%#x", a.PhysBase, a.MemSize)
	}
	if a.TopOfKernelData != 0x10003b14000 {
		t.Fatalf("top_of_kernel_data %#x", a.TopOfKernelData)
	}
	if a.MemSizeActual != 0x600000000 {
		t.Fatalf("mem_size_actual %#x (revisie 3 → cmdline 1024 bytes)", a.MemSizeActual)
	}
	// De device tree staat er virtueel in; wij willen het fysieke adres.
	if a.ADT != 0x1f27c000-0x1d374000+0x10001374000 {
		t.Fatalf("ADT %#x — virt→fys niet toegepast", a.ADT)
	}
	if a.ADTSize != 0x70000 {
		t.Fatalf("ADT-grootte %#x", a.ADTSize)
	}
	if a.FB.Base != 0x105e5304000 || a.FB.W != 640 || a.FB.H != 1136 {
		t.Fatalf("framebuffer %#x %dx%d", a.FB.Base, a.FB.W, a.FB.H)
	}
}

func TestRefusesNonsense(t *testing.T) {
	if _, ok := Read(0); ok {
		t.Fatal("nul-adres werd geaccepteerd")
	}
	b := make([]byte, 0x800) // alles nul: revisie 0
	if _, ok := Read(uintptr(unsafe.Pointer(&b[0]))); ok {
		t.Fatal("een leeg blok werd geaccepteerd")
	}
	binary.LittleEndian.PutUint16(b[offRevision:], 3)
	if _, ok := Read(uintptr(unsafe.Pointer(&b[0]))); ok {
		t.Fatal("een blok zonder RAM-contract werd geaccepteerd")
	}
}
