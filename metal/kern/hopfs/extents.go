package hopfs

import (
	"fmt"
	"sort"
)

type extent struct{ logical, physical, count uint32 }
type diskRange struct{ start, count uint32 }

// compact prevents a shortened file/free list retaining its largest index.
func compact[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	if cap(s) > 2*len(s) {
		return append([]T(nil), s...)
	}
	return s
}

func (n *node) position(block uint32) int {
	return sort.Search(len(n.extents), func(i int) bool { return uint64(n.extents[i].logical)+uint64(n.extents[i].count) > uint64(block) })
}
func (n *node) lookup(block uint32) (physical, count uint32, mapped bool) {
	i := n.position(block)
	if i < len(n.extents) {
		e := n.extents[i]
		if block >= e.logical {
			d := block - e.logical
			return e.physical + d, e.count - d, true
		}
		return 0, e.logical - block, false
	}
	return 0, ^uint32(0) - block, false
}
func adjacent(a, b extent) bool {
	return uint64(a.logical)+uint64(a.count) == uint64(b.logical) && uint64(a.physical)+uint64(a.count) == uint64(b.physical)
}
func (n *node) joins(logical, physical, count uint32) bool {
	i := n.position(logical)
	e := extent{logical, physical, count}
	return (i > 0 && adjacent(n.extents[i-1], e)) || (i < len(n.extents) && adjacent(e, n.extents[i]))
}

// mapRun inserts a previously unmapped logical run, merging both neighbors.
func (f *FS) mapRun(n *node, e extent) {
	i := n.position(e.logical)
	if i > 0 && adjacent(n.extents[i-1], e) {
		n.extents[i-1].count += e.count
		i--
	} else {
		n.extents = append(n.extents, extent{})
		copy(n.extents[i+1:], n.extents[i:])
		n.extents[i] = e
		f.index++
	}
	if i+1 < len(n.extents) && adjacent(n.extents[i], n.extents[i+1]) {
		n.extents[i].count += n.extents[i+1].count
		copy(n.extents[i+1:], n.extents[i+2:])
		n.extents = compact(n.extents[:len(n.extents)-1])
		f.index--
	}
}

// Keep the existing bump-first allocation policy and transfer-sized runs.
func (f *FS) allocRun(want uint32) (uint32, uint32, error) {
	if f.next < f.max {
		count := min(want, f.max-f.next)
		start := f.next
		f.next += count
		return start, count, nil
	}
	if len(f.free) > 0 {
		r := &f.free[0]
		start := r.start
		count := min(want, r.count)
		r.start += count
		r.count -= count
		if r.count == 0 {
			copy(f.free, f.free[1:])
			f.free = compact(f.free[:len(f.free)-1])
		}
		return start, count, nil
	}
	return 0, 0, fmt.Errorf("hopfs: disk full (%d blocks)", f.max)
}

// Free ranges are sorted and coalesced. Their number is bounded by the live
// physical runs plus one; deleting a large contiguous file adds just one run.
func (f *FS) freeRun(start, count uint32) {
	if count == 0 {
		return
	}
	i := sort.Search(len(f.free), func(i int) bool { return f.free[i].start >= start })
	if i > 0 && uint64(f.free[i-1].start)+uint64(f.free[i-1].count) == uint64(start) {
		i--
		f.free[i].count += count
	} else {
		f.free = append(f.free, diskRange{})
		copy(f.free[i+1:], f.free[i:])
		f.free[i] = diskRange{start, count}
	}
	if i+1 < len(f.free) && uint64(f.free[i].start)+uint64(f.free[i].count) == uint64(f.free[i+1].start) {
		f.free[i].count += f.free[i+1].count
		copy(f.free[i+1:], f.free[i+2:])
		f.free = f.free[:len(f.free)-1]
	}
	// Return the unallocated tail to the bump pointer (also exact rollback).
	last := len(f.free) - 1
	if last >= 0 && uint64(f.free[last].start)+uint64(f.free[last].count) == uint64(f.next) {
		f.next = f.free[last].start
		f.free = f.free[:last]
	}
	f.free = compact(f.free)
}
