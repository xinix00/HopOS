// Package virtionet is HopOS' eigen virtio-net-driver (virtio-mmio, modern /
// VERSION_1), bare-metal Go — geen tamago kvm/virtio (dat pakket bouwt niet op
// arm64), geen C. Het implementeert go-net's NetworkDevice (Receive/Transmit),
// dus de gVisor-netstack draait er direct op.
//
// Dezelfde vorm die de RTL8126 straks krijgt: MMIO-registers, split
// virtqueues (descriptortabel + avail/used-ring) en DMA-buffers. Alle DMA en
// registers via metal/dev (gealigneerde, device-veilige toegang); de
// DMA-regio is niet-gecached en op QEMU volledig coherent.
package virtionet

import (
	"errors"

	"hop-os/metal/dev"
)

// MMIO-registeroffsets (virtio-mmio, versie 2).
const (
	regMagic          = 0x000
	regVersion        = 0x004
	regDeviceID       = 0x008
	regDeviceFeatures = 0x010
	regDevFeaturesSel = 0x014
	regDrvFeatures    = 0x020
	regDrvFeaturesSel = 0x024
	regQueueSel       = 0x030
	regQueueNumMax    = 0x034
	regQueueNum       = 0x038
	regQueueReady     = 0x044
	regQueueNotify    = 0x050
	regInterruptACK   = 0x064
	regStatus         = 0x070
	regQueueDescLo    = 0x080
	regQueueDescHi    = 0x084
	regQueueDrvLo     = 0x090
	regQueueDrvHi     = 0x094
	regQueueDevLo     = 0x0a0
	regQueueDevHi     = 0x0a4
	regConfig         = 0x100

	magicValue = 0x74726976 // "virt"
	version2   = 2
	netDevice  = 1
)

// Device-status bits.
const (
	statusAck        = 1 << 0
	statusDriver     = 1 << 1
	statusDriverOK   = 1 << 2
	statusFeaturesOK = 1 << 3
)

// Feature bits (bit 32 = VIRTIO_F_VERSION_1, verplicht voor modern).
const featVersion1Hi = 1 << 0 // bit 32 in het hoge 32-bit venster

// Descriptor-flags.
const (
	descNext  = 1
	descWrite = 2
)

const (
	rxQueue   = 0
	txQueue   = 1
	hdrLen    = 12   // virtio_net_hdr_mrg_rxbuf (VERSION_1)
	bufSize   = 2048 // per RX/TX-buffer
	maxQueue  = 256
	descBytes = 16
)

// Net is een virtio-net-instantie op MMIO-basis Base, met een bump-allocator
// over de DMA-regio [dmaBase, dmaBase+dmaSize).
type Net struct {
	Base uintptr
	MAC  [6]byte

	dmaNext uintptr
	dmaEnd  uintptr

	qsize int
	rx    vq
	tx    vq

	RxCount uint64
	RxCalls uint64
	TxCount uint64
	TxErr   uint64
}

// vq is één split-virtqueue.
type vq struct {
	desc  uintptr // descriptortabel
	avail uintptr // driver-ring
	used  uintptr // device-ring
	bufs  uintptr // qsize * bufSize aaneengesloten databuffers

	lastUsed uint16 // laatst geconsumeerde used.idx
	availIdx uint16 // onze avail.idx
}

func align(x uintptr, a uintptr) uintptr { return (x + a - 1) &^ (a - 1) }

func (n *Net) alloc(size, a uintptr) uintptr {
	p := align(n.dmaNext, a)
	n.dmaNext = p + size
	if n.dmaNext > n.dmaEnd {
		panic("virtionet: DMA-regio te klein")
	}
	dev.Clear(p, uint64(size))
	return p
}

func (n *Net) rd8(off uintptr) uint32    { return dev.Read32(n.Base + off) }
func (n *Net) wr8(off uintptr, v uint32) { dev.Write32(n.Base+off, v) }

// Init reset het device, onderhandelt VERSION_1, zet RX+TX-queues op en maakt
// het device operationeel. dmaBase/dmaSize is de (device-gemapte) DMA-regio.
func (n *Net) Init(dmaBase, dmaSize uintptr) error {
	if n.rd8(regMagic) != magicValue {
		return errors.New("virtionet: geen virtio-mmio")
	}
	if n.rd8(regVersion) != version2 {
		return errors.New("virtionet: geen versie-2 (modern) transport")
	}
	if n.rd8(regDeviceID) != netDevice {
		return errors.New("virtionet: geen netwerk-device")
	}
	n.dmaNext, n.dmaEnd = dmaBase, dmaBase+dmaSize

	// Status-handshake: reset → ACK → DRIVER.
	n.wr8(regStatus, 0)
	n.wr8(regStatus, statusAck)
	n.wr8(regStatus, statusAck|statusDriver)

	// Feature-onderhandeling: alleen VERSION_1 (bit 32).
	n.wr8(regDrvFeaturesSel, 0)
	n.wr8(regDrvFeatures, 0)
	n.wr8(regDrvFeaturesSel, 1)
	n.wr8(regDrvFeatures, featVersion1Hi)

	n.wr8(regStatus, statusAck|statusDriver|statusFeaturesOK)
	if n.rd8(regStatus)&statusFeaturesOK == 0 {
		return errors.New("virtionet: device weigerde VERSION_1")
	}

	// MAC uit de config (eerste 6 config-bytes). Byte-reads: 32-bit reads op
	// oneven offsets zijn ongealigneerd en aborten op device-geheugen.
	for i := range n.MAC {
		n.MAC[i] = dev.Read8(n.Base + regConfig + uintptr(i))
	}

	if err := n.setupQueue(rxQueue, &n.rx); err != nil {
		return err
	}
	if err := n.setupQueue(txQueue, &n.tx); err != nil {
		return err
	}

	// RX-buffers publiceren zodat het device er in kan schrijven.
	for i := 0; i < n.qsize; i++ {
		n.setDesc(&n.rx, i, n.rx.bufs+uintptr(i*bufSize), bufSize, descWrite)
		n.setAvail(&n.rx, i, uint16(i))
	}
	n.rx.availIdx = uint16(n.qsize)
	dev.MB()
	dev.Write16(n.rx.avail+2, n.rx.availIdx) // avail.idx
	dev.MB()

	n.wr8(regStatus, statusAck|statusDriver|statusFeaturesOK|statusDriverOK)
	n.notify(rxQueue)
	return nil
}

func (n *Net) setupQueue(idx int, q *vq) error {
	n.wr8(regQueueSel, uint32(idx))
	max := int(n.rd8(regQueueNumMax))
	if max == 0 {
		return errors.New("virtionet: queue niet beschikbaar")
	}
	if n.qsize == 0 {
		n.qsize = max
		if n.qsize > maxQueue {
			n.qsize = maxQueue
		}
	}
	n.wr8(regQueueNum, uint32(n.qsize))

	q.desc = n.alloc(uintptr(n.qsize*descBytes), 16)
	q.avail = n.alloc(uintptr(6+2*n.qsize), 16)
	q.used = n.alloc(uintptr(6+8*n.qsize), 16)
	q.bufs = n.alloc(uintptr(n.qsize*bufSize), 16)

	n.wr8(regQueueDescLo, uint32(q.desc))
	n.wr8(regQueueDescHi, 0)
	n.wr8(regQueueDrvLo, uint32(q.avail))
	n.wr8(regQueueDrvHi, 0)
	n.wr8(regQueueDevLo, uint32(q.used))
	n.wr8(regQueueDevHi, 0)
	n.wr8(regQueueReady, 1)
	return nil
}

func (n *Net) setDesc(q *vq, i int, addr uintptr, length uint32, flags uint16) {
	d := q.desc + uintptr(i*descBytes)
	dev.Write64(d, uint64(addr))
	dev.Write32(d+8, length)
	dev.Write16(d+12, flags)
	dev.Write16(d+14, 0)
}

func (n *Net) setAvail(q *vq, slot int, descIdx uint16) {
	dev.Write16(q.avail+4+uintptr((slot%n.qsize)*2), descIdx)
}

func (n *Net) notify(queue uint32) { n.wr8(regQueueNotify, queue) }

// Receive haalt één ethernet-frame op (non-blocking: n=0 als er niets is).
// Voldoet aan go-net's NetworkDevice.
func (n *Net) Receive(buf []byte) (int, error) {
	n.RxCalls++
	usedIdx := dev.Read16(n.rx.used + 2)
	if usedIdx == n.rx.lastUsed {
		return 0, nil
	}
	dev.MB()

	slot := uintptr(n.rx.lastUsed % uint16(n.qsize))
	elem := n.rx.used + 4 + slot*8
	descIdx := uint16(dev.Read32(elem)) // used_elem.id
	length := int(dev.Read32(elem + 4)) // used_elem.len (incl. virtio-hdr)

	if length > hdrLen {
		frame := length - hdrLen
		if frame > len(buf) {
			frame = len(buf)
		}
		dev.CopyOut(buf[:frame], n.rx.bufs+uintptr(int(descIdx)*bufSize)+hdrLen)
		n.recycleRx(descIdx)
		n.rx.lastUsed++
		n.RxCount++
		return frame, nil
	}

	n.recycleRx(descIdx)
	n.rx.lastUsed++
	return 0, nil
}

// recycleRx geeft een RX-buffer terug aan het device.
func (n *Net) recycleRx(descIdx uint16) {
	n.setDesc(&n.rx, int(descIdx), n.rx.bufs+uintptr(int(descIdx)*bufSize), bufSize, descWrite)
	n.setAvail(&n.rx, int(n.rx.availIdx), descIdx)
	n.rx.availIdx++
	dev.MB()
	dev.Write16(n.rx.avail+2, n.rx.availIdx)
	dev.MB()
	n.notify(rxQueue)
}

// Transmit verstuurt één ethernet-frame (synchroon: wacht op voltooiing).
func (n *Net) Transmit(buf []byte) error {
	slot := int(n.tx.availIdx) % n.qsize
	bufAddr := n.tx.bufs + uintptr(slot*bufSize)

	// 12-byte virtio-header (nul) + frame.
	dev.Clear(bufAddr, hdrLen)
	frame := buf
	if len(frame) > bufSize-hdrLen {
		frame = frame[:bufSize-hdrLen]
	}
	dev.Copy(bufAddr+hdrLen, frame)

	n.setDesc(&n.tx, slot, bufAddr, uint32(hdrLen+len(frame)), 0)
	n.setAvail(&n.tx, int(n.tx.availIdx), uint16(slot))
	n.tx.availIdx++
	dev.MB()
	dev.Write16(n.tx.avail+2, n.tx.availIdx)
	dev.MB()
	n.notify(txQueue)

	// Wachten tot het device de descriptor heeft verwerkt.
	for i := 0; i < 1_000_000; i++ {
		if dev.Read16(n.tx.used+2) == n.tx.availIdx {
			n.tx.lastUsed = n.tx.availIdx
			n.TxCount++
			return nil
		}
	}
	n.TxErr++
	return errors.New("virtionet: TX timeout")
}
