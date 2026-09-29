// Host-tests voor Build: het PA-plan wijst naar een heap-buffer, zodat de
// stage-2-tabellen in gewoon testgeheugen landen; een walker leest ze terug
// zoals de MMU dat zou doen. De overige plan-adressen zijn verzonnen PA's —
// Build schrijft ze alleen als descriptor-wáárde, dereferentie gebeurt nooit.
// Dít is de plek waar de isolatiebelofte een toetsbare eigenschap is: de map
// van slot i bevat geen enkel fysiek adres van slot j.
package stage2

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"unsafe"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

// Verzonnen PA's voor alles wat Build alleen als waarde encodeert.
const (
	tCtrlPA        = 0x1_0000_0000
	tBootScratchPA = 0x1_3000_0000
	tRevokeVecPA   = 0x1_4000_0000
	tPoolPA        = 0x2_0000_0000
)

// s2buf draagt de echte tabellen; package-var zodat de GC hem nooit opruimt
// terwijl layout er nog met een uintptr naar wijst.
var s2buf []byte
var extraTables = map[int]uint64{}

// Only table memory is real host RAM; the app payload remains an opaque PA
// interval ending at that table memory. Test cleanup releases the mapping.
func buildPartition(t *testing.T, cage int, ipa, size uint64) uint64 {
	t.Helper()
	pa := uint64(tPoolPA)
	delete(extraTables, cage)
	if reserve := TableReserve(ipa, size); reserve != 0 {
		// A non-fixed high address hint models a large physical partition
		// without allocating its payload. Unlike MAP_FIXED, an occupied hint
		// is never replaced. Only the 2MB table tail gets actual host pages.
		n := uintptr(reserve + (2 << 20))
		p, _, errno := syscall.Syscall6(syscall.SYS_MMAP, 1<<40, n,
			syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE, ^uintptr(0), 0)
		if errno != 0 {
			t.Fatalf("map host table memory: %v", errno)
		}
		t.Cleanup(func() {
			syscall.Syscall(syscall.SYS_MUNMAP, p, n, 0)
			delete(extraTables, cage)
		})
		tablePA := (uint64(p) + (2 << 20) - 1) &^ ((2 << 20) - 1)
		if tablePA < size || tablePA+reserve > 1<<44 {
			t.Fatalf("host table address %#x cannot model %#x-byte partition", tablePA, size)
		}
		pa = tablePA - size
		extraTables[cage] = tablePA
	}
	if l1, err := Build(cage, ipa, pa, size); err != nil {
		t.Fatal(err)
	} else if l1 != uint64(layout.CageTablePA(cage))+l1Off {
		t.Fatalf("wrong root %#x", l1)
	}
	return pa
}

func TestMain(m *testing.M) {
	// SlotCap-maat (niet MaxSlots): de slot-105-regressietest bouwt kooien
	// tot in de hoogste slots.
	s2buf = make([]byte, (layout.SlotCap+2)*layout.CageStride)
	base := (uintptr(unsafe.Pointer(&s2buf[0])) + 0xFFF) &^ 0xFFF // stage-2 table alignment
	layout.UsePlan(layout.Plan{
		NodeCtrlPA:    tCtrlPA,
		CagePA:        uint64(base),
		TrapVecPA:     tRevokeVecPA,
		BootScratchPA: tBootScratchPA,
		Pool:          []layout.Region{{Base: tPoolPA, Size: 1 << 30}},
	})
	os.Exit(m.Run())
}

func rd(pa uint64) uint64 { return dev.Read64(uintptr(pa)) }

// paOf haalt het uitgangsadres uit een descriptor (OA-bits [47:12]).
func paOf(d uint64) uint64 { return d & 0x0000_FFFF_FFFF_F000 }

type leaf struct {
	ipa, pa, size uint64
	rw            bool
}

// Walk all reachable descriptors. Tables may only inhabit this cage's fixed
// metadata or its additional trusted reservation, never arbitrary host memory.
func walk(t *testing.T, i int) []leaf {
	t.Helper()
	var out []leaf
	base := uint64(layout.CageTablePA(i))
	var walkTbl func(tbl, ipa uint64, level int)
	walkTbl = func(tbl, ipa uint64, level int) {
		inCage := tbl >= base && tbl+4096 <= base+layout.CageStride
		extra := extraTables[i]
		inExtra := extra != 0 && tbl >= extra && tbl+4096 <= extra+(2<<20)
		if (!inCage && !inExtra) || tbl&4095 != 0 {
			t.Fatalf("table pointer %#x outside cage %d metadata", tbl, i)
		}
		shift := uint(39 - level*9)
		for idx := uint64(0); idx < 512; idx++ {
			d := rd(tbl + idx*8)
			addr := ipa + idx<<shift
			switch {
			case d == 0:
			case level < 3 && d&3 == descTable:
				walkTbl(paOf(d), addr, level+1)
			case level == 2 && d&3 == descBlock:
				out = append(out, leaf{addr, paOf(d), 2 << 20, d>>6&3 == 3})
			case level == 3 && d&3 == descPage:
				out = append(out, leaf{addr, paOf(d), 4 << 10, d>>6&3 == 3})
			default:
				t.Fatalf("unexpected descriptor %#x at level %d index %d", d, level, idx)
			}
		}
	}
	walkTbl(base+l1Off, 0, 1)
	return out
}

func assertPartition(t *testing.T, cage int, ipa, pa, size uint64) {
	t.Helper()
	leaves := walk(t, cage)
	if uint64(len(leaves)) != size>>21 {
		t.Fatalf("%d leaves for %#x-byte partition, want %d", len(leaves), size, size>>21)
	}
	for n, l := range leaves {
		off := uint64(n) << 21
		if l.ipa != ipa+off || l.pa != pa+off || l.size != 2<<20 || !l.rw {
			t.Fatalf("leaf %d: %+v, want IPA %#x -> PA %#x, 2MB RW", n, l, ipa+off, pa+off)
		}
	}
}

func TestBuildPrivatePartition(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ipa, size uint64
	}{
		{"small", layout.SlotBase(1), 64 << 20},
		{"cross-first-GB", layout.SlotBase(1), (768 << 20) + (2 << 20)},
		{"two-GB", layout.SlotBase(1), 2 << 30},
		{"five-GB", layout.SlotBase(1), 5 << 30},
		{"full-inline-window", layout.SlotBase(1), (11 << 30) - (layout.SlotBase(1) & ((1 << 30) - 1))},
		{"twenty-GB-external-tables", layout.SlotBase(1), 20 << 30},
		{"full-canonical-window", layout.SlotBase(1), IPALimit - layout.SlotBase(1)},
		{"noncanonical", layout.SlotBase(3), 1 << 30},
		{"first-RAM-address", 1 << 30, 2 << 20},
		{"last-block", IPALimit - (2 << 20), 2 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const cage = 3
			pa := buildPartition(t, cage, tc.ipa, tc.size)
			assertPartition(t, cage, tc.ipa, pa, tc.size)
		})
	}
}

func cageBytes(i int) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(layout.CageTablePA(i))), layout.CageStride)
}

func TestBuildRejectsInvalidRangeWithoutWrites(t *testing.T) {
	const cage = 3
	const block = uint64(2 << 20)
	ipa := layout.SlotBase(1)
	if _, err := Build(cage, ipa, tPoolPA, 2<<30); err != nil {
		t.Fatal(err)
	}
	before := append([]byte(nil), cageBytes(cage)...)
	for _, tc := range []struct {
		name          string
		slot          int
		ipa, pa, size uint64
	}{
		{"zero-slot", 0, ipa, tPoolPA, block},
		{"high-slot", layout.MaxSlots + 1, ipa, tPoolPA, block},
		{"zero-size", cage, ipa, tPoolPA, 0},
		{"unaligned-IPA", cage, ipa + 4096, tPoolPA, block},
		{"unaligned-PA", cage, ipa, tPoolPA + 4096, block},
		{"unaligned-size", cage, ipa, tPoolPA, block + 4096},
		{"framebuffer-GB", cage, layout.FbIPA, tPoolPA, block},
		{"cross-IPA-limit", cage, ipa, tPoolPA, IPALimit - ipa + block},
		{"IPA-limit", cage, IPALimit, tPoolPA, block},
		{"IPA-wrap", cage, ^uint64(0) &^ (block - 1), tPoolPA, block},
		{"size-wrap", cage, ipa, tPoolPA, ^uint64(0) &^ (block - 1)},
		{"PA-wrap", cage, ipa, ^uint64(0) &^ (block - 1), block},
		{"PA-limit", cage, ipa, (1 << 44) - block, 2 * block},
		{"PA-table-reserve-limit", cage, ipa, (1 << 44) - (20 << 30), 20 << 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Build(tc.slot, tc.ipa, tc.pa, tc.size); err == nil {
				t.Fatal("invalid partition accepted")
			}
			if !bytes.Equal(before, cageBytes(cage)) {
				t.Fatal("rejected input changed an existing cage")
			}
		})
	}
}

func TestBuildIsolatesLargeNeighborPartitions(t *testing.T) {
	ipa := layout.SlotBase(1)
	size := uint64(5 << 30)
	if _, err := Build(1, ipa, tPoolPA, size); err != nil {
		t.Fatal(err)
	}
	neighbor := append([]byte(nil), cageBytes(1)...)
	if _, err := Build(2, ipa, tPoolPA+size, size); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(neighbor, cageBytes(1)) {
		t.Fatal("large cage build overwrote its neighbor")
	}
	assertPartition(t, 1, ipa, tPoolPA, size)
	assertPartition(t, 2, ipa, tPoolPA+size, size)
	// Exact coverage above excludes node scratch, cage metadata, and every
	// byte of the other partition, including its ABI tail.
}

func TestBuildHighCageUsesCanonicalIPA(t *testing.T) {
	old := layout.MaxSlots
	layout.SetMaxSlots(127)
	defer layout.SetMaxSlots(old)
	ipa := layout.SlotBase(1)
	if _, err := Build(105, ipa, tPoolPA, 2<<30); err != nil {
		t.Fatal(err)
	}
	assertPartition(t, 105, ipa, tPoolPA, 2<<30)
}

func TestRebuildClearsOldGBsAndFramebuffer(t *testing.T) {
	ipa := layout.SlotBase(1)
	buildPartition(t, 1, ipa, 20<<30)
	if err := GrantWindow(1, 0x3e108000, 8<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(1, ipa, tPoolPA, 8<<20); err != nil {
		t.Fatal(err)
	}
	assertPartition(t, 1, ipa, tPoolPA, 8<<20)
}

// A cage's table block also stores the independent secondary CPU context for
// the same-numbered core. Rebuilding the cage must not erase that live CPU.
func TestBuildPreservesIndependentSecondaryContext(t *testing.T) {
	const cage = 3
	secondary := layout.CageTablePA(cage) + layout.SMPCtxOff
	primary := layout.CageTablePA(cage) + layout.CtxOff
	for off := uintptr(0); off < layout.CtxLen; off += 8 {
		dev.Write64(secondary+off, 0x53504d0000000000|uint64(off))
	}
	dev.Write64(primary+layout.CtxState, layout.CtxDead)
	if _, err := Build(cage, layout.SlotBase(cage), tPoolPA, 5<<30); err != nil {
		t.Fatal(err)
	}
	for off := uintptr(0); off < layout.CtxLen; off += 8 {
		want := uint64(0x53504d0000000000) | uint64(off)
		if got := dev.Read64(secondary + off); got != want {
			t.Fatalf("secondary context overwritten at %#x: got %#x, want %#x", off, got, want)
		}
	}
	if got := dev.Read64(primary + layout.CtxState); got != layout.CtxEmpty {
		t.Fatalf("new cage retained old primary state %d", got)
	}
}

func TestTableReservationBoundary(t *testing.T) {
	ipa := layout.SlotBase(1)
	inlineMax := uint64(11<<30) - (ipa & ((1 << 30) - 1))
	for _, tc := range []struct{ size, want uint64 }{
		{0, 0}, {5 << 30, 0}, {inlineMax, 0}, {inlineMax + (2 << 20), 2 << 20},
		{20 << 30, 2 << 20}, {IPALimit - ipa, 2 << 20},
	} {
		if got := TableReserve(ipa, tc.size); got != tc.want {
			t.Fatalf("TableReserve(%#x, %#x)=%#x, want %#x", ipa, tc.size, got, tc.want)
		}
	}
}
