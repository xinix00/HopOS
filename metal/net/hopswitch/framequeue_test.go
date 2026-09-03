package hopswitch

import (
	"bytes"
	"testing"
	"unsafe"

	"github.com/xinix00/HopOS/metal/v2/abi/frameq"
	"github.com/xinix00/HopOS/metal/v2/abi/layout"
)

type queueFixture struct {
	part []uint64
	tx   *frameq.Queue
	rx   *frameq.Queue
}

func newQueueFixture(t *testing.T) queueFixture {
	t.Helper()
	part := make([]uint64, 16<<10/8)
	txMem := make([]uint64, frameq.PageSize/8)
	rxMem := make([]uint64, frameq.PageSize/8)
	txBase := uintptr(unsafe.Pointer(&txMem[0]))
	rxBase := uintptr(unsafe.Pointer(&rxMem[0]))
	frameq.Init(txBase)
	frameq.Init(rxBase)
	t.Cleanup(func() { _, _, _ = part, txMem, rxMem })
	return queueFixture{part: part, tx: frameq.Open(txBase), rx: frameq.Open(rxBase)}
}

func (f queueFixture) base() uintptr { return uintptr(unsafe.Pointer(&f.part[0])) }

func useQueueFixtures(t *testing.T, fixtures ...queueFixture) {
	t.Helper()
	poolMem := make([]uint64, (64*frameChunkSize)/8)
	poolBase := uintptr(unsafe.Pointer(&poolMem[0]))

	mu.Lock()
	oldPorts, oldPool := ports, pool
	ports = make([]*port, layout.MaxSlots+1)
	if err := pool.configure(poolBase, uint64(len(poolMem))*8); err != nil {
		mu.Unlock()
		t.Fatal(err)
	}
	for i, f := range fixtures {
		ports[i+1] = &port{
			txq:      f.tx,
			rxq:      f.rx,
			partBase: uint64(f.base()),
			partSize: uint64(len(f.part)) * 8,
		}
	}
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		for _, pt := range ports {
			if pt != nil {
				freePendingLocked(pt)
			}
		}
		ports, pool = oldPorts, oldPool
		mu.Unlock()
		_ = poolMem
	})
}

func TestDescriptorQueuesMoveFrameBetweenCages(t *testing.T) {
	resetNAT()
	src, dst := newQueueFixture(t), newQueueFixture(t)
	useQueueFixtures(t, src, dst)

	frame := mkFrame(protoTCP, layout.SlotMAC(2), layout.SlotMAC(1),
		layout.SlotIP4(1), layout.SlotIP4(2), 1234, 4321, []byte("dynamic"))
	srcBytes := unsafe.Slice((*byte)(unsafe.Pointer(src.base())), len(frame))
	copy(srcBytes, frame)
	if ok, _ := src.tx.Submit(0, uint32(len(frame)), 11); !ok {
		t.Fatal("TX descriptor geweigerd")
	}
	const dstOff = 4096
	if ok, _ := dst.rx.Submit(dstOff, frameChunkSize, 22); !ok {
		t.Fatal("RX offer geweigerd")
	}

	if !switchPass(make([]byte, maxFrameLen)) {
		t.Fatal("switch zag geen descriptorwerk")
	}
	if done, ok := src.tx.Reap(); !ok || done.Token != 11 || done.Status != frameq.StatusOK {
		t.Fatalf("TX completion %#v ok=%v", done, ok)
	}
	done, ok := dst.rx.Reap()
	if !ok || done.Token != 22 || done.Length != uint32(len(frame)) || done.Status != frameq.StatusOK {
		t.Fatalf("RX completion %#v ok=%v", done, ok)
	}
	dstBytes := unsafe.Slice((*byte)(unsafe.Pointer(dst.base()+dstOff)), len(frame))
	if !bytes.Equal(dstBytes, frame) {
		t.Fatal("framepayload kwam niet ongewijzigd in de ontvangende kooi")
	}
}

func TestFrameWachtDynamischInPoolEnKeertTerug(t *testing.T) {
	dst := newQueueFixture(t)
	useQueueFixtures(t, dst)
	frame := make([]byte, 1500)
	mac := layout.SlotMAC(1)
	copy(frame[:6], mac[:])

	mu.Lock()
	before := pool.freeCount
	writeRXLocked(1, frame)
	pt := ports[1]
	if pt.pendingHead == 0 || pool.freeCount != before-1 {
		mu.Unlock()
		t.Fatalf("frame leende geen chunk: head=%d free=%d/%d", pt.pendingHead, pool.freeCount, before)
	}
	mu.Unlock()

	const dstOff = 8192
	if ok, _ := dst.rx.Submit(dstOff, frameChunkSize, 7); !ok {
		t.Fatal("RX offer geweigerd")
	}
	if !switchPass(make([]byte, maxFrameLen)) {
		t.Fatal("switch leverde gepoold frame niet")
	}
	if done, ok := dst.rx.Reap(); !ok || done.Token != 7 || done.Length != uint32(len(frame)) {
		t.Fatalf("RX completion %#v ok=%v", done, ok)
	}
	mu.Lock()
	defer mu.Unlock()
	if ports[1].pendingHead != 0 || pool.freeCount != before {
		t.Fatalf("chunk niet terug: head=%d free=%d/%d", ports[1].pendingHead, pool.freeCount, before)
	}
}

func TestOngeldigeTXDescriptorBlokkeertVolgendeFrameNiet(t *testing.T) {
	src := newQueueFixture(t)
	useQueueFixtures(t, src)
	if ok, _ := src.tx.Submit(0, 1, 3); !ok {
		t.Fatal("ongeldige descriptor kon niet worden aangeboden")
	}
	frame := make([]byte, 64)
	mac := layout.SlotMAC(1)
	copy(frame[6:12], mac[:])
	const off = 256
	copy(unsafe.Slice((*byte)(unsafe.Pointer(src.base()+off)), len(frame)), frame)
	if ok, _ := src.tx.Submit(off, uint32(len(frame)), 4); !ok {
		t.Fatal("geldige descriptor kon niet worden aangeboden")
	}

	buf := make([]byte, maxFrameLen)
	n, ok := readAppTXLocked(ports[1], buf)
	if !ok || n != len(frame) || !bytes.Equal(buf[:n], frame) {
		t.Fatalf("descriptor na fout niet gelezen: n=%d ok=%v", n, ok)
	}
	bad, ok := src.tx.Reap()
	if !ok || bad.Token != 3 || bad.Status == frameq.StatusOK {
		t.Fatalf("foute completion = %#v, ok=%v", bad, ok)
	}
	good, ok := src.tx.Reap()
	if !ok || good.Token != 4 || good.Status != frameq.StatusOK {
		t.Fatalf("goede completion = %#v, ok=%v", good, ok)
	}
}

func TestDynamischePoolHoudtRuimteVoorAndereOntvanger(t *testing.T) {
	a, b := newQueueFixture(t), newQueueFixture(t)
	useQueueFixtures(t, a, b)
	frame := make([]byte, 64)

	mu.Lock()
	for range 128 {
		enqueuePendingLocked(ports[1], frame)
	}
	aCount := ports[1].pendingCount
	for range 128 {
		enqueuePendingLocked(ports[2], frame)
	}
	bCount := ports[2].pendingCount
	free := pool.freeCount
	mu.Unlock()

	if aCount == 0 || bCount == 0 {
		t.Fatalf("één ontvanger monopoliseerde de pool: a=%d b=%d", aCount, bCount)
	}
	if aCount+bCount+free != 64 {
		t.Fatalf("chunkboekhouding: a=%d b=%d free=%d, wil totaal 64", aCount, bCount, free)
	}
}
