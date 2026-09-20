package stage2

import (
	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"testing"
)

func TestInheritedGrant(t *testing.T) {
	const slot = 7
	const size = uint64(8<<20) - 3
	setup := func() {
		t.Helper()
		if _, err := Build(slot, layout.SlotBase(1), tPoolPA, 4<<20); err != nil {
			t.Fatal(err)
		}
	}
	setup()
	if has, err := HasGrantWindow(slot, tFbPA, size); has || err != nil {
		t.Fatalf("empty: %v %v", has, err)
	}
	for _, pa := range []uint64{tFbPA, 0x1bc7a0000, 0x40000000} {
		setup()
		if err := GrantWindow(slot, pa, size); err != nil {
			t.Fatal(err)
		}
		if has, err := HasGrantWindow(slot, pa, size); !has || err != nil {
			t.Fatalf("valid: %v %v", has, err)
		}
		if _, err := HasGrantWindow(slot, pa, size-0x1000); err == nil {
			t.Fatal("accepted broader inherited mapping")
		}
		if _, err := HasGrantWindow(slot, pa, size+0x1000); err == nil {
			t.Fatal("accepted partial inherited mapping")
		}
	}
	base := layout.CageTablePA(slot)
	for _, tc := range []struct {
		name  string
		off   uintptr
		value uint64
	}{
		{"foreign root", l1Off + uintptr(layout.FbIPA>>30)*8, 0x1003},
		{"foreign edge", l2FbOff + uintptr(layout.FbIPA>>21)*8, 0x1003},
		{"missing page", l3FbHeadOff + uintptr((tFbPA&((2<<20)-1))>>12)*8, 0},
		{"read only", l3FbHeadOff + uintptr((tFbPA&((2<<20)-1))>>12)*8, tFbPA | (pageRWNC &^ attrRW)},
		{"extra mapping", l2FbOff, 0x40000000 | blockRWNC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup()
			if err := GrantWindow(slot, tFbPA, size); err != nil {
				t.Fatal(err)
			}
			dev.Write64(base+tc.off, tc.value)
			if has, err := HasGrantWindow(slot, tFbPA, size); has || err == nil {
				t.Fatalf("accepted malformed: %v %v", has, err)
			}
		})
	}
	for _, pa := range []uint64{0, ^uint64(0) - 1, 1 << 48} {
		if has, err := HasGrantWindow(slot, pa, 4096); has || err == nil {
			t.Fatal("accepted invalid window")
		}
	}
}
