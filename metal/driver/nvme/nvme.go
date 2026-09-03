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
	"runtime"
	"sync"
	"time"

	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/pcie"
	"github.com/xinix00/HopOS/metal/v2/driver/rtkit"
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
	admDeleteSQ = 0x00
	admCreateSQ = 0x01
	admDeleteCQ = 0x04
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

	dmaPageSize     = 4096
	maxTransferSize = 1 << 20 // één volledig system/storage-frame
	prpListSize     = dmaPageSize
	genericDataOff  = 4 * dmaPageSize
	genericPRPOff   = genericDataOff + maxTransferSize
	genericDMANeed  = genericPRPOff + prpListSize
)

// queue is één SQ/CQ-paar met poll-state.
type queue struct {
	sq, cq uintptr
	tcb    uintptr // ANS: de NVMMU-tabel bij deze queue (0 = gewone NVMe)
	tail   uint32  // SQ-tail (producer, wij)
	head   uint32  // CQ-head (consumer, wij)
	phase  uint32  // verwachte phase-bit in de CQ
	id     uint32
}

// Controller is één NVMe-controller met één actieve namespace.
type Controller struct {
	Base uintptr // BAR0

	BlockSize   uint64
	Blocks      uint64 // namespace-grootte in blokken
	MaxTransfer uint64 // grootste Read/Write in bytes
	Model       string

	mu         sync.Mutex // serialiseert I/O (één in-flight command, één DMA-buf)
	dstrd      uint64
	totalBytes uint64     // TNVMCAP uit de controller-identify
	nvmmu      uintptr    // ANS: de NVMMU naast het registerblok (0 = gewone NVMe)
	rt         *rtkit.Dev // ANS: de coprocessor die vóór de controller staat
	rtErr      error      // eerste fout uit zijn mailbox (een crash meldt zich daar)
	idDump     string     // de eerste woorden van de identify-buffer (meetbank)
	admin      queue
	io         queue
	buf        uintptr // aaneengesloten DMA-data voor identify en de blok-API
	prpList    uintptr // pagina met DMA-adressen voor transfers van > 2 pagina's
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
	prp1, prp2 uint64
	dw10, dw11 uint32
	dw12       uint32
}

// submit schrijft de command in de SQ, belt de doorbell en polt de CQ tot de
// completion binnen is. Geeft de statuscode (0 = succes) terug.
func (c *Controller) submit(q *queue, m cmd) error {
	cid := q.tail // uniek genoeg: één command in flight per queue
	if c.nvmmu != 0 {
		// De ANS: altijd slot 0. De lineaire submissiemodus wijst het slot aan
		// in plaats van de tail, en de afstand tussen entries in de queue komt
		// uit CC.IOSQES — een waarde die iBoot zet en die niet met onze
		// struct-maat hoeft te kloppen. Op slot 0 doet die afstand niet mee.
		cid = 0
	}
	// LET OP: het slot, niet de tail. Op het ANS-pad staat de opdracht altijd op
	// slot 0 en wijst de deurbel dat slot aan; de opdracht ergens anders
	// neerzetten laat de firmware een oude opdracht lezen bij een verse TCB, en
	// dat meldt hij als NVME_PERM_ERR — met een crash erachteraan die de hele
	// coprocessor tot de volgende power-reset onbruikbaar maakt (gekost: vier
	// boots, 29-08).
	sqe := q.sq + uintptr(cid)*sqeSize
	dev.Clear(sqe, sqeSize)
	dev.Write32(sqe+0, m.opc|cid<<16)
	dev.Write32(sqe+4, m.nsid)
	dev.Write64(sqe+24, m.prp1)
	dev.Write64(sqe+32, m.prp2)
	dev.Write32(sqe+40, m.dw10)
	dev.Write32(sqe+44, m.dw11)
	dev.Write32(sqe+48, m.dw12)
	dev.MB()

	// Aanbieden. Op de ANS gaat dat anders: eerst het NVMMU-slot vullen, dan de
	// mailbox leegtrekken (een wachtende coprocessor doet geen DMA), en dan de
	// lineaire doorbel — die krijgt het SLOT, niet de nieuwe tail.
	if c.nvmmu != 0 {
		c.writeTCB(q, cid, m)
		c.serviceCoprocessor()
		db := uintptr(regDBLinearIOSQ)
		if q.id == 0 {
			db = regDBLinearASQ
		}
		dev.Write32(c.Base+db, cid)
	}
	if c.nvmmu == 0 {
		q.tail = (q.tail + 1) % qEntries
		dev.Write32(c.doorbell(q, false), q.tail)
	}

	// Poll de completion (phase-bit wisselt per CQ-omloop). Gosched per ronde:
	// deze lus mag tot 5 seconden lopen en op een node met één app-core is dat
	// anders 5 seconden zonder scheduler-punt — de heartbeat-goroutine, de
	// servicers en de andere slots staan dan stil omdat één disk-command hangt.
	// Een gezonde completion is er binnen microseconden, dus de yield kost niets
	// op de blije weg.
	cqe := q.cq + uintptr(q.head)*cqeSize
	deadline := time.Now().Add(5 * time.Second)
	for {
		status := dev.Read32(cqe + 12)
		if (status>>16)&1 == q.phase {
			dev.MB()
			// De NVMMU houdt het slot vast tot je het ongeldig verklaart; doe
			// je dat niet, dan is de tabel na 64 opdrachten vol en hangt de
			// volgende. TCB_STAT meldt of de invalidatie aankwam.
			if c.nvmmu != 0 {
				dev.Write32(c.nvmmu+regNVMMUTCBInval, cid)
				if st := dev.Read32(c.nvmmu + regNVMMUTCBStat); st != 0 {
					return fmt.Errorf("nvme: NVMMU invalidation for slot %d failed (%#x)", cid, st)
				}
			}
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
		c.serviceCoprocessor()
		runtime.Gosched()
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
	if dmaSize < genericDMANeed {
		return fmt.Errorf("nvme: DMA-regio %d bytes, minimaal %d", dmaSize, genericDMANeed)
	}
	cap := dev.Read64(c.Base + regCAP)
	c.dstrd = (cap >> 32) & 0xf
	if mqes := cap & 0xffff; mqes+1 < qEntries {
		return fmt.Errorf("nvme: MQES %d < %d", mqes+1, qEntries)
	}

	// DMA-indeling: vier queue-pagina's, één MiB aaneengesloten data en één
	// PRP-lijstpagina. Eén system/storage-frame kan zo één NVMe-opdracht zijn.
	c.admin = queue{sq: dmaBase, cq: dmaBase + 4096, phase: 1, id: 0}
	c.io = queue{sq: dmaBase + 2*4096, cq: dmaBase + 3*4096, phase: 1, id: 1}
	c.buf = dmaBase + genericDataOff
	c.prpList = dmaBase + genericPRPOff
	c.MaxTransfer = maxTransferSize
	dev.Clear(dmaBase, genericDMANeed)

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

	if err := c.identify(); err != nil {
		return err
	}
	return c.createIOQueues()
}

// identify leest modelnaam, namespace-grootte en blokmaat. Los van Init omdat
// het ANS-pad (apple.go) dezelfde stappen in een andere volgorde doet.
func (c *Controller) identify() error {
	if err := c.identifyCtrl(); err != nil {
		return err
	}
	return c.identifyNS()
}

// identifyCtrl leest de controller zelf (CNS=1): modelnaam voor de log, en
// TNVMCAP — de totale capaciteit in bytes, die het ANS-pad gebruikt omdat het
// de namespace niet mag bevragen.
func (c *Controller) identifyCtrl() error {
	if err := c.submit(&c.admin, cmd{opc: admIdentify, prp1: uint64(c.buf), dw10: 1}); err != nil {
		return err
	}
	model := make([]byte, 40)
	dev.CopyOut(model, c.buf+24)
	c.Model = trim(model)
	c.totalBytes = dev.Read64(c.buf + 280) // TNVMCAP, lage helft van 128 bits
	// MDTS=0 betekent "geen gemelde limiet". Anders is het maximum 2^MDTS
	// maal de 4KB-controllerpagina. Onze eigen één-MiB-DMA-regio blijft altijd
	// de harde bovengrens.
	if mdts := dev.Read8(c.buf + 77); mdts != 0 && mdts < 52 {
		if limit := uint64(dmaPageSize) << mdts; limit < c.MaxTransfer {
			c.MaxTransfer = limit
		}
	}
	return nil
}

// identifyNS leest namespace 1 (CNS=0): grootte + blokmaat (LBAF/FLBAS).
func (c *Controller) identifyNS() error {
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
	return nil
}

// createIOQueues meldt het I/O-queue-paar aan (CQ eerst; PC=1, geen interrupts).
func (c *Controller) createIOQueues() error {
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

// dataPRPs maakt de standaard-NVMe-paginawijzers voor n bytes in c.buf. PRP1
// wijst naar de eerste pagina. PRP2 wijst voor twee pagina's direct naar de
// tweede, en bij grotere transfers naar een lijst met alle vervolgpagina's.
// Eén lijstpagina bevat 512 adressen; onze één-MiB-buffer vraagt er 255.
func (c *Controller) dataPRPs(n uint64) (prp1, prp2 uint64) {
	prp1 = uint64(c.buf)
	if n <= dmaPageSize {
		return prp1, 0
	}
	if n <= 2*dmaPageSize {
		return prp1, uint64(c.buf + dmaPageSize)
	}
	dev.Clear(c.prpList, prpListSize)
	pages := (n + dmaPageSize - 1) / dmaPageSize
	for page := uint64(1); page < pages; page++ {
		dev.Write64(c.prpList+uintptr((page-1)*8), uint64(c.buf)+page*dmaPageSize)
	}
	return prp1, uint64(c.prpList)
}

// xfer leest of schrijft len(p) bytes vanaf blok lba via het DMA-datavenster.
// Meerdere slot-servicers delen de controller: mutex over de hele transfer.
func (c *Controller) xfer(opc uint32, lba uint64, p []byte, write bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Nul bytes expliciet weigeren. NLB is een 0-based veld (dw12 = nlb-1), dus
	// een lege transfer maakt daar 0xffffffff van: een opdracht van 4Gi blokken
	// op één DMA-pagina — de controller DMA't dan ver buiten onze buffer. Er is
	// geen zinnige lege NVMe-transfer, dus dit is een fout van de aanroeper.
	if len(p) == 0 {
		return errors.New("nvme: zero-length transfer")
	}
	if uint64(len(p)) > c.MaxTransfer || uint64(len(p))%c.BlockSize != 0 {
		return fmt.Errorf("nvme: length %d not a block multiple (bs=%d, max %d)",
			len(p), c.BlockSize, c.MaxTransfer)
	}
	nlb := uint64(len(p)) / c.BlockSize
	if lba+nlb > c.Blocks {
		return fmt.Errorf("nvme: lba %d+%d buiten namespace (%d)", lba, nlb, c.Blocks)
	}
	if write {
		dev.Copy(c.buf, p)
	}
	prp1, prp2 := c.dataPRPs(uint64(len(p)))
	err := c.submit(&c.io, cmd{opc: opc, nsid: nsid, prp1: prp1, prp2: prp2,
		dw10: uint32(lba), dw11: uint32(lba >> 32), dw12: uint32(nlb - 1)})
	if err == nil && !write {
		dev.CopyOut(p, c.buf)
	}
	return err
}

// Write schrijft p (blokveelvoud, ≤ MaxTransfer) naar blok lba.
func (c *Controller) Write(lba uint64, p []byte) error {
	return c.xfer(ioWrite, lba, p, true)
}

// Read leest len(p) bytes (blokveelvoud, ≤ MaxTransfer) vanaf blok lba.
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
