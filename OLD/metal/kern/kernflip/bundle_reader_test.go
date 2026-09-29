package kernflip

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

type boundedBundleReader struct {
	*bytes.Reader
	maxRead int
	failAt  int64
	short   bool
	reads   int
}

func (r *boundedBundleReader) ReadAt(p []byte, off int64) (int, error) {
	r.reads++
	if len(p) > r.maxRead {
		return 0, errors.New("unbounded parser read")
	}
	if off == r.failAt {
		if r.short {
			return 0, nil
		}
		return 0, io.ErrUnexpectedEOF
	}
	return r.Reader.ReadAt(p, off)
}

func TestBundleReaderParityAndBoundedReads(t *testing.T) {
	relocs := make([]uint32, 2500)
	for i := range relocs {
		relocs[i] = uint32(i * 8)
	}
	data := buildBundle(t, 0x40000000, 1<<20, 0x40001000, relocs)
	r := &boundedBundleReader{Reader: bytes.NewReader(data), maxRead: 4096, failAt: -1}
	got, err := ParseBundleReader(r, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if got.ELF != nil || got.Relocs != nil {
		t.Fatal("reader parser copied payload/table")
	}
	if got.ELFSize() != 4096 || got.RelocCount() != len(relocs) || r.reads != 5 {
		t.Fatalf("wrong bounded metadata reads: %+v, reads%d", got, r.reads)
	}
	for _, i := range []int{0, 1023, 1024, 2048, 2499} {
		v, err := got.RelocAt(i)
		if err != nil || v != relocs[i] {
			t.Fatalf("reloc%d=%#x,%v", i, v, err)
		}
	}
	for _, i := range []int{-1, len(relocs)} {
		if _, err := got.RelocAt(i); err == nil {
			t.Fatal("out-of-bounds relocation accepted")
		}
	}
	var last [2]byte
	if n, err := got.ELFReader().ReadAt(last[:], 4095); n != 1 || !errors.Is(err, io.EOF) {
		t.Fatalf("ELF reader reached trailer: %d,%v", n, err)
	}
	legacy, err := ParseBundle(data)
	if err != nil || !bytes.Equal(legacy.ELF, data[:4096]) || len(legacy.Relocs) != len(relocs)*4 {
		t.Fatalf("compatibility: %v", err)
	}
}

func TestBundleReaderRejectsShortMetadataAndLateBadRelocation(t *testing.T) {
	relocs := make([]uint32, 2500)
	data := buildBundle(t, 0x40000000, 1<<20, 0x40001000, relocs)
	for _, off := range []int64{int64(len(data) - 16), 4096, 4096 + 56, 4096 + 56 + 4096} {
		for _, short := range []bool{false, true} {
			r := &boundedBundleReader{Reader: bytes.NewReader(data), maxRead: 4096, failAt: off, short: short}
			if _, err := ParseBundleReader(r, int64(len(data))); err == nil {
				t.Fatalf("accepted metadata read failure at%d, short%v", off, short)
			}
		}
	}
	binary.LittleEndian.PutUint32(data[4096+56+2499*4:], 1<<20)
	if _, err := ParseBundleReader(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("accepted invalid relocation in final chunk")
	}
	for _, n := range []int64{-1, 0, 100, int64(len(data) + 100)} {
		if _, err := ParseBundleReader(bytes.NewReader(data), n); err == nil {
			t.Fatalf("accepted size%d", n)
		}
	}
}

// A virtual 64MiB ELF cannot be read into the heap accidentally: this reader
// only serves the small trailer. ELF validity itself belongs to leanelf/place.
type metadataOnlyBundle struct {
	trailer []byte
	elfSize int64
	reads   int
}

func (r *metadataOnlyBundle) ReadAt(p []byte, off int64) (int, error) {
	r.reads++
	if off < r.elfSize || len(p) > 4096 {
		return 0, errors.New("parser tried to read ELF payload or oversized scratch")
	}
	return bytes.NewReader(r.trailer).ReadAt(p, off-r.elfSize)
}
func TestBundleReaderDoesNotLoadLargeELF(t *testing.T) {
	data := buildBundle(t, 0x40000000, 128<<20, 0x40001000, []uint32{64})
	tail := append([]byte(nil), data[4096:]...)
	const elfSize = 64 << 20
	binary.LittleEndian.PutUint64(tail[16:], elfSize)
	binary.LittleEndian.PutUint64(tail[len(tail)-16:], elfSize)
	r := &metadataOnlyBundle{trailer: tail, elfSize: elfSize}
	got, err := ParseBundleReader(r, elfSize+int64(len(tail)))
	if err != nil || got.ELFSize() != elfSize || r.reads != 3 {
		t.Fatalf("large ELF metadata: %+v,%v, reads%d", got, err, r.reads)
	}
}
