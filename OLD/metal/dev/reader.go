package dev

import "io"

// ReaderAt reads a bounded device-memory region through alignment-safe loads.
// Both app placement and kernel flip parse their staged ELF through this reader.
type ReaderAt struct {
	Base uintptr
	Size int64
}

func (r ReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 || off >= r.Size {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.Size-off {
		n = int(r.Size - off)
	}
	CopyOut(p[:n], r.Base+uintptr(off))
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
