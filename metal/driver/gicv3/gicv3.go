//go:build tamago && arm64

// Package gicv3 is de irq.Controller voor een GICv3/v4 (distributor +
// redistributor van de aanroepende core, system-register-interface). Eén
// adapter voor elk ARM-board met zo'n GIC — QEMU virt, de Radxa (GIC-600),
// de Altra, de Orion O6N (GIC-700). De Pi's hebben een GIC-400 (v2) en
// krijgen hun eigen driver.
//
// Group 1, niet Group 0 (09-09): op élk bord met TF-A op EL3 staat
// GICD_CTLR.DS=0 en is Group 0 het secure domein — voor ons (non-secure
// EL2/EL1) zijn de Group-0-registers RAZ/WI en meldt ICC_IAR0 nooit iets.
// Non-secure Group 1 is wat de firmware voor het OS overlaat, en het is ook
// op een GIC zonder security (QEMU: DS=1) gewoon beschikbaar. Dus: IGROUPR-
// bit gezet (Group 1; op DS=0 al zo en WI), EnableGrp1NS in GICD_CTLR,
// ICC_IGRPEN1_EL1, en IAR1/EOIR1. Een Group-1-interrupt komt als IRQ (niet
// FIQ) binnen; tamago's vector bedient beide op dezelfde manier.
//
// Wat deze adapter bewust NIET doet: alle interrupts van de distributor
// uitzetten bij Init (tamago's gebruik). Op een bord met firmware-eigen
// lijnen (SCP, EC) is dat niet ons register; wij raken alleen de lijnen aan
// die wij enablen. En de route van een SPI wordt expliciet naar déze core
// gezet, met IRM=0: een app-core mag nooit een target zijn — dat is de
// isolatieregel, niet "meestal".
package gicv3

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/cpu/irq"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

// Distributor-registers.
const (
	gicdCTLR      = 0x0000
	gicdIIDR      = 0x0008
	gicdISPENDR   = 0x0200 // pending-bits, ook van uitgeschakelde SPI's (level: de lijnstand)
	gicdTYPER     = 0x0004 // [4:0] ITLinesNumber: SPI's = 32*(n+1)
	gicdISENABLER = 0x0100
	gicdICENABLER = 0x0180
	gicdIROUTER   = 0x6000 // 8 bytes per INTID (ARE)

	ctlrEnableGrp1NS = 1 << 1 // NS-view: EnableGrp1A; DS=1: EnableGrp1
	ctlrARENS        = 1 << 4 // NS-view: ARE_NS; DS=1: ARE
)

// Redistributor-registers (RD_base).
const (
	gicrCTLR  = 0x0000
	gicrIIDR  = 0x0004
	gicrTYPER = 0x0008 // 64 bit: bit 4 Last, bit 1 VLPIS, [63:32] affinity
	gicrWAKER = 0x0014

	typerLast  = 1 << 4
	typerVLPIS = 1 << 1

	wakerProcessorSleep = 1 << 1
	wakerChildrenAsleep = 1 << 2

	frameRD  = 0x10000 // RD_base-frame (64KB) + SGI_base-frame (64KB) = 128KB
	frameVLP = 0x20000 // + VLPI_base + reserved bij VLPIS (GICv4): 256KB
)

// ICC-systeemregisters (icc_arm64.s).
func writeICCSRE(v uint64)
func readICCSRE() uint64
func writeICCPMR(v uint64)
func writeICCSGI1R(v uint64)
func writeICCIGRPEN1(v uint64)
func readICCIAR1() uint64
func writeICCEOIR1(v uint64)

// Ctrl is één GICv3, geïnitialiseerd voor de aanroepende core.
type Ctrl struct {
	gicd  uintptr
	gicr  uintptr // RD_base van DEZE core
	mpidr uint64
}

// New initialiseert de GIC: gicd is de distributor, gicr het redistributor-
// frame (RD_base) van DEZE core — zie FindRedistributor voor de MADT/DT-
// range. Aanroepen op HOP's core, de core die de interrupts gaat nemen.
func New(gicd, gicr uintptr) *Ctrl {
	c := &Ctrl{gicd: gicd, gicr: gicr, mpidr: dev.MPIDR()}

	// Redistributor wakker (op DS=0 is WAKER secure-only en RAZ/WI: dan is
	// hij al wakker — de firmware bracht deze core op).
	dev.Write32(gicr+gicrWAKER, dev.Read32(gicr+gicrWAKER)&^wakerProcessorSleep)
	for i := 0; dev.Read32(gicr+gicrWAKER)&wakerChildrenAsleep != 0; i++ {
		if i > 10000 {
			fmt.Println("gicv3: redistributor stays asleep (WAKER) — continuing")
			break
		}
		time.Sleep(100 * time.Microsecond)
	}

	// CPU-interface: system-register-interface aan (SRE), alle prioriteiten
	// door (PMR 0xff), Group 1 aan.
	writeICCSRE(readICCSRE() | 1)
	writeICCPMR(0xff)
	writeICCIGRPEN1(1)

	// Distributor: affinity routing + Group 1 (NS) aan — idempotent, en
	// zonder de andere bits (Group 0 is van de firmware) aan te raken.
	dev.Write32(gicd+gicdCTLR, dev.Read32(gicd+gicdCTLR)|ctlrARENS|ctlrEnableGrp1NS)
	dev.MB()
	return c
}

// Describe is één regel voor de bootlog: IIDR's en CTLR — de meting dat we
// met de goede blokken praten (GIC-700: GICD_IIDR 0x0402143b).
func (c *Ctrl) Describe() string {
	return fmt.Sprintf("GICD %#x (IIDR %#x, CTLR %#x), GICR %#x (IIDR %#x, TYPER %#x), ICC_SRE %#x",
		c.gicd, dev.Read32(c.gicd+gicdIIDR), dev.Read32(c.gicd+gicdCTLR),
		c.gicr, dev.Read32(c.gicr+gicrIIDR), dev.Read64(c.gicr+gicrTYPER), readICCSRE())
}

func (c *Ctrl) Enable(l irq.Line) error {
	return enableLine(c.gicd, c.gicr, l.ID, c.mpidr)
}

func (c *Ctrl) Disable(l irq.Line) {
	if l.ID >= 0 && l.ID < firstSpecial {
		base, n, bit := lineReg(c.gicd, c.gicr, l.ID)
		dev.Write32(base+0x180+4*n, bit) // ICENABLER, W1C: never read-modify-write peers
		dev.MB()
	}
}

// Claim leest ICC_IAR1 en doet meteen de EOI (EOImode 0: priority drop én
// deactivate); een speciaal nummer (1020+) is "niets".
func (c *Ctrl) Claim() (irq.Line, bool) {
	id := int(readICCIAR1() & 0xffffff)
	if id >= firstSpecial {
		return irq.Line{}, false
	}
	writeICCEOIR1(uint64(id))
	return irq.Line{ID: id}, true
}

// Complete: de EOI zat al in Claim.
func (c *Ctrl) Complete(irq.Line) {}

// FindRedistributor zoekt in de GICR-range [base, base+length) het RD_base-
// frame van de core met dit MPIDR: elk frame draagt in GICR_TYPER[63:32]
// de affinity van zijn PE, en is 128KB (v3) of 256KB (v4, VLPIS) groot —
// precies wat de MADT-GICR-structuur (0x0e) en de DT-reg beschrijven. Stopt
// op TYPER.Last of het eind van de range. 0 = niet gevonden.
func FindRedistributor(base, length, mpidr uint64) (uint64, bool) {
	want := uint32(mpidr&0xffffff | mpidr>>32&0xff<<24) // aff0-2 | aff3
	for off := uint64(0); off+frameRD <= length; {
		// GICR_TYPER staat op +0x8; +0 is CTLR|IIDR en matchte op QEMU alleen
		// per ongeluk (core 0 = affinity 0, IIDR hoog woord 0) — O6N 09-09: "EE".
		typer := dev.Read64(uintptr(base + off + gicrTYPER))
		if uint32(typer>>32) == want {
			return base + off, true
		}
		if typer&typerLast != 0 {
			break
		}
		// v3: RD + SGI = 2 frames; v4 (VLPIS): RD + SGI + VLPI + reserved =
		// 4 frames (GICv3/4 §12.10). Tot 17-09 stond hier 3 frames voor v4:
		// op de GIC-700 van de O6N liep de zoeker dan langs de frames heen
		// ("no redistributor for MPIDR 0xa00") en bleef de NIC gepold.
		if typer&typerVLPIS != 0 {
			off += 4 * frameRD
		} else {
			off += 2 * frameRD
		}
	}
	return 0, false
}

// PendingSPIs geeft de SPI's in [lo, hi] die nu pending staan bij de
// distributor — óók als ze uit staan: voor een level-lijn is dat gewoon de
// lijnstand. Zo vindt een board de NIC-lijn door het masker van de chip te
// openen en te kijken welke SPI opkomt, zonder ooit een onbekende lijn scherp
// te zetten (de M4-les van 18-09, hier zonder risico).
func (c *Ctrl) PendingSPIs(lo, hi int) []int {
	var out []int
	if lo < 32 {
		lo = 32
	}
	if hi >= firstSpecial {
		hi = firstSpecial - 1
	}
	for w := lo >> 5; w <= hi>>5; w++ {
		bits := dev.Read32(c.gicd + gicdISPENDR + 4*uintptr(w))
		for b := 0; bits != 0 && b < 32; b++ {
			if bits&(1<<uint(b)) != 0 {
				if id := w<<5 + b; id >= lo && id <= hi {
					out = append(out, id)
				}
			}
		}
	}
	return out
}

// KickSGI is het SGI-nummer waarmee HOP een slapende app-core wekt (kern/
// slots waker.go via board.Cores.Kick). 1: de SGI's 0-7 laat TF-A als
// Non-secure Group 1 achter, 8-15 zijn van de secure wereld.
const KickSGI = 1

// PrepareSGI maakt de redistributor van een app-core klaar om SGI id te
// ontvangen: wakker (WAKER), Group 1 (IGROUPR0, NS-write mag genegeerd
// worden) en scherp (ISENABLER0). Vanaf HOP's core, vóór de eerste kick.
// gicr = het RD_base-frame van díe core (MADT GICC of FindRedistributor).
func PrepareSGI(gicr uintptr, id int) {
	dev.Write32(gicr+gicrWAKER, dev.Read32(gicr+gicrWAKER)&^wakerProcessorSleep)
	for i := 0; dev.Read32(gicr+gicrWAKER)&wakerChildrenAsleep != 0 && i < 1<<16; i++ {
	}
	sgi := gicr + frameRD // SGI_base-frame: IGROUPR0 +0x80, ISENABLER0 +0x100
	bit := uint32(1) << uint(id&31)
	dev.Write32(sgi+0x80, dev.Read32(sgi+0x80)|bit)
	dev.MB()
	dev.Write32(sgi+0x100, bit)
	dev.MB()
}

// SendSGI stuurt SGI id naar de core met dit MPIDR (IRM=0: één doel; de
// target-list is de aff0-bit binnen de aff1-groep).
func SendSGI(id int, mpidr uint64) {
	aff0 := mpidr & 0xff
	v := (mpidr>>32&0xff)<<48 | (mpidr>>16&0xff)<<32 | uint64(id&0xf)<<24 | (mpidr>>8&0xff)<<16 | uint64(1)<<aff0
	writeICCSGI1R(v)
}
