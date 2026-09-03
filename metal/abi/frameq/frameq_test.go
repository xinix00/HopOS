package frameq

import (
	"testing"
	"unsafe"
)

func testQueue(t *testing.T) *Queue {
	t.Helper()
	mem := make([]uint64, PageSize/8)
	base := uintptr(unsafe.Pointer(&mem[0]))
	Init(base)
	q := Open(base)
	t.Cleanup(func() { _ = mem })
	return q
}

func TestSubmitCompleteRoundTrip(t *testing.T) {
	q := testQueue(t)
	ok, notify := q.Submit(0x1234000, 1518, 7)
	if !ok || !notify {
		t.Fatalf("submit ok=%v notify=%v", ok, notify)
	}
	d, ok := q.Take()
	if !ok || d.Offset != 0x1234000 || d.Length != 1518 || d.Token != 7 {
		t.Fatalf("take %#v ok=%v", d, ok)
	}
	ok, notify = q.Complete(d.Token, d.Length, StatusOK)
	if !ok || !notify {
		t.Fatalf("complete ok=%v notify=%v", ok, notify)
	}
	done, ok := q.Reap()
	if !ok || done.Token != 7 || done.Length != 1518 || done.Status != StatusOK {
		t.Fatalf("reap %#v ok=%v", done, ok)
	}
}

func TestQueueBackpressureIncludesCompletions(t *testing.T) {
	q := testQueue(t)
	for i := uint32(0); i < Entries; i++ {
		if ok, _ := q.Submit(uint64(i)*2048, 1500, i); !ok {
			t.Fatalf("submit %d", i)
		}
		d, ok := q.Take()
		if !ok || d.Token != i {
			t.Fatalf("take %d: %#v %v", i, d, ok)
		}
		if ok, _ := q.Complete(i, 1500, StatusOK); !ok {
			t.Fatalf("complete %d", i)
		}
	}
	if ok, _ := q.Submit(0, 1, Entries); !ok {
		t.Fatal("submission ring should have room after HOP consumed it")
	}
	if _, ok := q.Take(); ok {
		t.Fatal("Take succeeded while completion ring was full")
	}
	if _, ok := q.Reap(); !ok {
		t.Fatal("completion ring did not contain work")
	}
	if _, ok := q.Take(); !ok {
		t.Fatal("Take did not resume after completion was reaped")
	}
}
