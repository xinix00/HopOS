package mve

import "github.com/xinix00/HopOS/metal/v2/dev"

// Het registerblok is klein: veertien woorden voor de hele VPU en vijftien per
// hardware-sessie. Alle codec-kennis zit in de firmware, dus dit is echt alles
// wat er aan registers te programmeren valt — een verademing na een NIC.
//
//	0x000 HARDWARE_ID   0x5664 op de Linlon V8 van de O6N
//	0x004 ENABLE        hardware-scheduler aan/uit
//	0x008 NCORES        hoeveel videocores dit blok heeft
//	0x00c NLSID         hoeveel sessies er tegelijk ingeladen kunnen zijn
//	0x010 CORELSID      welke core op welke sessie staat
//	0x014 JOBQUEUE      vier wachtende jobs, 8 bits elk
//	0x018 IRQVE         welke sessie de interrupt veroorzaakte
//	0x024 CLKFORCE      klokken forceren (diagnose)
//	0x030 SVNREV        revisie van het blok
//	0x034 FUSE          uitgeschakelde codecs
//	0x040 PROTCTRL      beveiligde modus (DRM-pad; wij raken het niet aan)
//	0x044 BUSCTRL       burstgedrag op de AXI-bus
//	0x050 RESET         software-reset
const (
	regHardwareID = 0x00
	regEnable     = 0x04
	regNCores     = 0x08
	regNLSID      = 0x0c
	regCoreLSID   = 0x10
	regJobQueue   = 0x14
	regIRQVE      = 0x18
	regClkForce   = 0x24
	regSVNRev     = 0x30
	regFuse       = 0x34
	regProtCtrl   = 0x40
	regBusCtrl    = 0x44
	regReset      = 0x50

	lsidBase   = 0x200
	lsidStride = 0x40

	lsCtrl      = 0x00 // welke cores deze sessie mag en hoeveel
	lsMMUCtrl   = 0x04 // fysiek adres van de L1-tabel
	lsNProt     = 0x08 // niet-beveiligd (wij: altijd)
	lsAlloc     = 0x0c // 0 vrij, 1 gewoon, 2 beveiligd
	lsFlushAll  = 0x10 // MMU-tabellen opnieuw lezen
	lsSched     = 0x14 // deze sessie mag ingepland worden
	lsTerminate = 0x18 // afbreken; leest 0 zodra het klaar is
	lsLIRQVE    = 0x1c // interrupt naar ons; wissen na afhandeling
	lsIRQHost   = 0x20 // interrupt naar de firmware: de deurbel (kick)
	lsStreamID  = 0x2c // SMMU-stroom (blijft 0 zolang de SMMU bypast)
	lsBusAttr0  = 0x30
)

// De fuse-bits vertellen welke codecs in dit exemplaar uitgezet zijn. Alleen
// de twee waar Supports op beslist.
const (
	fuseNoVPX  = 1 << 2
	fuseNoHEVC = 1 << 3
)

// LSID-toewijzing en de CTRL-velden.
const (
	allocFree         = 0
	allocNonProtected = 1
	allocProtected    = 2

	ctrlDisallowShift = 0 // 8 bits: welke cores deze sessie NIET mag
	ctrlMaxCoresShift = 8 // 4 bits: hoeveel cores hij tegelijk mag hebben

	jobSlots    = 4
	jobInvalid  = 0xf // in het lsid-nibble van een slot
	jobLSIDMask = 0xf
)

// regs is het registervenster van één VPU: het blok zelf plus het RCSU
// (reset/strap-registers ernaast, een apart venster in de DSDT).
type regs struct {
	base uintptr
	rcsu uintptr
}

func (r regs) read(off uintptr) uint32     { return dev.Read32(r.base + off) }
func (r regs) write(off uintptr, v uint32) { dev.Write32(r.base+off, v) }

// lsid geeft het registeradres van een veld binnen een hardware-sessie.
func (r regs) lsidOff(id int, off uintptr) uintptr {
	return uintptr(lsidBase+id*lsidStride) + off
}

func (r regs) readLSID(id int, off uintptr) uint32 {
	return dev.Read32(r.base + r.lsidOff(id, off))
}

func (r regs) writeLSID(id int, off uintptr, v uint32) {
	dev.Write32(r.base+r.lsidOff(id, off), v)
}

// jobSlotLSID leest het sessienummer uit slot i van de job-queue.
func jobSlotLSID(q uint32, i int) uint32 { return (q >> (i * 8)) & jobLSIDMask }

// setJobSlot zet sessie en corecount in slot i.
func setJobSlot(q uint32, i int, lsid, ncores uint32) uint32 {
	job := (lsid & jobLSIDMask) | (ncores&0xf)<<4
	return q&^(0xff<<(i*8)) | job<<(i*8)
}

// emptyJobQueue is de waarde waarmee alle vier de slots leeg zijn.
const emptyJobQueue = 0x0f0f0f0f
