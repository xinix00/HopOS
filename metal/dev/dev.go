// Package dev bevat primitieven voor device-gemapt geheugen (alles buiten de
// eigen RAM-declaratie: slot-partities, control-pages, ringen). Device-nGnRnE
// eist gealigneerde toegang — Go's memmove/clear faulten daar — dus hier
// uitsluitend expliciete 8-byte-gealigneerde word-ops, met byte-ops voor
// staarten (bytes zijn per definitie gealigneerd).
package dev

import (
	"encoding/binary"
	"sync"
	"unsafe"
)

// Notify is het ene producer→consumer-doorbellcontract voor iedere
// architectuur. De producer roept het aan na een succesvolle leeg→niet-leeg
// overgang van een gedeelde ring. De implementatie mag per machine verschillen
// (ARM64: DSB+SEV; RISC-V: fence plus board-kick/failsafe), maar code boven dev
// kent en test uitsluitend deze betekenis — niet de instructie eronder.
func Notify() { SEV() }

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

// normalWindows: de vensters die met memattr.NormalNC van Device-nGnRnE naar
// Normal-NC zijn gezet (MarkNormal). Daarbinnen mag een kopie gewoon memmove
// zijn; daarbuiten blijft de 8-byte-discipline van device-geheugen. De lijst
// is kort (ringstaarten, twee DMA-regio's) en verandert alleen bij attach,
// dus een lineaire zoektocht per kopie kost niets naast de kopie zelf.
//
// Waarom dit ertoe doet: op de M4 kost één Device-store ~290ns, dus 1MiB in
// Write64-stappen is 38ms — 27MB/s voor élke ring en DMA-buffer (gemeten
// 03-09). Op Normal-NC doet de store-buffer write-combining en mag de
// compiler LDP/STP gebruiken: hetzelfde MiB in enkele milliseconden.
var (
	normalMu      sync.RWMutex
	normalWindows []normalWindow
)

// normalWindow: cached = Normal-WB (gecached, memattr.NormalWB), anders
// Normal-NC (ongecached, memattr.NormalNC). Het verschil zit in Push/Pull: een
// WB-venster deelt de ABI met lezers die de cache niet zien — de EL2-switcher
// op de app-core draait met de MMU uit en leest de ringkop en de deurbel dus
// rechtstreeks uit DRAM — dus daar is cache-onderhoud per publicatie de eis,
// precies zoals op riscv64 (share_riscv64.go). NC en device hebben dat niet.
type normalWindow struct {
	lo, hi uintptr
	cached bool
}

// MarkNormal registreert [addr, addr+size) als Normal-NC-geheugen. Aanroepen
// na een geslaagde memattr.NormalNC (die doet het zelf) — nooit voor een
// venster dat nog device-gemapt is: dan zou memmove ongealigneerde of vector-
// accesses op nGnRnE doen, en dat is een fault.
func MarkNormal(addr, size uintptr) { mark(addr, size, false) }

// MarkCached registreert [addr, addr+size) als Normal-WB-geheugen (gecached,
// inner-shareable): memmove voor de payload, en Push/Pull doen er echt
// cache-onderhoud. Aanroepen na een geslaagde memattr.NormalWB.
func MarkCached(addr, size uintptr) { mark(addr, size, true) }

func mark(addr, size uintptr, cached bool) {
	if size == 0 {
		return
	}
	normalMu.Lock()
	normalWindows = append(normalWindows, normalWindow{addr, addr + size, cached})
	normalMu.Unlock()
}

// IsNormal meldt of [addr, addr+n) geheel in een geregistreerd Normal-venster
// ligt (NC of WB): dan mag een kopie memmove zijn.
func IsNormal(addr uintptr, n int) bool {
	_, ok := lookup(addr, n)
	return ok
}

// IsCached meldt of [addr, addr+n) geheel in een Normal-WB-venster ligt: dan
// horen bij Push en Pull echte clean/invalidate-stappen.
func IsCached(addr uintptr, n int) bool {
	w, ok := lookup(addr, n)
	return ok && w.cached
}

// lookup: het LAATST geregistreerde venster dat het bereik dekt wint. Een
// board mapt eerst een hele DMA-regio NC en daarna één blok erin WB (de
// databuffer); het jongere, kleinere venster is dan het juiste antwoord. De
// omgekeerde volgorde gaf op T21 (03-09) een WB-blok waar Push/Pull niets
// deden: memmove uit een cache die de DMA niet ziet.
func lookup(addr uintptr, n int) (normalWindow, bool) {
	normalMu.RLock()
	defer normalMu.RUnlock()
	end := addr + uintptr(n)
	for i := len(normalWindows) - 1; i >= 0; i-- {
		if w := normalWindows[i]; addr >= w.lo && end <= w.hi {
			return w, true
		}
	}
	return normalWindow{}, false
}

// Copy kopieert src naar fysiek adres dst (elke alignment).
func Copy(dst uintptr, src []byte) {
	n := len(src)
	if IsNormal(dst, n) {
		copy(unsafe.Slice((*byte)(unsafe.Pointer(dst)), n), src)
		return
	}
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
	if IsNormal(src, n) {
		copy(dst, unsafe.Slice((*byte)(unsafe.Pointer(src)), n))
		return
	}
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

// Move kopieert n bytes van fysiek adres src naar fysiek adres dst — beide
// device-geheugen, elke (ook verschillende) alignment. Via een vaste
// stack-buffer (geen heap): zo kan de kern een image de slot-partitie in
// streamen zonder ooit het hele bestand in de eigen RAM te houden (de
// download-in-app-memory-aanpak: core 0 blijft klein, een op-hol-geslagen
// image raakt hooguit zijn eigen partitie).
func Move(dst, src uintptr, n uint64) {
	var buf [4096]byte
	for i := uint64(0); i < n; {
		c := n - i
		if c > uint64(len(buf)) {
			c = uint64(len(buf))
		}
		CopyOut(buf[:c], src+uintptr(i))
		Copy(dst+uintptr(i), buf[:c])
		i += c
	}
}

// MB, SEV en CleanInv (barrières, core-wakeup, cache-onderhoud) staan per
// doel apart: dev_tamago.go (assembly, het board) en dev_host.go (no-ops,
// unit-tests op de ontwikkelmachine).
