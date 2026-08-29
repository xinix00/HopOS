// sart.go — de SART, Apple's adresfilter voor de ANS (de NVMe-coprocessor).
//
// Waar een PCIe-device achter een DART hangt (een IOMMU met paginatabellen),
// staat voor de ANS een eenvoudiger zeef: zestien vensters van
// (adres, grootte, vlaggen), en wat daarbuiten valt komt er niet door. Een DMA
// van de coprocessor naar geheugen dat niet in een venster staat, gebeurt
// gewoon niet — zonder foutmelding, want het filter is de foutmelding niet.
//
// De vensters die de firmware al zette laten we met rust: die horen bij buffers
// die iBoot en de ANS-firmware onderling gebruiken, en een venster overschrijven
// dat nog in gebruik is zet de coprocessor stil. Wij zoeken een lege plek.
//
// Registerkaart: m1n1 src/sart.c, variant 3 — dat is wat de ADT van deze mini
// meldt (`sart-version = 3`, node sart-ans@85C50000).
package apple

import "github.com/xinix00/HopOS/metal/dev"

const (
	sartConfig  = 0x00 // + 4*index: vlaggen (0 = leeg)
	sartPAddr   = 0x40 // + 4*index: fysiek adres >> 12
	sartSize    = 0x80 // + 4*index: grootte >> 12
	sartShift   = 12
	sartAllow   = 0xff
	sartEntries = 16
	sartVersion = 3
)

// AllowDMA opent een venster voor de ANS op [paddr, paddr+size). Beide moeten
// op 4KB uitgelijnd zijn. false = geen SART bekend, verkeerde variant, scheve
// grenzen, of alle zestien vensters zijn al van de firmware.
func AllowDMA(paddr, size uint64) bool {
	_, _, _, sart := ANSAddrs()
	if sart == 0 || SARTVersion() != sartVersion {
		return false
	}
	const mask = 1<<sartShift - 1
	if paddr&mask != 0 || size&mask != 0 || size == 0 {
		return false
	}
	base := uintptr(sart)
	for i := uintptr(0); i < sartEntries; i++ {
		if dev.Read32(base+sartConfig+i*4) != 0 {
			continue // van de firmware
		}
		dev.Write32(base+sartPAddr+i*4, uint32(paddr>>sartShift))
		dev.Write32(base+sartSize+i*4, uint32(size>>sartShift))
		dev.Write32(base+sartConfig+i*4, sartAllow)
		dev.MB()
		return true
	}
	return false
}

// SARTWindows geeft de vensters die nu openstaan — voor diagnose: staat onze
// DMA-regio er niet in, dan komt er geen byte van de coprocessor aan.
func SARTWindows() (n int, first, firstSize uint64) {
	_, _, _, sart := ANSAddrs()
	if sart == 0 {
		return 0, 0, 0
	}
	base := uintptr(sart)
	for i := uintptr(0); i < sartEntries; i++ {
		if dev.Read32(base+sartConfig+i*4) == 0 {
			continue
		}
		if n == 0 {
			first = uint64(dev.Read32(base+sartPAddr+i*4)) << sartShift
			firstSize = uint64(dev.Read32(base+sartSize+i*4)) << sartShift
		}
		n++
	}
	return n, first, firstSize
}
