// Package frameq implements the fixed, shared descriptor queue between one
// caged app and HOP's Ethernet switch. The queue carries buffer descriptions,
// never frame payload. Payload lives in the app partition; HOP validates every
// offset against that partition before touching it.
//
// Both directions use the same page-sized queue and the same ownership:
//
//	app -- submit --> HOP -- complete --> app
//
// For TX a submission describes bytes the app filled. For RX it describes an
// empty buffer offered to HOP; the completion carries the received length.
// Indices are monotonic and each writer owns its own cache line.
package frameq

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

const (
	PageSize = 4 << 10
	Entries  = 64

	submitHeadOff = 0x000 // app writes
	submitTailOff = 0x040 // HOP writes
	doneHeadOff   = 0x080 // HOP writes
	doneTailOff   = 0x0c0 // app writes
	magicOff      = 0x100 // HOP writes once at Init

	submitOff  = 0x200
	submitSize = 16
	doneOff    = submitOff + Entries*submitSize
	doneSize   = 16

	Magic = 0x3151454d41524648 // "HFRAMEQ1" little endian
)

// CompletionHeadOff is the physical word the slot scheduler peeks while an
// app is asleep. It is exported as part of the queue/doorbell contract.
const CompletionHeadOff = doneHeadOff

const (
	StatusOK       = 0
	StatusBounds   = 1
	StatusTooSmall = 2
)

type Desc struct {
	Offset uint64
	Length uint32
	Token  uint32
}

type Done struct {
	Token  uint32
	Length uint32
	Status uint32
}

type Queue struct {
	base    uintptr
	corrupt bool
	why     string
}

func Init(base uintptr) {
	dev.Clear(base, PageSize)
	dev.Write64(base+magicOff, Magic)
	dev.Push(base, PageSize)
	dev.MB()
}

func Open(base uintptr) *Queue {
	dev.Pull(base+magicOff, 8)
	q := &Queue{base: base}
	if got := dev.Read64(base + magicOff); got != Magic {
		q.markCorrupt(fmt.Sprintf("magic=%#x want=%#x", got, uint64(Magic)))
	}
	return q
}

func (q *Queue) markCorrupt(why string) {
	if !q.corrupt {
		q.corrupt = true
		q.why = why
	}
}

func readIndex(base uintptr, off uintptr) uint64 {
	dev.Pull(base+off, 8)
	return dev.Read64(base + off)
}

func writeIndex(base uintptr, off uintptr, value uint64) {
	dev.Write64(base+off, value)
	dev.Push(base+off, 8)
}

func (q *Queue) Submit(offset uint64, length, token uint32) (ok, notify bool) {
	if q.corrupt {
		return false, false
	}
	head := readIndex(q.base, submitHeadOff)
	tail := readIndex(q.base, submitTailOff)
	if head-tail > Entries {
		q.markCorrupt(fmt.Sprintf("submit head-tail=%d > %d", head-tail, Entries))
		return false, false
	}
	if head-tail == Entries {
		return false, false
	}
	addr := q.base + submitOff + uintptr(head%Entries)*submitSize
	dev.Write64(addr, offset)
	dev.Write64(addr+8, uint64(length)|uint64(token)<<32)
	dev.Push(addr, submitSize)
	dev.MB()
	writeIndex(q.base, submitHeadOff, head+1)
	return true, head == tail
}

// Take consumes one app submission, but only when a completion can be
// published for it. This keeps descriptor ownership exact even when the app
// stops reaping completions.
func (q *Queue) Take() (Desc, bool) {
	if q.corrupt {
		return Desc{}, false
	}
	doneHead := readIndex(q.base, doneHeadOff)
	doneTail := readIndex(q.base, doneTailOff)
	if doneHead-doneTail > Entries {
		q.markCorrupt(fmt.Sprintf("done head-tail=%d > %d", doneHead-doneTail, Entries))
		return Desc{}, false
	}
	if doneHead-doneTail == Entries {
		return Desc{}, false
	}
	head := readIndex(q.base, submitHeadOff)
	tail := readIndex(q.base, submitTailOff)
	if head-tail > Entries {
		q.markCorrupt(fmt.Sprintf("submit head-tail=%d > %d", head-tail, Entries))
		return Desc{}, false
	}
	if head == tail {
		return Desc{}, false
	}
	addr := q.base + submitOff + uintptr(tail%Entries)*submitSize
	dev.Pull(addr, submitSize)
	d := Desc{Offset: dev.Read64(addr)}
	word := dev.Read64(addr + 8)
	d.Length, d.Token = uint32(word), uint32(word>>32)
	dev.MB()
	writeIndex(q.base, submitTailOff, tail+1)
	return d, true
}

func (q *Queue) Complete(token, length, status uint32) (ok, notify bool) {
	if q.corrupt {
		return false, false
	}
	head := readIndex(q.base, doneHeadOff)
	tail := readIndex(q.base, doneTailOff)
	if head-tail > Entries {
		q.markCorrupt(fmt.Sprintf("done head-tail=%d > %d", head-tail, Entries))
		return false, false
	}
	if head-tail == Entries {
		return false, false
	}
	addr := q.base + doneOff + uintptr(head%Entries)*doneSize
	dev.Write64(addr, uint64(token)|uint64(length)<<32)
	dev.Write64(addr+8, uint64(status))
	dev.Push(addr, doneSize)
	dev.MB()
	writeIndex(q.base, doneHeadOff, head+1)
	return true, head == tail
}

func (q *Queue) Reap() (Done, bool) {
	if q.corrupt {
		return Done{}, false
	}
	head := readIndex(q.base, doneHeadOff)
	tail := readIndex(q.base, doneTailOff)
	if head-tail > Entries {
		q.markCorrupt(fmt.Sprintf("done head-tail=%d > %d", head-tail, Entries))
		return Done{}, false
	}
	if head == tail {
		return Done{}, false
	}
	addr := q.base + doneOff + uintptr(tail%Entries)*doneSize
	dev.Pull(addr, doneSize)
	word := dev.Read64(addr)
	d := Done{Token: uint32(word), Length: uint32(word >> 32), Status: uint32(dev.Read64(addr + 8))}
	dev.MB()
	writeIndex(q.base, doneTailOff, tail+1)
	return d, true
}

func (q *Queue) SubmitPending() bool {
	return readIndex(q.base, submitHeadOff) != readIndex(q.base, submitTailOff)
}

func (q *Queue) CompletionPending() (head uint64, pending bool) {
	head = readIndex(q.base, doneHeadOff)
	return head, head != readIndex(q.base, doneTailOff)
}

func (q *Queue) CorruptWhy() string { return q.why }
