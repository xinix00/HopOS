package licheerv

import (
	"runtime"
	"unsafe"
)

// Volatile MMIO accessors (tamago's internal/reg is niet importeerbaar
// buiten de tamago module; dit is dezelfde aanpak).

//go:nosplit
func read32(addr uint64) uint32 {
	return *(*uint32)(unsafe.Pointer(uintptr(addr)))
}

//go:nosplit
func write32(addr uint64, val uint32) {
	*(*uint32)(unsafe.Pointer(uintptr(addr))) = val
	runtime.KeepAlive(val)
}

// Read32/Write32 zijn de geëxporteerde vorm voor de HOP-helft
// (board/licheerv/hop, die het reset-blok bedient) — de basis-helft houdt
// zijn eigen read32/write32 voor de runtime-hooks.
//
//go:nosplit
func Read32(addr uint64) uint32 { return read32(addr) }

//go:nosplit
func Write32(addr uint64, val uint32) { write32(addr, val) }
