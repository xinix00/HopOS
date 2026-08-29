//go:build tamago && arm64

package apple

import (
	"runtime"
	"unsafe"

	"github.com/xinix00/HopOS/metal/dev"
)

// De stage-1-tabellen van de tamago-fork (hopos-highram) voor RAM boven 512GB:
// L0 op RamStart+0xA000, de L1 van de 512GB-regio mét het RAM op +0x4000 (1GB-
// blokken, behalve de RAM-GB die via een L2 op +0x6000 loopt), de L1 van de
// lage MMIO op +0x9000. Wij lezen die layout, we bouwen hem niet — vandaar
// constanten, zoals cpu/memattr dat voor de vlakke map doet.
//
// GEMETEN boot 4+5 (28-08): een 1GB-blokdescriptor met uitvoeradres ≥ 2^40
// geeft op dit silicium een abort ("address size fault, level 0") hoewel hij
// welgevormd is; dezelfde GB via een L2-tabel met 2MB-blokken werkt, en de lage
// MMIO-GB's (adres < 2^39) werken als 1GB-blok. Regel voor dit board dus: het
// DRAM (alles boven 2^40) via L2-tabellen. Dat staat hier en niet in de
// tamago-fork: daar zijn 1GB-blokken architectonisch correct en is dit een
// Apple-eigenaardigheid. MapDRAM doet het voor het hele DRAM bij boot uit een
// statische arena (hwinit-tijd: niet alloceren).
const (
	tableL0Off = 0xA000
	tableL1Off = 0x4000

	descTable = 0b11
	descBlock = 0b01
	// Device-nGnRnE 2MB-blok, execute-never: dezelfde bits als tamago's
	// deviceAttributes | TTE_BLOCK | TTE_EXECUTE_NEVER (arm64/mmu.go).
	deviceBlock2M = descBlock | 1<<10 | 0b10<<8 | 0<<2 | 0b11<<53
)

// De tabel-arena voor MapDRAM: één L2-pagina per GB DRAM, statisch (BSS) omdat
// dit vóór of buiten de heap moet kunnen draaien; één pagina extra voor de
// 4KB-uitlijning die Go een globale array niet garandeert.
const maxDRAMGB = 64 // tot 64GB-machines

var dramL2 [(maxDRAMGB + 1) * 4096]byte
var dramL2Used int

// MapDRAM zet elke GB van [base, base+size) om van 1GB-blok naar een L2-tabel
// met 2MB-device-blokken (GB's die al een tabel zijn — de RAM-GB — blijven).
// Geeft het aantal omgezette GB's; stopt als de arena op is.
func MapDRAM(base, size uint64) int {
	ramStart, _ := runtime.MemRegion()
	arena := (uintptr(unsafe.Pointer(&dramL2[0])) + 4095) &^ 4095
	n := 0
	for gb := base &^ (1<<30 - 1); gb < base+size; gb += 1 << 30 {
		idx := (gb >> 30) & 0x1FF
		l1e := uintptr(ramStart) + tableL1Off + uintptr(idx)*8
		if *(*uint64)(unsafe.Pointer(l1e))&0b11 == descTable {
			continue
		}
		if dramL2Used >= maxDRAMGB {
			break
		}
		l2 := arena + uintptr(dramL2Used)*4096
		dramL2Used++
		for i := uintptr(0); i < 512; i++ {
			*(*uint64)(unsafe.Pointer(l2 + i*8)) = (gb + uint64(i)<<21) | deviceBlock2M
		}
		// De verse tabel naar geheugen vegen vóór hij in gebruik komt. De
		// tabelwandelaar leest zijn descriptors niet noodzakelijk uit ónze
		// cache: op een core die net van "MMU en caches uit" naar "aan" is
		// gegaan — precies wat er met de zuinige core gebeurt bij de hop —
		// zag hij de oude 1GB-blokdescriptor in DRAM staan en niet de nieuwe
		// tabel in de cache. Gevolg: de eerste lees in dat GB gaf een
		// "address size fault, level 0", alsof de remap nooit gebeurd was
		// (GEMETEN 29-08, alleen op de gehopte core).
		dev.CleanInv(l2, 4096)
		dev.MB()
		*(*uint64)(unsafe.Pointer(l1e)) = uint64(l2) | descTable
		dev.CleanInv(l1e&^63, 64)
		n++
	}
	dev.MB()
	tlbiAll()
	return n
}

// Walk loopt de tabellen af zoals de hardware dat zou doen en geeft de vier
// descriptors terug (0 = niet bereikt). Het antwoord op "wat gebruikt de
// wandelaar hier eigenlijk" — een vraag die anders alleen als abort terugkomt.
func Walk(pa uintptr) (l0, l1, l2, l3 uint64) {
	ramStart, _ := runtime.MemRegion()
	a := uint64(pa)
	l0 = *(*uint64)(unsafe.Pointer(uintptr(ramStart) + tableL0Off + uintptr(a>>39&0x1FF)*8))
	if l0&0b11 != descTable {
		return
	}
	t1 := uintptr(l0 &^ 0xFFF)
	l1 = *(*uint64)(unsafe.Pointer(t1 + uintptr(a>>30&0x1FF)*8))
	if l1&0b11 != descTable {
		return
	}
	t2 := uintptr(l1 &^ 0xFFF)
	l2 = *(*uint64)(unsafe.Pointer(t2 + uintptr(a>>21&0x1FF)*8))
	if l2&0b11 != descTable {
		return
	}
	t3 := uintptr(l2 &^ 0xFFF)
	l3 = *(*uint64)(unsafe.Pointer(t3 + uintptr(a>>12&0x1FF)*8))
	return
}

// L1Entry geeft de L1-descriptor van de 512GB-regio met het RAM voor de GB
// die pa bevat (alleen zinvol voor pa in die regio).
func L1Entry(pa uintptr) uint64 {
	ramStart, _ := runtime.MemRegion()
	idx := (uint64(pa) >> 30) & 0x1FF
	return *(*uint64)(unsafe.Pointer(uintptr(ramStart) + tableL1Off + uintptr(idx)*8))
}

// L0Entry geeft L0[i].
func L0Entry(i int) uint64 {
	ramStart, _ := runtime.MemRegion()
	return *(*uint64)(unsafe.Pointer(uintptr(ramStart) + tableL0Off + uintptr(i)*8))
}

func tlbiAll()
