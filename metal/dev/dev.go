// Package dev bevat primitieven voor device-gemapt geheugen (alles buiten de
// eigen RAM-declaratie: slot-partities, control-pages, ringen). Device-nGnRnE
// eist gealigneerde toegang — Go's memmove/clear faulten daar — dus hier
// uitsluitend expliciete 8-byte-gealigneerde word-ops, met byte-ops voor
// staarten (bytes zijn per definitie gealigneerd).
package dev

import (
	"encoding/binary"
	"unsafe"
)

// Read64/Write64: gealigneerde 64-bit toegang op fysiek adres.
func Read64(addr uintptr) uint64 {
	return *(*uint64)(unsafe.Pointer(addr))
}

func Write64(addr uintptr, v uint64) {
	*(*uint64)(unsafe.Pointer(addr)) = v
}

// Read32/Write32: gealigneerde 32-bit toegang (MMIO-registers).
func Read32(addr uintptr) uint32 {
	return *(*uint32)(unsafe.Pointer(addr))
}

func Write32(addr uintptr, v uint32) {
	*(*uint32)(unsafe.Pointer(addr)) = v
}

// Read8/Write8: byte-toegang (altijd gealigneerd; voor device-configruimte
// met byte-velden op oneven offsets, bv. virtio MAC).
func Read8(addr uintptr) uint8 {
	return *(*uint8)(unsafe.Pointer(addr))
}

func Write8(addr uintptr, v uint8) {
	*(*uint8)(unsafe.Pointer(addr)) = v
}

// Read16/Write16: gealigneerde 16-bit toegang (virtio-ringvelden).
func Read16(addr uintptr) uint16 {
	return *(*uint16)(unsafe.Pointer(addr))
}

func Write16(addr uintptr, v uint16) {
	*(*uint16)(unsafe.Pointer(addr)) = v
}

// Device-nGnRnE-geheugen eist natuurlijk gealigneerde toegang; een 64-bit
// store op een niet-8-gealigneerd adres abort. De onderstaande helpers doen
// daarom een byte-proloog tot 8-alignment, dan 8-byte-bulk, dan een
// byte-staart. Werkt voor elke start-alignment (bv. virtio-payload op +12).

func toAlign8(addr uintptr) int {
	return int(-addr & 7)
}

// Copy kopieert src naar fysiek adres dst (elke alignment).
func Copy(dst uintptr, src []byte) {
	n := len(src)
	i := 0
	for pro := toAlign8(dst); i < n && pro > 0; i, pro = i+1, pro-1 {
		*(*byte)(unsafe.Pointer(dst + uintptr(i))) = src[i]
	}
	for ; i+8 <= n; i += 8 {
		// De device-store blijft één gealigneerde Write64 (nGnRnE-eis); alleen
		// het samenstellen van het woord uit de (gecachte) slice gaat via
		// encoding/binary, dat de compiler op arm64 tot één load vouwt i.p.v.
		// 8 loads + 7 shifts.
		Write64(dst+uintptr(i), binary.LittleEndian.Uint64(src[i:]))
	}
	for ; i < n; i++ {
		*(*byte)(unsafe.Pointer(dst + uintptr(i))) = src[i]
	}
}

// CopyOut leest len(dst) bytes vanaf fysiek adres src (elke alignment) naar dst.
func CopyOut(dst []byte, src uintptr) {
	n := len(dst)
	i := 0
	for pro := toAlign8(src); i < n && pro > 0; i, pro = i+1, pro-1 {
		dst[i] = *(*byte)(unsafe.Pointer(src + uintptr(i)))
	}
	for ; i+8 <= n; i += 8 {
		// Idem CopyOut: de device-load blijft één gealigneerde Read64; het
		// uitpakken naar de (gecachte) slice gaat via encoding/binary (één
		// store op arm64 i.p.v. 8 stores + 7 shifts).
		binary.LittleEndian.PutUint64(dst[i:], Read64(src+uintptr(i)))
	}
	for ; i < n; i++ {
		dst[i] = *(*byte)(unsafe.Pointer(src + uintptr(i)))
	}
}

// Clear zet [dst, dst+n) op nul (elke alignment).
func Clear(dst uintptr, n uint64) {
	i := uint64(0)
	for pro := uint64(toAlign8(dst)); i < n && pro > 0; i, pro = i+1, pro-1 {
		*(*byte)(unsafe.Pointer(dst + uintptr(i))) = 0
	}
	for ; i+8 <= n; i += 8 {
		Write64(dst+uintptr(i), 0)
	}
	for ; i < n; i++ {
		*(*byte)(unsafe.Pointer(dst + uintptr(i))) = 0
	}
}

// MB is een volledige geheugenbarrière (DMB SY) — publiceer data vóór de
// index-update, en andersom bij het lezen. Zie dev_arm64.s.
func MB()
