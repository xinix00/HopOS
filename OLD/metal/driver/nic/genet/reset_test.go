//go:build !tamago

package genet

import (
	"runtime"
	"testing"
	"unsafe"
)

func TestResetRequiresBothDMAEnginesStopped(t *testing.T) {
	for _, stalled := range []uintptr{0, tdmaStatus, rdmaStatus} {
		regs := make([]uint64, 4096)
		n := &Net{Base: uintptr(unsafe.Pointer(&regs[0]))}
		n.wr(tdmaCtrl, dmaEnableMask)
		n.wr(rdmaCtrl, dmaEnableMask)
		n.wr(umacCmd, 3)
		n.wr(sysRBufFlushCtrl, 0x40)
		n.wr(tdmaStatus, dmaDisabled)
		n.wr(rdmaStatus, dmaDisabled)
		if stalled != 0 {
			n.wr(stalled, 0)
		}
		err := n.Reset()
		if (err != nil) != (stalled != 0) {
			t.Fatalf("stalled=%#x: %v", stalled, err)
		}
		if n.rd(tdmaCtrl)&1 != 0 || n.rd(umacCmd)&2 != 0 {
			t.Fatal("TX DMA or MAC RX left enabled")
		}
		if stalled == tdmaStatus {
			if n.rd(rdmaCtrl) != dmaEnableMask {
				t.Fatal("RX disabled before TX completed draining")
			}
		} else if n.rd(rdmaCtrl)&1 != 0 {
			t.Fatal("RX DMA left enabled after TX stopped")
		}
		if stalled != 0 {
			if n.rd(tdmaCtrl)&(dmaEnableMask&^1) != dmaEnableMask&^1 || n.rd(rdmaCtrl)&(dmaEnableMask&^1) != dmaEnableMask&^1 {
				t.Fatal("queue enables cleared before both engines stopped")
			}
		} else if n.rd(tdmaCtrl)&dmaEnableMask != 0 || n.rd(rdmaCtrl)&dmaEnableMask != 0 {
			t.Fatal("stale queue enables remained after completed shutdown")
		}
		if stalled != 0 && n.rd(umacCmd)&1 == 0 {
			t.Fatal("MAC TX disabled before DMA finished draining")
		}
		if stalled != 0 && n.rd(sysRBufFlushCtrl) != 0x40 {
			t.Fatal("MAC reset began before DMA stopped")
		}
		if stalled == 0 && n.rd(sysPortCtrl) != 3 {
			t.Fatal("successful stop did not complete reset")
		}
		runtime.KeepAlive(regs)
	}
}

func TestInitAlignsRetainedHardwareCounters(t *testing.T) {
	for _, counter := range []uint32{0, 193, 0xFFFF} {
		regs := make([]uint64, 4096)
		n := &Net{Base: uintptr(unsafe.Pointer(&regs[0]))}
		n.wr(tdmaRing16+8, counter)
		n.wr(rdmaRing16+8, 0xABCD0000|counter)
		dma := make([]byte, 2*nBD*bufSize)
		dmaBase := uintptr(unsafe.Pointer(&dma[0]))
		if err := n.Init(dmaBase, uintptr(len(dma)), 1000, true); err != nil {
			t.Fatal(err)
		}
		if n.txProd != counter || n.rxCons != counter || n.rd(tdmaRing16+12) != counter || n.rd(rdmaRing16+12) != counter {
			t.Fatal("software queue did not start empty at retained counter")
		}
		if n.rd(tdmaRing16+8) != counter || n.rd(rdmaRing16+8) != 0xABCD0000|counter {
			t.Fatal("wrote hardware-owned counter")
		}
		if n.rd(tdmaCtrl)&(1<<17|1) != 1<<17|1 || n.rd(rdmaCtrl)&(1<<17|1) != 1<<17|1 {
			t.Fatal("rings not enabled")
		}
		ptr := (counter % nBD) * 3
		for _, off := range []uintptr{tdmaRing16, tdmaRing16 + 0x2c, rdmaRing16, rdmaRing16 + 0x2c} {
			if got := n.rd(off); got != ptr {
				t.Fatalf("counter %#x pointer %#x = %#x, want %#x words", counter, off, got, ptr)
			}
		}
		// Hardware's seeded cursor and the software's first TX/RX access
		// must select the same descriptor, including counter wrap.
		packet := []byte("first frame")
		index := counter % nBD
		if err := n.Transmit(packet); err != nil {
			t.Fatal(err)
		}
		txOff := nBD*bufSize + int(index)*bufSize
		if string(dma[txOff:txOff+len(packet)]) != string(packet) || n.rd(txBD+uintptr(index)*12)>>16 != uint32(len(packet)) {
			t.Fatal("first TX did not use the hardware's seeded descriptor")
		}
		next := (counter + 1) & 0xffff
		if n.txProd != next || n.rd(tdmaRing16+12) != next {
			t.Fatal("TX producer did not advance with 16-bit wrap")
		}
		rxOff := int(index)*bufSize + 2
		copy(dma[rxOff:], packet)
		n.wr(rxBD+uintptr(index)*12, uint32(len(packet)+2)<<16|dmaSOP|dmaEOP)
		n.wr(rdmaRing16+8, next)
		buf := make([]byte, len(packet))
		if got, err := n.Receive(buf); err != nil || got != len(packet) || string(buf) != string(packet) {
			t.Fatalf("first RX did not use the hardware's seeded descriptor: %d %v %q", got, err, buf)
		}
		if n.rxCons != next || n.rd(rdmaRing16+12) != next {
			t.Fatal("RX consumer did not advance with 16-bit wrap")
		}
		runtime.KeepAlive(dma)
		runtime.KeepAlive(regs)
	}
}
