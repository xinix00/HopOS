// apple.go — dezelfde controller, andere aanlanding: de ANS.
//
// Op Apple silicon zit de SSD niet als PCIe-device op een bus maar achter een
// co-processor (ANS, Apple NVMe Storage) met zijn eigen mailbox-firmware. Het
// NVMe-protocol daarboven is gewoon NVMe — dezelfde queues, dezelfde opcodes,
// dezelfde completions — dus alles in nvme.go geldt hier ook. Drie dingen zijn
// anders, en die staan in dit bestand:
//
//  1. De submission-doorbel is "lineair": je schrijft niet de nieuwe tail maar
//     het SLOT waar de opdracht staat. De completion-doorbellen zijn wél de
//     gewone (0x1004 / 0x100c bij DSTRD=0).
//  2. Elke opdracht heeft naast zijn 64-byte SQE een TCB van 128 bytes in een
//     tweede tabel: de NVMMU leest daaruit welke kant de DMA op gaat en welke
//     buffers erbij horen. Na elke completion moet dat slot ongeldig verklaard
//     worden, anders loopt de tabel vol.
//  3. De coprocessor praat terug via een mailbox. Wij hebben hem niets te
//     vertellen — iBoot heeft de firmware al gestart, wij nemen alleen de
//     NVMe-controller over — maar zijn berichten moeten wél weggelezen worden,
//     anders loopt zijn uitgaande mailbox vol en houdt hij op.
//
// Referentie: m1n1 src/nvme.c (en src/asc.c voor de mailbox). Wat m1n1 daar
// méér doet is de coprocessor van nul opstarten (rtkit_boot, 725 regels); dat
// hoeven wij niet, want we treffen hem draaiend aan met BOOT_STATUS op OK.
package nvme

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/rtkit"
)

// Apple-registers bovenop de gewone NVMe-kaart (m1n1 src/nvme.c).
const (
	regBootStatus = 0x1300
	bootStatusOK  = 0xde71ce55

	regIOQCmds       = 0x1200
	regIOQCQEs       = 0x1208
	regMaxPendCmds   = 0x1210
	regLinearSQCtrl  = 0x24908
	linearSQCtrlEn   = 1 << 0
	regDBLinearASQ   = 0x2490c
	regDBLinearIOSQ  = 0x24910
	regNVMMUNum      = 0x28100
	regNVMMUASQBase  = 0x28108
	regNVMMUIOSQBase = 0x28110
	regNVMMUTCBInval = 0x28118
	regNVMMUTCBStat  = 0x29120

	tcbSize       = 128
	tcbFromDevice = 1 << 0
	tcbToDevice   = 1 << 1
	tcbOffFlags   = 1
	tcbOffSlot    = 2
	tcbOffLen     = 4
	tcbOffPRP1    = 24
	tcbOffPRP2    = 32

	appleDataOff = 0x18000
	applePRPOff  = appleDataOff + maxTransferSize
	appleDMANeed = applePRPOff + prpListSize
)

// AppleConfig zijn de adressen die de ADT levert (via het param-blok van het
// board) plus de coprocessor die ervoor staat. Op M4 zijn NVMe en NVMMU twee
// verschillende vensters; op M1-M3 wijzen ze naar hetzelfde.
type AppleConfig struct {
	NVMe  uintptr
	NVMMU uintptr
	// RTKit is de coprocessor. InitApple voert eerst zijn opstartgesprek: ook
	// als hij al draait moet dat, want zonder wekbericht blijft hij in de
	// slaapstand die iBoot achterliet en negeert de NVMe-controller elke
	// schrijf naar CC (gemeten 29-08: CSTS.RDY werd nooit 1).
	RTKit *rtkit.Dev
}

// serviceCoprocessor handelt af wat de coprocessor kwijt wil: syslog-regels
// bevestigen, geheugenverzoeken beantwoorden. Een volle uitgaande mailbox is
// een coprocessor die wacht, en een wachtende coprocessor doet geen DMA meer —
// dus dit hoort in elke wachtlus.
func (c *Controller) serviceCoprocessor() {
	if c.rt != nil {
		if err := c.rt.Poll(); err != nil && c.rtErr == nil {
			c.rtErr = err
		}
	}
}

// AppleDiag vertelt wat de coprocessor en de NVMMU ervan vinden. Alleen voor de
// meetbank: een opdracht die niet terugkomt is hier of een slapende
// coprocessor, of een NVMMU-slot dat blijft hangen.
func (c *Controller) AppleDiag() string {
	// Een diagnose mag nooit zelf omvallen. Faalt de bring-up al vóór de
	// queues (bijvoorbeeld op de RTKit-handshake), dan staan Base, nvmmu en de
	// admin-queue nog op nul, en dan leest dit register-voor-register op adres
	// 0 — een data-abort in de functie die had moeten vertellen wát er misging
	// (gemeten 30-08: ESR 0x96000007, FAR 0x0, precies op het escalatiepad).
	if c.Base == 0 {
		return "controller never reached its registers (bring-up failed before that)"
	}
	if c.admin.cq == 0 || c.nvmmu == 0 {
		return fmt.Sprintf("boot=%#x csts=%#x cc=%#x — no queues yet, rtkit=%v",
			dev.Read32(c.Base+regBootStatus), dev.Read32(c.Base+regCSTS),
			dev.Read32(c.Base+regCC), c.rtErr)
	}
	adm := c.admin.cq + uintptr(c.admin.head)*cqeSize
	return fmt.Sprintf("boot=%#x csts=%#x cc=%#x tcb_stat=%#x adm_cqe[%d]=%08x/%08x/%08x/%08x rtkit=%v",
		dev.Read32(c.Base+regBootStatus), dev.Read32(c.Base+regCSTS), dev.Read32(c.Base+regCC),
		dev.Read32(c.nvmmu+regNVMMUTCBStat), c.admin.head,
		dev.Read32(adm), dev.Read32(adm+4), dev.Read32(adm+8), dev.Read32(adm+12), c.rtErr)
}

// writeTCB vult het NVMMU-slot dat bij deze opdracht hoort. De richting van de
// DMA leidt de chip niet zelf af: oneven opcodes schrijven (write = 0x01),
// even lezen (read = 0x02, identify = 0x06) — precies zoals tg3's tabel het
// doet, en zoals m1n1 het overneemt.
func (c *Controller) writeTCB(q *queue, tag uint32, m cmd) {
	tcb := q.tcb + uintptr(tag)*tcbSize
	dev.Clear(tcb, tcbSize)
	if m.prp1 != 0 {
		if m.opc&1 != 0 {
			dev.Write8(tcb+tcbOffFlags, tcbToDevice)
		} else {
			dev.Write8(tcb+tcbOffFlags, tcbFromDevice)
		}
	}
	dev.Write8(tcb+tcbOffSlot, uint8(tag))
	dev.Write32(tcb+tcbOffLen, m.dw12)
	dev.Write64(tcb+tcbOffPRP1, m.prp1)
	dev.Write64(tcb+tcbOffPRP2, m.prp2)
	dev.MB()
}

// InitApple neemt de NVMe-controller van een draaiende ANS over. dmaBase moet
// op 16KB uitgelijnd zijn (de paginamaat van dit silicium), device-gemapt en
// door de SART toegelaten — anders komt de coprocessor er niet bij.
func (c *Controller) InitApple(cfg AppleConfig, dmaBase uintptr, dmaSize uint64) error {
	if dmaSize < appleDMANeed {
		return fmt.Errorf("nvme: DMA-regio %d bytes, ANS vraagt %d", dmaSize, appleDMANeed)
	}
	if dmaBase&0x3fff != 0 {
		return fmt.Errorf("nvme: DMA-regio %#x niet op 16KB uitgelijnd", dmaBase)
	}
	c.Base, c.nvmmu, c.rt = cfg.NVMe, cfg.NVMMU, cfg.RTKit

	// Eerst de coprocessor wakker praten; pas daarna heeft de NVMe-kant zin.
	if c.rt != nil {
		if err := c.rt.Boot(); err != nil {
			return err
		}
	}

	// En dan wachten tot de NVMe-persoonlijkheid van de coprocessor er staat.
	// Dit hoort ná de handshake: het opstartgesprek zet die kant opnieuw op, dus
	// een OK-status van vóór het gesprek zegt niets (gemeten 29-08: ervoor
	// kijken gaf een controller die de ene boot wel en de andere niet ready
	// werd).
	deadline := time.Now().Add(time.Second)
	for dev.Read32(c.Base+regBootStatus) != bootStatusOK {
		if time.Now().After(deadline) {
			return fmt.Errorf("nvme: ANS boot status %#x (verwacht %#x) — firmware niet klaar",
				dev.Read32(c.Base+regBootStatus), bootStatusOK)
		}
		c.serviceCoprocessor()
	}

	// De queues. Elk paar heeft drie tabellen: de TCB's voor de NVMMU, de
	// opdrachten zelf, en de completions. Allemaal 16KB-uitgelijnd.
	c.admin = queue{tcb: dmaBase, sq: dmaBase + 0x4000, cq: dmaBase + 0x8000, phase: 1, id: 0}
	c.io = queue{tcb: dmaBase + 0xC000, sq: dmaBase + 0x10000, cq: dmaBase + 0x14000, phase: 1, id: 1}
	c.buf = dmaBase + appleDataOff
	c.prpList = dmaBase + applePRPOff
	c.MaxTransfer = maxTransferSize
	dev.Clear(dmaBase, appleDMANeed)

	// DSTRD is 0 op dit silicium; CAP uitlezen doet m1n1 hier niet en de
	// completion-doorbellen liggen op de vaste plek die daarbij hoort.
	c.dstrd = 0

	// De lineaire submission-modus en de NVMMU: hoeveel slots, en waar de twee
	// TCB-tabellen liggen. Dit moet vóór het inschakelen van de controller.
	dev.Write32(c.Base+regLinearSQCtrl, dev.Read32(c.Base+regLinearSQCtrl)|linearSQCtrlEn)
	dev.Write32(c.Base+regMaxPendCmds, (qEntries-1)<<16|(qEntries-1))
	dev.Write32(c.nvmmu+regNVMMUNum, qEntries-1)
	write64LoHi(c.nvmmu+regNVMMUASQBase, uint64(c.admin.tcb))
	write64LoHi(c.nvmmu+regNVMMUIOSQBase, uint64(c.io.tcb))
	dev.MB()

	// Vanaf hier is het gewone NVMe: controller uit, admin-queue aanmelden, aan.
	//
	// LET OP dat we CC niet overschrijven maar bijstellen. iBoot laat er een
	// waarde in achter (hier 0x474000: shutdown-normal, 128-byte SQE's) en de
	// entry-maten dáárin zijn wat de controller gebruikt. Een verse CC met onze
	// eigen maten erin komt niet ready — gemeten 29-08, en m1n1 doet het
	// daarom ook zo: alleen SHN wissen en EN zetten.
	const ccSHN = 3 << 14
	dev.Write32(c.Base+regCC, dev.Read32(c.Base+regCC)&^uint32(ccEnable))
	if err := c.waitCSTS(0, 5*time.Second); err != nil {
		return err
	}
	dev.Write32(c.Base+regAQA, (qEntries-1)<<16|(qEntries-1))
	write64LoHi(c.Base+regASQ, uint64(c.admin.sq))
	write64LoHi(c.Base+regACQ, uint64(c.admin.cq))
	dev.MB()
	dev.Write32(c.Base+regCC, dev.Read32(c.Base+regCC)&^uint32(ccSHN)|ccEnable)
	if err := c.waitCSTS(1, 5*time.Second); err != nil {
		return fmt.Errorf("%w (CC %#x, boot status %#x)", err,
			dev.Read32(c.Base+regCC), dev.Read32(c.Base+regBootStatus))
	}

	// Alleen de controller-identify. De namespace bevrágen mag niet: de
	// firmware antwoordt daarop met NVME_PERM_ERR en valt om (gemeten 29-08,
	// gelezen uit zijn eigen crashlog). m1n1 doet die vraag ook niet — het
	// neemt blokken van 4KB aan. De capaciteit halen we uit TNVMCAP, dat wél
	// in de controller-identify staat.
	if err := c.identifyCtrl(); err != nil {
		return err
	}
	c.BlockSize = 4096

	// I/O-queue-paar, en daarna de twee schrijfacties die alleen deze
	// generatie vraagt: zonder die twee crasht de coprocessor met een
	// crashlog waarin de I/O-queues op nul staan (m1n1, NVME_T8132).
	if err := c.createIOQueues(); err != nil {
		return err
	}
	write64LoHi(c.Base+regIOQCQEs, uint64(c.io.cq))
	write64LoHi(c.Base+regIOQCmds, uint64(c.io.sq))
	dev.MB()
	return c.capacityFromGPT()
}

// capacityFromGPT bepaalt hoe groot de schijf is. De ANS meldt dat namelijk
// niet: TNVMCAP is nul, en de namespace bevragen mag niet. De schijf zegt het
// zelf — de GPT-header op LBA 1 draagt het adres van zijn reservekopie, en die
// ligt per definitie op het laatste blok.
//
// Zonder GPT weigeren we. Een NVMe-transfer zonder bovengrens is precies hoe je
// andermans data overschrijft, en een schijf zonder partitietabel is er een
// waarvan we de indeling niet kennen — daar hoort HopOS al helemaal niet aan te
// komen.
func (c *Controller) capacityFromGPT() error {
	c.Blocks = 2 // net genoeg om LBA 1 te mogen lezen
	buf := make([]byte, c.BlockSize)
	if err := c.Read(1, buf); err != nil {
		c.Blocks = 0
		return fmt.Errorf("nvme: GPT-header lezen: %w", err)
	}
	if string(buf[0:8]) != "EFI PART" {
		c.Blocks = 0
		return fmt.Errorf("nvme: geen GPT op LBA 1 (%q) — capaciteit onbekend, schijf blijft dicht", buf[0:8])
	}
	last := binary.LittleEndian.Uint64(buf[32:])
	if last == 0 || last > 1<<44 {
		c.Blocks = 0
		return fmt.Errorf("nvme: GPT wijst zijn reservekopie op blok %d aan — onbruikbaar", last)
	}
	c.Blocks = last + 1
	return nil
}

// write64LoHi schrijft een 64-bit registerpaar in twee helften. Deze registers
// nemen geen enkele 64-bit toegang aan; m1n1 heeft er een eigen helper voor.
func write64LoHi(addr uintptr, v uint64) {
	dev.Write32(addr, uint32(v))
	dev.Write32(addr+4, uint32(v>>32))
}

// Shutdown geeft de coprocessor terug in de staat waarin we hem aantroffen: de
// I/O-queues weg, de controller netjes uitgezet, en de coprocessor in slaap.
// Dit hoort echt te gebeuren — een achtergelaten ANS is een ANS die de volgende
// boot niet meer opengaat.
func (c *Controller) Shutdown() error {
	if c.nvmmu == 0 {
		return nil // gewone PCIe-NVMe: niets bijzonders af te sluiten
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.submit(&c.admin, cmd{opc: admDeleteSQ, dw10: c.io.id}); err != nil {
		return err
	}
	if err := c.submit(&c.admin, cmd{opc: admDeleteCQ, dw10: c.io.id}); err != nil {
		return err
	}

	// CC.SHN op "normaal afsluiten" en wachten tot de controller meldt dat hij
	// klaar is; daarna pas de enable eraf.
	const ccSHNNormal, cstsSHSTDone = 1 << 14, 2 << 2
	dev.Write32(c.Base+regCC, dev.Read32(c.Base+regCC)&^uint32(3<<14)|ccSHNNormal)
	deadline := time.Now().Add(5 * time.Second)
	for dev.Read32(c.Base+regCSTS)&(3<<2) != cstsSHSTDone {
		if time.Now().After(deadline) {
			return fmt.Errorf("nvme: shutdown did not complete (CSTS %#x)", dev.Read32(c.Base+regCSTS))
		}
		c.serviceCoprocessor()
	}
	dev.Write32(c.Base+regCC, dev.Read32(c.Base+regCC)&^uint32(ccEnable))
	if err := c.waitCSTS(0, 5*time.Second); err != nil {
		return err
	}
	c.serviceCoprocessor()

	if c.rt != nil {
		return c.rt.Sleep()
	}
	return nil
}
