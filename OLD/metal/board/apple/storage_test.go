package apple

import "testing"

func TestStorageBuffersStayOutsideDriver(t *testing.T) {
	saved := storageNext
	defer func() { storageNext = saved }()
	storageNext = uintptr(StorageDMAPA) + StorageDriverReserved
	if p := StorageBuf(1); p != uintptr(StorageDMAPA)+StorageDriverReserved {
		t.Fatalf("bad start %#x", p)
	}
	if p := StorageBuf(^uint64(0)); p != 0 {
		t.Fatal("overflow accepted")
	}
	storageNext = uintptr(StorageDMAPA) + StorageDMASize - 0x4000
	if p := StorageBuf(0x4000); p == 0 {
		t.Fatal("last page refused")
	}
	if p := StorageBuf(1); p != 0 {
		t.Fatal("allocation beyond storage DMA")
	}
}
