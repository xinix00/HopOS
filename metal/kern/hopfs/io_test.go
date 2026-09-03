package hopfs

import (
	"bytes"
	"fmt"
	"testing"
)

type diskRun struct {
	lba   uint64
	bytes int
}

type recordingDisk struct {
	blockSize uint64
	data      []byte
	reads     []diskRun
	writes    []diskRun
}

func (d *recordingDisk) Read(lba uint64, p []byte) error {
	off := lba * d.blockSize
	if off+uint64(len(p)) > uint64(len(d.data)) {
		return fmt.Errorf("read buiten testdisk: %#x+%#x", off, len(p))
	}
	copy(p, d.data[off:off+uint64(len(p))])
	d.reads = append(d.reads, diskRun{lba: lba, bytes: len(p)})
	return nil
}

func (d *recordingDisk) Write(lba uint64, p []byte) error {
	off := lba * d.blockSize
	if off+uint64(len(p)) > uint64(len(d.data)) {
		return fmt.Errorf("write buiten testdisk: %#x+%#x", off, len(p))
	}
	copy(d.data[off:off+uint64(len(p))], p)
	d.writes = append(d.writes, diskRun{lba: lba, bytes: len(p)})
	return nil
}

func TestAaneengeslotenIOWordtPerMiBGebundeld(t *testing.T) {
	const diskBlockSize = 512
	disk := &recordingDisk{blockSize: diskBlockSize, data: make([]byte, 4<<20)}
	fs := newRange(disk, 0, uint64(len(disk.data))/diskBlockSize, diskBlockSize, 1<<20)

	want := make([]byte, 2<<20)
	for i := range want {
		want[i] = byte(i*13 + 7)
	}
	if err := fs.WriteAt("/data.bin", 0, want); err != nil {
		t.Fatal(err)
	}
	if got := disk.writes; len(got) != 2 || got[0].bytes != 1<<20 || got[1].bytes != 1<<20 {
		t.Fatalf("writes=%+v, wil twee runs van 1MiB", got)
	}

	got := make([]byte, len(want))
	if n, err := fs.ReadAt("/data.bin", 0, got); err != nil || n != len(got) {
		t.Fatalf("ReadAt n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("gebundelde roundtrip veranderde data")
	}
	if runs := disk.reads; len(runs) != 2 || runs[0].bytes != 1<<20 || runs[1].bytes != 1<<20 {
		t.Fatalf("reads=%+v, wil twee runs van 1MiB", runs)
	}
}

func TestContiguousRunStoptBijGatEnFragment(t *testing.T) {
	blocks := []uint32{10, 11, holeBlock, 12, 14, 15}
	if got := contiguousRun(blocks, 0, 6); got != 2 {
		t.Fatalf("run vóór gat=%d, wil 2", got)
	}
	if got := contiguousRun(blocks, 2, 6); got != 0 {
		t.Fatalf("run op gat=%d, wil 0", got)
	}
	if got := contiguousRun(blocks, 3, 3); got != 1 {
		t.Fatalf("run vóór fragment=%d, wil 1", got)
	}
	if got := contiguousRun(blocks, 4, 2); got != 2 {
		t.Fatalf("laatste run=%d, wil 2", got)
	}
}
