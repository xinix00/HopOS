package hopswitch

import (
	"fmt"
	"unsafe"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// Frames are Ethernet-sized; 2 KiB keeps every chunk page-friendly while
// leaving room for VLAN headers without teaching the allocator protocols.
const frameChunkSize = 2 << 10

type poolChunk struct {
	next   uint32
	length uint16
}

type framePool struct {
	base      uintptr
	chunks    []poolChunk // index 0 is the nil/sentinel value
	free      uint32
	freeCount uint32
	keep      []uint64 // host-test backing; nil on metal
}

func (p *framePool) configure(base uintptr, size uint64) error {
	count := size / frameChunkSize
	if base == 0 || count == 0 || count > uint64(^uint32(0)-1) {
		return fmt.Errorf("hopswitch: frame pool %#x+%d is invalid", base, size)
	}
	p.base = base
	p.chunks = make([]poolChunk, count+1)
	for i := uint32(1); i <= uint32(count); i++ {
		if i < uint32(count) {
			p.chunks[i].next = i + 1
		}
	}
	p.free = 1
	p.freeCount = uint32(count)
	return nil
}

func (p *framePool) configureLocal(size uint64) error {
	words := make([]uint64, (size+7)/8)
	p.keep = words
	return p.configure(uintptr(unsafe.Pointer(&words[0])), uint64(len(words))*8)
}

func (p *framePool) alloc(frame []byte) uint32 {
	if len(frame) > frameChunkSize || p.free == 0 {
		return 0
	}
	id := p.free
	p.free = p.chunks[id].next
	p.freeCount--
	p.chunks[id] = poolChunk{length: uint16(len(frame))}
	dev.Copy(p.addr(id), frame)
	return id
}

func (p *framePool) copyOut(id uint32, dst []byte) (int, bool) {
	if id == 0 || int(id) >= len(p.chunks) {
		return 0, false
	}
	n := int(p.chunks[id].length)
	if n > len(dst) || n > frameChunkSize {
		return 0, false
	}
	dev.CopyOut(dst[:n], p.addr(id))
	return n, true
}

func (p *framePool) copyTo(id uint32, dst uintptr) (int, bool) {
	if id == 0 || int(id) >= len(p.chunks) {
		return 0, false
	}
	n := int(p.chunks[id].length)
	if n > frameChunkSize {
		return 0, false
	}
	dev.Move(dst, p.addr(id), uint64(n))
	return n, true
}

func (p *framePool) release(id uint32) {
	if id == 0 || int(id) >= len(p.chunks) {
		return
	}
	p.chunks[id] = poolChunk{next: p.free}
	p.free = id
	p.freeCount++
}

func (p *framePool) addr(id uint32) uintptr {
	return p.base + uintptr(id-1)*frameChunkSize
}

// ConfigureFramePool hands the switch its one HOP-wide payload pool. The
// fixed per-slot pages contain descriptors only; every queued frame borrows a
// chunk here and returns it immediately after delivery or detach.
func ConfigureFramePool(base uintptr, size uint64) error {
	mu.Lock()
	defer mu.Unlock()
	for i, pt := range ports {
		if i > 0 && pt != nil {
			return fmt.Errorf("hopswitch: cannot replace frame pool while slot %d is attached", i)
		}
	}
	var next framePool
	if err := next.configure(base, size); err != nil {
		return err
	}
	pool = next
	return nil
}
