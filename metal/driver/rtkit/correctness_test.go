package rtkit

import (
	"github.com/xinix00/HopOS/metal/v2/dev"
	"runtime"
	"testing"
	"unsafe"
)

func TestPollDeliversApplicationEndpoint(t *testing.T) {
	regs := make([]uint64, 0x9000/8)
	d := &Dev{Base: uintptr(unsafe.Pointer(&regs[0]))}
	dev.Write64(d.mb()+i2aRecv0, 0x1234)
	dev.Write64(d.mb()+i2aRecv1, epSystem)
	count := 0
	d.App = func(msg uint64, ep uint32) {
		if msg != 0x1234 || ep != epSystem {
			t.Fatal("wrong application message")
		}
		count++
		dev.Write32(d.mb()+i2aControl, mboxEmpty)
	}
	if err := d.Poll(); err != nil {
		t.Fatal(err)
	}
	if count != 1 || !d.appEP[epSystem] {
		t.Fatal("application endpoint discarded")
	}
	runtime.KeepAlive(regs)
}

func TestCrashlogBounds(t *testing.T) {
	buf := make([]uint64, 4)
	buf[0] = 0x434c4845
	d := &Dev{}
	d.bufs[epCrashlog] = uintptrToUint64(&buf[0])
	d.bufSizes[epCrashlog] = 32
	if got := d.Crashlog(); got != "crashlog without a text entry" {
		t.Fatal(got)
	}
	// Firmware-provided addresses have no host-owned bound and are not dereferenced.
	d.bufs[epCrashlog] = 1
	d.bufSizes[epCrashlog] = 0
	if got := d.Crashlog(); got != "no crashlog buffer" {
		t.Fatal(got)
	}
	runtime.KeepAlive(buf)
}
func uintptrToUint64(p *uint64) uint64 { return uint64(uintptr(unsafe.Pointer(p))) }
