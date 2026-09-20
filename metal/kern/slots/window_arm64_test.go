package slots

import (
	"testing"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/kern/stage2"
)

func TestLargeARMPartitionAdmissionAndRelease(t *testing.T) {
	const poolBase = uint64(0x10000000000)
	const poolSize = uint64(64 << 30)
	poolReset(t, []layout.Region{{Base: poolBase, Size: poolSize}})
	if got := PoolLargest(); got != poolSize-2<<20 {
		t.Fatalf("largest partition = %d MiB, want pool minus table reserve", got>>20)
	}
	for _, size := range []uint64{2 << 30, 5 << 30, 20 << 30} {
		if got := cageLinkWindow(size); got != size {
			t.Fatalf("window for %d = %d", size, got)
		}
		base, grown, err := partAlloc(1, size)
		if err != nil || grown != size {
			t.Fatalf("allocate %d: %#x + %#x, %v", size, base, grown, err)
		}
		claim, err := claimSize(size)
		if err != nil {
			t.Fatal(err)
		}
		if freeSpan(base, base+claim) {
			t.Fatal("active claim is free")
		}
		if _, _, err := partAlloc(1, size); err == nil {
			t.Fatal("duplicate owner accepted")
		}
		// Same visible PartSize survives handoff; deterministic table reservation
		// is adopted and released with it, without another handoff field.
		poolReset(t, []layout.Region{{Base: poolBase, Size: poolSize}})
		if err := partAdopt(1, base, size); err != nil {
			t.Fatal(err)
		}
		if err := partAdopt(2, base+size, 2<<20); cageReserve(size) > 0 && err == nil {
			t.Fatal("table storage adopted by another owner")
		}
		partRelease(2)
		partRelease(1)
		if !freeSpan(poolBase, poolBase+poolSize) {
			t.Fatalf("claim %d did not return completely", size)
		}
	}
	if got := cageLinkWindow(^uint64(0)); got != stage2.IPALimit-cageLinkBase() {
		t.Fatalf("address limit = %#x", got)
	}
}
