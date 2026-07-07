package qemuvirt

// GICv3-minimum voor precies één taak: de hard-kill-SGI. HOP (core 0) stuurt
// een SGI naar een hangende app-core; de EL2-trampoline heeft daar IRQ's naar
// EL2 gerouteerd (HCR_EL2.IMO — de app op EL1 kan ze niet maskeren, en zijn
// ICC-registers zijn onder IMO de virtuele), dus de SGI trapt naar de
// EL2-vectoren die de core via PSCI CPU_OFF uitzetten.
//
// LET OP Pi 5: dit is GICv3 (systeemregister-SGI's); de Pi 5 heeft GIC-400
// (GICv2, memory-mapped GICD_SGIR) — de SGI-send wordt daar een boardvariant.

import (
	"sync"
	"time"

	"hop-os/metal/dev"
)

// Redistributor-layout (GICv3 zonder VLPI's: 2 frames van 64KB per core).
const (
	gicrStride  = 0x20000
	gicrTYPER   = 0x0008  // 64-bit; bit 4 = Last, [39:32] = aff0
	gicrWAKER   = 0x0014  // bit 1 = ProcessorSleep, bit 2 = ChildrenAsleep
	gicrSGIBase = 0x10000 // tweede frame: SGI/PPI-configuratie
	gicrIGROUPR = gicrSGIBase + 0x0080
	gicrISENABL = gicrSGIBase + 0x0100
	gicrICPEND  = gicrSGIBase + 0x0280
	gicrIPRIO   = gicrSGIBase + 0x0400

	gicdCTLR     = 0x0000
	gicdCTLRARE  = 1 << 4
	gicdCTLRGrp1 = 1 << 1
	gicdCTLRRWP  = 1 << 31
	killSGI      = 15 // SGI-intid gereserveerd voor de hard-kill
)

var (
	gicOnce sync.Once
	gicSGI  = map[uint64]uintptr{} // core (aff0) → SGI-frame-basis
)

// icc_sgi1r schrijft ICC_SGI1R_EL1 (zie gic_arm64.s): genereert de SGI.
func icc_sgi1r(v uint64)

// gicInit zet de distributor aan (ARE + groep 1) en maakt elke redistributor
// wakker met de kill-SGI enabled (groep 1, hoogste prioriteit). Eénmalig,
// vanaf core 0; de redistributor-walk stopt bij TYPER.Last.
func gicInit() {
	dev.Write32(GICDBase+gicdCTLR, gicdCTLRARE)
	for dev.Read32(GICDBase+gicdCTLR)&gicdCTLRRWP != 0 {
	}
	dev.Write32(GICDBase+gicdCTLR, gicdCTLRARE|gicdCTLRGrp1)
	for dev.Read32(GICDBase+gicdCTLR)&gicdCTLRRWP != 0 {
	}

	for rd := uintptr(GICRBase); ; rd += gicrStride {
		typer := dev.Read64(rd + gicrTYPER)
		aff0 := (typer >> 32) & 0xff
		gicSGI[aff0] = rd

		// Redistributor wakker (vereist vóór SGI-aflevering).
		dev.Write32(rd+gicrWAKER, dev.Read32(rd+gicrWAKER)&^(1<<1))
		deadline := time.Now().Add(time.Second)
		for dev.Read32(rd+gicrWAKER)&(1<<2) != 0 && time.Now().Before(deadline) {
		}

		dev.Write32(rd+gicrIGROUPR, 0xffffffff)   // SGI's in groep 1
		dev.Write32(rd+gicrIPRIO+(killSGI&^3), 0) // prioriteit 0 (hoogste)
		dev.Write32(rd+gicrISENABL, 1<<killSGI)   // kill-SGI aan

		if typer&(1<<4) != 0 { // Last
			break
		}
	}
	dev.MB()
}

// SGIKill stuurt de hard-kill-SGI naar een core (MPIDR aff0 < 16). De
// EL2-vectoren doen daar CPU_OFF; layout.CtrlFaultVec meldt layout.FaultIRQ.
func SGIKill(core uint64) {
	gicOnce.Do(gicInit)
	dev.MB() // eerdere ctrl-page-writes zichtbaar vóór de IRQ aankomt
	icc_sgi1r(killSGI<<24 | 1<<core)
}

// SGIClearPending haalt een eventueel nog hangende kill-SGI van een core weg.
// Aanroepen vóór een CPU_ON: een pending SGI van een eerdere kill zou de
// verse app anders direct zijn core kosten.
func SGIClearPending(core uint64) {
	if rd, ok := gicSGI[core]; ok {
		dev.Write32(rd+gicrICPEND, 1<<killSGI)
		dev.MB()
	}
}
