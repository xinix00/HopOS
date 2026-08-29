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
// statische arena (hwinit-tijd: niet alloceren); RemapGBToL2 was het
// experiment dat het bewees en blijft als bouwsteen.
const (
	tableL0Off    = 0xA000
	tableL1Off    = 0x4000
	tableL2Spare  = 0xB000 // vrije pagina's: +0xB000..+0xE000 (scratch begint op +0xE000)
	tableL2Spares = 3

	descTable = 0b11
	descBlock = 0b01
	// Device-nGnRnE 2MB-blok, execute-never: dezelfde bits als tamago's
	// deviceAttributes | TTE_BLOCK | TTE_EXECUTE_NEVER (arm64/mmu.go).
	deviceBlock2M = descBlock | 1<<10 | 0b10<<8 | 0<<2 | 0b11<<53
)

var spareUsed int

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
		dev.MB()
		*(*uint64)(unsafe.Pointer(l1e)) = uint64(l2) | descTable
		n++
	}
	dev.MB()
	tlbiAll()
	return n
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

// RemapGBToL2 vervangt het 1GB-blok voor de GB die pa bevat door een L2-tabel
// met 512 device-2MB-blokken op dezelfde adressen — zelfde attributen, andere
// tabeldiepte. false = geen vrije tabelpagina meer, of de ingang was al een
// tabel (dan is er niets te doen).
func RemapGBToL2(pa uintptr) bool {
	ramStart, _ := runtime.MemRegion()
	idx := (uint64(pa) >> 30) & 0x1FF
	l1e := uintptr(ramStart) + tableL1Off + uintptr(idx)*8
	if cur := *(*uint64)(unsafe.Pointer(l1e)); cur&0b11 == descTable {
		return true
	}
	if spareUsed >= tableL2Spares {
		return false
	}
	l2 := uintptr(ramStart) + tableL2Spare + uintptr(spareUsed)*4096
	spareUsed++
	gb := uint64(pa) &^ (1<<30 - 1)
	for i := uintptr(0); i < 512; i++ {
		*(*uint64)(unsafe.Pointer(l2 + i*8)) = (gb + uint64(i)<<21) | deviceBlock2M
	}
	dev.MB()
	*(*uint64)(unsafe.Pointer(l1e)) = uint64(l2) | descTable
	dev.MB()
	tlbiAll()
	return true
}

// In regs_arm64.s.
func tlbiAll()
