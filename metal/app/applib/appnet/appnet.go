// Package appnet gives a caged app its own Ethernet device over two shared
// descriptor queues. Frame payload stays in the app's ordinary heap; HOP sees
// only validated offsets into that partition. After Up, net.Listen and
// net.Dial work normally over the internal network.
//
// The descriptor pages are fixed metadata; actual buffering is demand-driven
// by leannet and HOP's one global frame pool.
// Na Up werken net.Listen en net.Dial gewoon. Op het interne net
// (10.100.0.0/24) praat een
// app rechtstreeks met andere apps en met HOP, zonder dat er ooit een
// TCP-stack op core 0 tussen zit.
//
// Bewust een apart pakket naast applib: alleen apps die netwerk willen linken
// de netstack mee; wie het niet importeert houdt een kleine image.
//
// De stack is leannet (xinix00/lean), sinds 12-08; de opbouw staat in up.go
// en de afwegingen in lean/leannet/DESIGN.md. Geen build-tags, geen backends.
//
// De geschiedenis in het kort, want die verklaart waarom dit pakket zo dun is:
// gVisor was de bewezen maar forse backend (~2,7MB van elk app-image, 340k
// allocaties per 64MiB op het RX-pad); lneto (09-08) haalde dat weg maar bleek
// bugs te dragen die wij niet konden repareren zonder fork-onderhoud (29
// bevindingen, 11-08). Elke wissel raakte alleen up.go, want alles eromheen
// hangt aan het twee-methode-device (metal/net/netdev).
package appnet

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/xinix00/HopOS/metal/v2/abi/frameq"
	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/app/applib"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

const (
	txBackpressure  = 10 * time.Millisecond
	frameBufferSize = 2 << 10
	rxOffers        = 8
)

var errTXQueueFull = errors.New("appnet: TX descriptorqueue bleef vol")

// nic is the app side of the virtio-shaped queue contract. TX buffers are
// allocated lazily and stay pinned until HOP completes their token. RX keeps a
// tiny standing set of offers; bursts wait in HOP's shared pool.
type nic struct {
	mu sync.Mutex
	tx *frameq.Queue
	rx *frameq.Queue

	ramStart uint64
	ramPhys  uint64
	ramSize  uint64

	txBuf  [frameq.Entries][]byte
	txFree [frameq.Entries]bool
	rxBuf  [rxOffers][]byte
}

func newNIC(a *applib.App) (*nic, error) {
	n := &nic{
		tx:       frameq.Open(layout.NetQueueTX(a.Slot)),
		rx:       frameq.Open(layout.NetQueueRX(a.Slot)),
		ramStart: a.RAMStart,
		ramPhys:  a.RAMPhys,
		ramSize:  a.RAMSize,
	}
	if why := n.tx.CorruptWhy(); why != "" {
		return nil, fmt.Errorf("appnet: TX queue: %s", why)
	}
	if why := n.rx.CorruptWhy(); why != "" {
		return nil, fmt.Errorf("appnet: RX queue: %s", why)
	}
	for i := range n.txFree {
		n.txFree[i] = true
	}
	for i := range n.rxBuf {
		n.rxBuf[i] = make([]byte, frameBufferSize)
		if err := n.offerRX(uint32(i)); err != nil {
			return nil, err
		}
	}
	return n, nil
}

func (n *nic) bufferRef(buf []byte) (offset uint64, virtual, physical uintptr, ok bool) {
	if len(buf) == 0 {
		return 0, 0, 0, false
	}
	virtual = uintptr(unsafe.Pointer(&buf[0]))
	// Host tests have no caged MemRegion. Raw pointers suffice there and cache
	// maintenance is a no-op.
	if n.ramSize == 0 {
		return uint64(virtual), virtual, virtual, true
	}
	v := uint64(virtual)
	if v < n.ramStart || uint64(len(buf)) > n.ramSize || v-n.ramStart > n.ramSize-uint64(len(buf)) {
		return 0, 0, 0, false
	}
	offset = v - n.ramStart
	return offset, virtual, uintptr(n.ramPhys + offset), true
}

func (n *nic) offerRX(token uint32) error {
	buf := n.rxBuf[token]
	off, virt, phys, ok := n.bufferRef(buf)
	if !ok {
		return fmt.Errorf("appnet: RX buffer %d ligt buiten de eigen partitie", token)
	}
	frameq.PublishPayload(virt, phys, uintptr(len(buf)))
	ok, notify := n.rx.Submit(off, uint32(len(buf)), token)
	if !ok {
		return fmt.Errorf("appnet: RX descriptorqueue vol of corrupt: %s", n.rx.CorruptWhy())
	}
	if notify {
		dev.Notify()
	}
	return nil
}

// Receive reaps one filled offer and immediately returns its buffer to HOP.
func (n *nic) Receive(dst []byte) (int, error) {
	done, ok := n.rx.Reap()
	if !ok {
		if why := n.rx.CorruptWhy(); why != "" {
			return 0, fmt.Errorf("appnet: RX queue corrupt: %s", why)
		}
		return 0, nil
	}
	if done.Token >= rxOffers {
		return 0, fmt.Errorf("appnet: RX completion token %d buiten bereik", done.Token)
	}
	buf := n.rxBuf[done.Token]
	if done.Status != frameq.StatusOK || int(done.Length) > len(buf) || int(done.Length) > len(dst) {
		_ = n.offerRX(done.Token)
		return 0, nil
	}
	_, virt, phys, _ := n.bufferRef(buf)
	frameq.AcquirePayload(virt, phys, uintptr(done.Length))
	copy(dst, buf[:done.Length])
	if err := n.offerRX(done.Token); err != nil {
		return 0, err
	}
	return int(done.Length), nil
}

func (n *nic) reapTX() {
	for {
		done, ok := n.tx.Reap()
		if !ok {
			return
		}
		if done.Token < frameq.Entries {
			n.txFree[done.Token] = true
		}
	}
}

func (n *nic) takeTXBuffer() (uint32, []byte, bool) {
	for i := range n.txFree {
		if !n.txFree[i] {
			continue
		}
		n.txFree[i] = false
		if n.txBuf[i] == nil {
			n.txBuf[i] = make([]byte, frameBufferSize)
		}
		return uint32(i), n.txBuf[i], true
	}
	return 0, nil, false
}

// Transmit moves payload into a lazily allocated app-owned buffer and returns
// as soon as ownership has moved to HOP.
func (n *nic) Transmit(src []byte) error {
	if len(src) > frameBufferSize {
		return fmt.Errorf("appnet: frame van %d bytes is te groot", len(src))
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	deadline := time.Now().Add(txBackpressure)
	for {
		n.reapTX()
		token, buf, ok := n.takeTXBuffer()
		if ok {
			copy(buf, src)
			off, virt, phys, valid := n.bufferRef(buf[:len(src)])
			if !valid {
				n.txFree[token] = true
				return fmt.Errorf("appnet: TX buffer ligt buiten de eigen partitie")
			}
			frameq.PublishPayload(virt, phys, uintptr(len(src)))
			if accepted, notify := n.tx.Submit(off, uint32(len(src)), token); accepted {
				if notify {
					dev.Notify()
				}
				runtime.KeepAlive(buf)
				return nil
			}
			n.txFree[token] = true
		}
		dev.Notify()
		if time.Now().After(deadline) {
			return errTXQueueFull
		}
		runtime.Gosched()
	}
}
