//go:build tamago

package kernflip

import (
	"github.com/xinix00/HopOS/metal/v2/dev"
	"io"
	"runtime"
)

// The bundle lives in its reserved destination window, never in kernel heap.
// Device memory must only be accessed through aligned dev operations.
type windowWriter struct {
	base uintptr
	left int64
}

func (w *windowWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.left {
		return 0, io.ErrShortWrite
	}
	dev.Copy(w.base, p)
	w.base += uintptr(len(p))
	w.left -= int64(len(p))
	runtime.Gosched()
	return len(p), nil
}
