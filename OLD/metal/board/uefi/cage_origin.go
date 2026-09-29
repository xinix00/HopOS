package uefi

import (
	"fmt"
	"github.com/xinix00/HopOS/metal/v2/abi/layout"
)

// Versioned extension within the unused firmware-facts header. The preceding
// firmware/config fields and their offsets remain unchanged.
const cageFactsTag uint64 = 0x3145474143504f48 // HOPCAGE1
const cageFactsVersion uint64 = 1
const cageFactsOffset = 88

type cageReservation struct{ Base, Size uint64 }

var persistentCage cageReservation

func decodeCageReservation(tag, version, base, size, expectedSize uint64) (cageReservation, error) {
	if tag != cageFactsTag || version != cageFactsVersion {
		return cageReservation{}, fmt.Errorf("uefi: missing or unsupported persistent cage origin; cold boot this kernel first")
	}
	if base == 0 || base&((2<<20)-1) != 0 || size != expectedSize || size == 0 || base >= 1<<48 || size > (1<<48)-base {
		return cageReservation{}, fmt.Errorf("uefi: invalid persistent cage reservation %#x+%#x", base, size)
	}
	return cageReservation{base, size}, nil
}

// reservedCarve always names the first kernel's administration owner. Later
// kernels' private carves are not retained when those kernels leave.
func reservedCarve(currentBase, carveOffset, carveSize uint64, inherited cageReservation) cageReservation {
	if inherited.Size != 0 {
		return inherited
	}
	return cageReservation{currentBase + carveOffset, carveSize}
}

// Same owner exclusion for a cold pool and every reconstructed FLIP pool.
func cagePoolHoles(currentBase, currentSize uint64, owner cageReservation) []layout.Region {
	return []layout.Region{{Base: currentBase, Size: currentSize}, {Base: owner.Base, Size: owner.Size}}
}
