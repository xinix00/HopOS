package hopfs

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"
)

// A declared 400GiB device without a 400GiB host allocation. Large sequential
// tests track transferred bytes; small sparse tests retain the touched blocks.
type sparseDisk struct {
	blocks  map[uint64][]byte
	fail    bool
	written uint64
	discard bool
}

func (d *sparseDisk) Read(lba uint64, p []byte) error {
	for off := 0; off < len(p); off += BlockSize {
		clear(p[off : off+BlockSize])
		copy(p[off:off+BlockSize], d.blocks[lba+uint64(off/BlockSize)])
	}
	return nil
}
func (d *sparseDisk) Write(lba uint64, p []byte) error {
	if d.fail {
		return errors.New("injected write failure")
	}
	d.written += uint64(len(p))
	if d.discard {
		return nil
	}
	if d.blocks == nil {
		d.blocks = map[uint64][]byte{}
	}
	for off := 0; off < len(p); off += BlockSize {
		d.blocks[lba+uint64(off/BlockSize)] = append([]byte(nil), p[off:off+BlockSize]...)
	}
	return nil
}
func extentFS(d blockDevice, bytes uint64) *FS {
	return newRange(d, 0, bytes/BlockSize, BlockSize, 1<<20)
}

func TestDatabaseAndBackupExceedOldGlobalLimit(t *testing.T) {
	d := &sparseDisk{discard: true}
	f := extentFS(d, 400<<30)
	chunk := make([]byte, 1<<20)
	for _, name := range []string{"/database", "/backup"} {
		for off := uint64(0); off < 9<<30; off += uint64(len(chunk)) {
			if err := f.WriteAt(name, off, chunk); err != nil {
				t.Fatalf("%s at %d: %v", name, off, err)
			}
		}
	}
	if d.written != 18<<30 || f.index != 2 {
		t.Fatalf("bytes=%d extents=%d", d.written, f.index)
	}
	if err := f.Remove("/database"); err != nil {
		t.Fatal(err)
	}
	if f.index != 1 || len(f.free) != 1 {
		t.Fatalf("large removal grew index: %d/%d", f.index, len(f.free))
	}
}
func TestSparseFileUsesFull400GiBWindow(t *testing.T) {
	d := &sparseDisk{}
	f := extentFS(d, 400<<30)
	const off = uint64(400<<30) - 1
	if err := f.WriteAt("/large", off, []byte{0x79}); err != nil {
		t.Fatal(err)
	}
	if f.index != 1 || f.next != 1 {
		t.Fatalf("sparse metadata/data grew with offset: %d/%d", f.index, f.next)
	}
	got := make([]byte, 9)
	if n, err := f.ReadAt("/large", off-8, got); err != nil || n != 9 || !bytes.Equal(got, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0x79}) {
		t.Fatalf("sparse read %x n%d err%v", got, n, err)
	}
	if err := f.WriteAt("/large", off+1, []byte{1}); err == nil {
		t.Fatal("wrote outside disk window")
	}
	if err := f.Truncate("/hole", 400<<30); err != nil {
		t.Fatal(err)
	}
	if f.index != 1 {
		t.Fatal("sparse truncate allocated index")
	}
}
func TestTruncateAndRecycledWriteFailureDoNotExposeOldData(t *testing.T) {
	d := &sparseDisk{}
	f := extentFS(d, 2*BlockSize)
	if err := f.WriteAt("/a", 0, bytes.Repeat([]byte{0xA5}, BlockSize)); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate("/a", 3); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate("/a", BlockSize); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, BlockSize)
	f.ReadAt("/a", 0, got)
	if !bytes.Equal(got[:3], []byte{0xA5, 0xA5, 0xA5}) || !bytes.Equal(got[3:], make([]byte, BlockSize-3)) {
		t.Fatal("discarded truncate tail returned")
	}
	if err := f.Remove("/a"); err != nil {
		t.Fatal(err)
	}
	d.fail = true
	if err := f.WriteAt("/b", 0, []byte{9}); err == nil {
		t.Fatal("missing injected failure")
	}
	if f.index != 0 || f.next != 0 || len(f.free) != 0 {
		t.Fatal("failed fresh write leaked ownership")
	}
	d.fail = false
	if err := f.Truncate("/b", BlockSize); err != nil {
		t.Fatal(err)
	}
	f.ReadAt("/b", 0, got)
	if !bytes.Equal(got, make([]byte, BlockSize)) {
		t.Fatal("failed write exposed previous owner")
	}
}
func TestExtentBudgetFailureRollsBackAllocation(t *testing.T) {
	f := extentFS(&sparseDisk{}, 400<<30)
	f.index = maxIndexExtents
	if err := f.WriteAt("/x", 100<<30, []byte{1}); err == nil {
		t.Fatal("unbounded fragmentation")
	}
	if f.next != 0 || len(f.free) != 0 {
		t.Fatal("budget refusal leaked blocks")
	}
	f.index = 0
	if err := f.WriteAt("/x", 100<<30, []byte{1}); err != nil {
		t.Fatal(err)
	}
}
func TestFragmentedRandomIOAndReuse(t *testing.T) {
	d := &sparseDisk{}
	f := extentFS(d, 128*BlockSize)
	rng := rand.New(rand.NewSource(13))
	model := map[string][]byte{"/a": nil, "/b": nil, "/c": nil}
	for step := 0; step < 700; step++ {
		name := []string{"/a", "/b", "/c"}[rng.Intn(3)]
		want := model[name]
		switch rng.Intn(4) {
		case 0:
			if err := f.Remove(name); err != nil && !IsNotExist(err) {
				t.Fatal(err)
			}
			model[name] = nil
		case 1:
			size := rng.Intn(12 * BlockSize)
			if err := f.Truncate(name, uint64(size)); err != nil {
				t.Fatal(err)
			}
			n := make([]byte, size)
			copy(n, want)
			model[name] = n
		default:
			off := rng.Intn(10 * BlockSize)
			p := make([]byte, 1+rng.Intn(2*BlockSize))
			rng.Read(p)
			if err := f.WriteAt(name, uint64(off), p); err != nil {
				t.Fatal(err)
			}
			if off+len(p) > len(want) {
				n := make([]byte, off+len(p))
				copy(n, want)
				want = n
			}
			copy(want[off:], p)
			model[name] = want
		}
		for name, want := range model {
			if want == nil {
				continue
			}
			got := make([]byte, len(want))
			n, err := f.ReadAt(name, 0, got)
			if err != nil || n != len(want) || !bytes.Equal(got, want) {
				t.Fatalf("step%d %s n%d err%v dataequal%v", step, name, n, err, bytes.Equal(got, want))
			}
		}
	}
	for name := range model {
		f.Remove(name)
	}
	if f.index != 0 || f.next != 0 || len(f.free) != 0 {
		t.Fatalf("cleanup index%d next%d free%v", f.index, f.next, f.free)
	}
}

func TestFull400GiBAllocationAndRelease(t *testing.T) {
	d := &sparseDisk{discard: true}
	f := extentFS(d, 400<<30)
	chunk := make([]byte, 1<<20)
	for off := uint64(0); off < 400<<30; off += uint64(len(chunk)) {
		if err := f.WriteAt("/full", off, chunk); err != nil {
			t.Fatalf("at %d: %v", off, err)
		}
	}
	if f.index != 1 || f.next != f.max || d.written != 400<<30 {
		t.Fatalf("index%d used%d/%d bytes%d", f.index, f.next, f.max, d.written)
	}
	if err := f.WriteAt("/extra", 0, []byte{1}); err == nil {
		t.Fatal("allocation exceeded physical disk")
	}
	if err := f.Remove("/full"); err != nil {
		t.Fatal(err)
	}
	if f.index != 0 || f.next != 0 || len(f.free) != 0 {
		t.Fatal("full disk release retained per-block metadata")
	}
	if err := f.WriteAt("/again", 0, chunk); err != nil {
		t.Fatal(err)
	}
}

func TestHoleFillMergesBothNeighborsAtIndexBudget(t *testing.T) {
	d := &sparseDisk{}
	f := extentFS(d, 3*BlockSize)
	// Arrange a physically contiguous hole between two file extents.
	if err := f.WriteAt("/x", 0, make([]byte, BlockSize)); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteAt("/temporary", 0, make([]byte, BlockSize)); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteAt("/x", 2*BlockSize, make([]byte, BlockSize)); err != nil {
		t.Fatal(err)
	}
	if err := f.Remove("/temporary"); err != nil {
		t.Fatal(err)
	}
	f.index = maxIndexExtents
	if err := f.WriteAt("/x", BlockSize, make([]byte, BlockSize)); err != nil {
		t.Fatal(err)
	}
	if f.index != maxIndexExtents-1 || len(f.root.children["x"].extents) != 1 {
		t.Fatal("both-side merge failed")
	}
}
