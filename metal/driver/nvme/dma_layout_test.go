package nvme

import "testing"

func TestCachedDataOwnsWholeBlock(t *testing.T) {
	const block = 2 << 20
	if DataOff%block != 0 || DataSize%block != 0 || maxTransferSize > DataSize {
		t.Fatal("payload is not contained in whole cache-mappable blocks")
	}
	for _, r := range [][2]int{
		{0, qEntries * sqeSize},
		{dmaPageSize, qEntries * cqeSize},
		{2 * dmaPageSize, qEntries * sqeSize},
		{3 * dmaPageSize, qEntries * cqeSize},
		{genericPRPOff, prpListSize},
	} {
		if r[0]+r[1] > DataOff {
			t.Fatalf("controller metadata overlaps cacheable payload: %v", r)
		}
	}
	if genericDMANeed < DataOff+DataSize {
		t.Fatal("DMA reservation excludes part of the cacheable block")
	}
}

func TestInitRejectsPartialCachedBlockBeforeMMIO(t *testing.T) {
	for _, size := range []uint64{DataOff + maxTransferSize, genericDMANeed - 1} {
		// Base is deliberately unmapped: budget validation must precede MMIO.
		c := &Controller{}
		if err := c.Init(0x200000, size); err == nil {
			t.Fatalf("accepted incomplete DMA reservation of %d bytes", size)
		}
	}
}
