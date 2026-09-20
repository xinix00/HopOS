package uefi

import (
	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"testing"
)

func TestCageOriginValidation(t *testing.T) {
	const base = 0x48000000
	const size = 32 << 20
	for _, v := range []struct {
		tag, version, base, size uint64
		ok                       bool
	}{
		{cageFactsTag, 1, base, size, true}, {0, 0, 0, 0, false}, {cageFactsTag, 2, base, size, false},
		{cageFactsTag, 1, base + 1, size, false}, {cageFactsTag, 1, base, 16 << 20, false},
		{cageFactsTag, 1, (1 << 48) - (2 << 20), size, false}, {cageFactsTag, 1, 0, size, false},
	} {
		r, e := decodeCageReservation(v.tag, v.version, v.base, v.size, size)
		if (e == nil) != v.ok {
			t.Fatalf("%+v got %+v %v", v, r, e)
		}
	}
}
func TestRepeatedFlipKeepsOneOwnerAndReleasesOldKernel(t *testing.T) {
	const ram = 128 << 20
	const carve = 32 << 20
	const footprint = ram + carve
	const cold = 0x40000000
	bank := []layout.Region{{Base: cold, Size: 1 << 30}}
	owner := reservedCarve(cold, ram, carve, cageReservation{})
	contains := func(pool []layout.Region, base, size uint64) bool {
		for _, r := range pool {
			if base >= r.Base && base+size <= r.Base+r.Size {
				return true
			}
		}
		return false
	}
	for gen, current := range []uint64{cold, 0x60000000, 0x70000000, 0x60000000} {
		carried, err := decodeCageReservation(cageFactsTag, cageFactsVersion, owner.Base, owner.Size, carve)
		if err != nil {
			t.Fatal(err)
		}
		owner = reservedCarve(current, ram, carve, carried)
		if owner.Base != cold+ram || owner.Size != carve {
			t.Fatal("owner moved", owner)
		}
		pool := layout.CarvePool(bank, cagePoolHoles(current, footprint, owner), 2<<20)
		for _, r := range pool {
			if r.Base < owner.Base+owner.Size && owner.Base < r.Base+r.Size {
				t.Fatal("persistent owner became allocatable", gen, r)
			}
			if r.Base < current+footprint && current < r.Base+r.Size {
				t.Fatal("current kernel became allocatable", gen, r)
			}
		}
		if gen > 0 && !contains(pool, cold, ram) {
			t.Fatal("old cold Go RAM did not return", gen, pool)
		}
		if gen == 2 && !contains(pool, 0x60000000, footprint) {
			t.Fatal("intermediate kernel carve leaked", pool)
		}
		if gen == 3 && !contains(pool, 0x70000000, footprint) {
			t.Fatal("second intermediate kernel carve leaked", pool)
		}
	}
}
