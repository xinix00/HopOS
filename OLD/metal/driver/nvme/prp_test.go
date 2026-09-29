package nvme

import (
	"runtime"
	"testing"
	"unsafe"
)

func TestDataPRPs(t *testing.T) {
	list := make([]uint64, prpListSize/8)
	c := &Controller{
		buf:     0x400000,
		prpList: uintptr(unsafe.Pointer(&list[0])),
	}

	prp1, prp2 := c.dataPRPs(dmaPageSize)
	if prp1 != uint64(c.buf) || prp2 != 0 {
		t.Fatalf("1 pagina: PRP1=%#x PRP2=%#x", prp1, prp2)
	}

	prp1, prp2 = c.dataPRPs(2 * dmaPageSize)
	if prp1 != uint64(c.buf) || prp2 != uint64(c.buf+dmaPageSize) {
		t.Fatalf("2 pagina's: PRP1=%#x PRP2=%#x", prp1, prp2)
	}

	prp1, prp2 = c.dataPRPs(maxTransferSize)
	if prp1 != uint64(c.buf) || prp2 != uint64(c.prpList) {
		t.Fatalf("1MiB: PRP1=%#x PRP2=%#x", prp1, prp2)
	}
	pages := uint64(maxTransferSize / dmaPageSize)
	for page := uint64(1); page < pages; page++ {
		if got, want := list[page-1], uint64(c.buf)+page*dmaPageSize; got != want {
			t.Fatalf("PRP-lijst[%d]=%#x, wil %#x", page-1, got, want)
		}
	}
	if list[pages-1] != 0 {
		t.Fatalf("eerste ongebruikte PRP-lijstentry=%#x, wil 0", list[pages-1])
	}
	runtime.KeepAlive(list)
}
