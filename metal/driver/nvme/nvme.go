// Package nvme is HopOS' eigen NVMe-driver in bare-metal Go: admin-queue +
// één I/O-queue-paar, polled (geen interrupts — HOP heeft één taak tegelijk
// voor zijn scratch-verkeer en niets anders te doen tijdens het wachten).
// Zelfde vorm als virtionet: MMIO-registers en DMA-ringen via metal/dev.
//
// NVMe is in HopOS uitsluitend scratch/RAM-overloop (plan §3): bij boot leeg
// verondersteld, nooit persistent. Vandaar ook: één namespace, geen
// partities, geen filesystem — raw blocks door HOP beheerd.
package nvme

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"hop-os/metal/dev"
	"hop-os/metal/driver/pcie"
)

// Controller-registers (NVMe 1.4, MMIO op BAR0).
const (
	regCAP  = 0x00 // 64-bit capabilities
	regCC   = 0x14 // controller configuration
	regCSTS = 0x1c // controller status
	regAQA  = 0x24 // admin queue attributes
	regASQ  = 0x28 // 64-bit admin submission queue base
	regACQ  = 0x30 // 64-bit admin completion queue base
	regDB   = 0x1000

	ccEnable = 1 << 0
	ccIOSQES = 6 << 16 // 64B submission entries (2^6)
	ccIOCQES = 4 << 20 // 16B completion entries (2^4)

	cstsRDY = 1 << 0
)

// Opcodes.
const (
	admCreateSQ = 0x01
	admCreateCQ = 0x05
	admIdentify = 0x06
	ioWrite     = 0x01
	ioRead      = 0x02
)

const (
	qEntries = 64 // per queue; ruim voldoende voor één consument
	sqeSize  = 64
	cqeSize  = 16
	nsid     = 1
)

// queue is één SQ/CQ-paar met poll-state.
type queue struct {
	sq, cq uintptr
	tail   uint32 // SQ-tail (producer, wij)
	head   uint32 // CQ-head (consumer, wij)
	phase  uint32 // verwachte phase-bit in de CQ
	id     uint32
}

// Controller is één NVMe-controller met één actieve namespace.
type Controller struct {
	Base uintptr // BAR0

	BlockSize uint64
	Blocks    uint64 // namespace-grootte in blokken
	Model     string

	mu    sync.Mutex // serialiseert I/O (één in-flight command, één DMA-buf)
	dstrd uint64
	admin queue
	io    queue
	buf   uintptr // één pagina DMA voor identify en de blok-API
}

func (c *Controller) doorbell(q *queue, cq bool) uintptr {
	n := uintptr(2 * q.id)
	if cq {
		n++
	}
	return c.Base + regDB + n<<(2+c.dstrd)
}

// waitCSTS polt CSTS.RDY tot de gewenste waarde.
func (c *Controller) waitCSTS(rdy uint32, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if dev.Read32(c.Base+regCSTS)&cstsRDY == rdy {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return fmt.Errorf("nvme: CSTS.RDY never became %d", rdy)
}

// cmd is een 64-byte submission-entry in opbouw.
type cmd struct {
	opc        uint32
	nsid       uint32
	prp1       uint64
	dw10, dw11 uint32
	dw12       uint32
}

// submit schrijft de command in de SQ, belt de doorbell en polt de CQ tot de
// completion binnen is. Geeft de statuscode (0 = succes) terug.
func (c *Controller) submit(q *queue, m cmd) error {
	cid := q.tail // uniek genoeg: één command in flight per queue
	sqe := q.sq + uintptr(q.tail)*sqeSize
	dev.Clear(sqe, sqeSize)
	dev.Write32(sqe+0, m.opc|cid<<16)
	dev.Write32(sqe+4, m.nsid)
	dev.Write64(sqe+24, m.prp1)
	dev.Write32(sqe+40, m.dw10)
	dev.Write32(sqe+44, m.dw11)
	dev.Write32(sqe+48, m.dw12)
	dev.MB()

	q.tail = (q.tail + 1) % qEntries
	dev.Write32(c.doorbell(q, false), q.tail)

	// Poll de completion (phase-bit wisselt per CQ-omloop).
	cqe := q.cq + uintptr(q.head)*cqeSize
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := dev.Read32(cqe + 12)
		if (status>>16)&1 == q.phase {
			dev.MB()
			q.head = (q.head + 1) % qEntries
			if q.head == 0 {
				q.phase ^= 1
			}
			dev.Write32(c.doorbell(q, true), q.head)
			if sc := status >> 17; sc != 0 {
				return fmt.Errorf("nvme: command %#x status %#x", m.opc, sc)
			}
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nvme: timeout on command %#x", m.opc)
		}
	}
}

// Probe zoekt de NVMe-controller op bus 0 in het PCIe-venster van het board,
// zet memory-decode + bus-mastering aan en initialiseert de driver op de
// gegeven DMA-regio. De gedeelde storage-opstart van elke HOP-main (voorheen
// los in nvmeDemo en storageUp — de demo doet daarna nog een scratch-zelftest,
// productie niet).
//
// BAR0: op een kale fabric (win.MMIOBase != 0) wijzen wij hem zelf toe; op
// een firmware-geconfigureerd platform (UEFI/ACPI: win.MMIOBase == 0) wees de
// firmware hem al toe en LEZEN we hem — hem overschrijven met SetBAR64(0, 0)
// zou de controller op PA 0 zetten (op QEMU de flash-alias, op de Altra een
// data-abort). Zelfde read-only conventie als het igb-pad.
func Probe(win pcie.Window, dmaBase uintptr, dmaSize uint64) (*Controller, error) {
	var nd *pcie.Device
	for _, d := range pcie.Scan(win) {
		if d.Class>>8 == 0x0108 { // mass storage / NVM express
			nd = d
		}
	}
	if nd == nil {
		return nil, fmt.Errorf("nvme: no device on bus 0")
	}
	base := win.MMIOBase
	if base == 0 {
		// Firmware-toegewezen BAR0 (de firmware deed de toewijzing); een hoge
		// BAR vergt dat het board hem al bereikbaar maakte (MapHigh), net als
		// bij igb — op de kale-fabric-boards ligt hij laag/vlak.
		base = uintptr(nd.BAR(0))
		if base == 0 {
			return nil, fmt.Errorf("nvme: BAR0 not assigned (firmware MMIOBase=0 and device BAR0=0)")
		}
	} else {
		nd.SetBAR64(0, uint64(base))
	}
	nd.Enable()
	c := &Controller{Base: base}
	if err := c.Init(dmaBase, dmaSize); err != nil {
		return nil, err
	}
	return c, nil
}

// Init reset de controller, zet admin- en I/O-queues op en identificeert de
// namespace. dmaBase/dmaSize is de (device-gemapte, niet-gecachte) DMA-regio.
func (c *Controller) Init(dmaBase uintptr, dmaSize uint64) error {
	if dmaSize < 5*4096 {
		return errors.New("nvme: DMA-regio te klein")
	}
	cap := dev.Read64(c.Base + regCAP)
	c.dstrd = (cap >> 32) & 0xf
	if mqes := cap & 0xffff; mqes+1 < qEntries {
		return fmt.Errorf("nvme: MQES %d < %d", mqes+1, qEntries)
	}

	// DMA-indeling: vier queue-pagina's + één databuffer-pagina.
	c.admin = queue{sq: dmaBase, cq: dmaBase + 4096, phase: 1, id: 0}
	c.io = queue{sq: dmaBase + 2*4096, cq: dmaBase + 3*4096, phase: 1, id: 1}
	c.buf = dmaBase + 4*4096
	for i := uintptr(0); i < 5; i++ {
		dev.Clear(dmaBase+i*4096, 4096)
	}

	// Reset → admin-queues registreren → enable.
	dev.Write32(c.Base+regCC, 0)
	if err := c.waitCSTS(0, 5*time.Second); err != nil {
		return err
	}
	dev.Write32(c.Base+regAQA, (qEntries-1)<<16|(qEntries-1))
	dev.Write64(c.Base+regASQ, uint64(c.admin.sq))
	dev.Write64(c.Base+regACQ, uint64(c.admin.cq))
	dev.MB()
	dev.Write32(c.Base+regCC, ccEnable|ccIOSQES|ccIOCQES)
	if err := c.waitCSTS(1, 5*time.Second); err != nil {
		return err
	}

	// Identify controller (CNS=1): modelnaam voor de log.
	if err := c.submit(&c.admin, cmd{opc: admIdentify, prp1: uint64(c.buf), dw10: 1}); err != nil {
		return err
	}
	model := make([]byte, 40)
	dev.CopyOut(model, c.buf+24)
	c.Model = trim(model)

	// Identify namespace 1 (CNS=0): grootte + blokmaat (LBAF/FLBAS).
	if err := c.submit(&c.admin, cmd{opc: admIdentify, nsid: nsid, prp1: uint64(c.buf), dw10: 0}); err != nil {
		return err
	}
	c.Blocks = dev.Read64(c.buf)
	flbas := uint64(dev.Read8(c.buf+26)) & 0xf
	lbads := (dev.Read32(c.buf+128+uintptr(flbas)*4) >> 16) & 0xff
	c.BlockSize = 1 << lbads
	if c.Blocks == 0 || c.BlockSize == 0 || c.BlockSize > 4096 {
		return fmt.Errorf("nvme: namespace onbruikbaar (blocks=%d bs=%d)", c.Blocks, c.BlockSize)
	}

	// I/O-queue-paar (CQ eerst; PC=1, geen interrupts).
	if err := c.submit(&c.admin, cmd{opc: admCreateCQ, prp1: uint64(c.io.cq),
		dw10: (qEntries-1)<<16 | c.io.id, dw11: 1}); err != nil {
		return err
	}
	if err := c.submit(&c.admin, cmd{opc: admCreateSQ, prp1: uint64(c.io.sq),
		dw10: (qEntries-1)<<16 | c.io.id, dw11: c.io.id<<16 | 1}); err != nil {
		return err
	}
	return nil
}

// xfer leest of schrijft len(p) bytes vanaf blok lba via de ene DMA-pagina.
// Meerdere slot-servicers delen de controller: mutex over de hele transfer.
func (c *Controller) xfer(opc uint32, lba uint64, p []byte, write bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if uint64(len(p)) > 4096 || uint64(len(p))%c.BlockSize != 0 {
		return fmt.Errorf("nvme: length %d not a block multiple (bs=%d, max 4096)", len(p), c.BlockSize)
	}
	nlb := uint64(len(p)) / c.BlockSize
	if lba+nlb > c.Blocks {
		return fmt.Errorf("nvme: lba %d+%d buiten namespace (%d)", lba, nlb, c.Blocks)
	}
	if write {
		dev.Copy(c.buf, p)
	}
	err := c.submit(&c.io, cmd{opc: opc, nsid: nsid, prp1: uint64(c.buf),
		dw10: uint32(lba), dw11: uint32(lba >> 32), dw12: uint32(nlb - 1)})
	if err == nil && !write {
		dev.CopyOut(p, c.buf)
	}
	return err
}

// Write schrijft p (blokveelvoud, ≤ 4KB) naar blok lba.
func (c *Controller) Write(lba uint64, p []byte) error {
	return c.xfer(ioWrite, lba, p, true)
}

// Read leest len(p) bytes (blokveelvoud, ≤ 4KB) vanaf blok lba.
func (c *Controller) Read(lba uint64, p []byte) error {
	return c.xfer(ioRead, lba, p, false)
}

// trim knipt spaties en nullen van een identify-string.
func trim(b []byte) string {
	end := len(b)
	for end > 0 && (b[end-1] == 0 || b[end-1] == ' ') {
		end--
	}
	return string(b[:end])
}
