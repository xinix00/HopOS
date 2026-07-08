// Package stage2 bouwt de stage-2-vertaaltabellen (ARMv8, VMSAv8-64) waarmee
// HOP een app-core hardwarematig insluit: de app op EL1 kan mappen wat hij
// wil, maar de IPA→PA-vertaling die HOP hier vastlegt laat alleen zijn eigen
// slot door. Dít is de isolatiebelofte van het plan (fase 4.2) — geen
// conventie maar een MMU-grens die de app niet kan aanraken (de tabellen
// zelf staan in geen enkele map).
//
// De partitie-map is tevens de relocatie: een image is canoniek gelinkt
// (één linkadres, doorgaans het slot-1-bereik) en de stage-2 vertaalt dat
// IPA-bereik naar de fysieke partitie van dít slot. Zelfde artifact op elk
// slot, nul relocatiewerk, nul overhead — de MMU doet het. De device-regio's
// (ctrl/ringen) blijven identity.
//
// Vorm: 4KB-granule, 32-bit IPA (VTCR.T0SZ=32, startlevel 1):
//
//	L1[4]    1GB/entry: [ipa>>30]→L2part, [2]→L2dev (0x80000000-)
//	L2part   2MB-blokken: canoniek IPA-bereik → eigen slot-partitie (PA)
//	L2dev    [384]→L3ctrl (2MB rond CtrlBase), [392+..]→L3ring,
//	         [408+i-1] = eigen 2MB net-ring-blok als blockRW (frame-ringen)
//	L3ctrl   scratch-page read-only (PSCI-conduitkeuze), eigen ctrl-page RW
//	L3ring   de eigen 64KB ring-regio RW
//
// Per slot leeft het blok op layout.Stage2Table(i); Stage2Base+0 draagt de
// gedeelde EL2-parkeervectoren (stage-2-fault ⇒ core parkeert in WFE-lus;
// heartbeat stopt ⇒ HOP's hang-detectie ziet het).
package stage2

import (
	"fmt"

	"hop-os/metal/dev"
	"hop-os/metal/layout"
	"hop-os/metal/psci"
)

// Descriptor-bits (stage-2): AF, SH=inner, S2AP en MemAttr per gebruik.
const (
	descTable = 0x3 // L1/L2-entry → volgende tabel
	descBlock = 0x1 // L2-entry → 2MB-blok
	descPage  = 0x3 // L3-entry → 4KB-pagina

	attrAF      = 1 << 10
	attrSHInner = 0x3 << 8
	attrRW      = 0x3 << 6 // S2AP: lezen+schrijven
	attrRO      = 0x1 << 6 // S2AP: alleen lezen
	attrNormal  = 0xF << 2 // MemAttr: normal, WB cacheable (stage-1 wint bij device)

	blockRW = descBlock | attrAF | attrSHInner | attrRW | attrNormal
	pageRW  = descPage | attrAF | attrSHInner | attrRW | attrNormal
	pageRO  = descPage | attrAF | attrSHInner | attrRO | attrNormal

	l1Off     = 0x0000
	l2PartOff = 0x1000
	l2DevOff  = 0x2000
	l3CtrlOff = 0x3000
	l3RingOff = 0x4000
)

// InitVectors schrijft de gedeelde EL2-vectoren op Stage2Base (2KB-aligned
// per architectuur-eis; 16 entries met stride 0x80). Elke EL2-exception —
// een stage-2-fault (de app greep buiten zijn kooi) of de hard-kill-SGI van
// HOP — rapporteert eerst wáárom hij viel op de eigen control-page
// (vectorindex+1, ESR_EL2, FAR_EL2; slot = MPIDR aff0, op virt gelijk aan de
// core-index) en zet dan de core uit via PSCI CPU_OFF. HOP ziet "core off
// zonder StatusExited" mét syndroom: hard gestopt, slot direct herbruikbaar.
func InitVectors() {
	// CtrlBase en PSCI_CPU_OFF moeten met één movz (+movk) te laden zijn: de
	// adres-immediates hieronder worden uit de constanten gerekend, niet
	// hand-gebakken, zodat de asm nooit stil kan afwijken van layout/psci.
	// Deze invariant bewaakt dat (layout is compile-time, dus dit is in feite
	// een build-time-check die als panic bij de eerste boot verschijnt).
	if uint64(layout.CtrlBase)>>32 != 0 || uint64(layout.CtrlBase)&0xFFFF != 0 {
		panic("stage2: CtrlBase moet 32-bit en 16-bit-shiftbaar zijn (één movz)")
	}
	if psci.CPU_OFF>>32 != 0 {
		panic("stage2: PSCI_CPU_OFF past niet in 32 bits (movz+movk)")
	}
	handler := func(v uint32) []uint32 {
		return []uint32{
			0xd53800a0,                              // mrs  x0, mpidr_el1
			0x92401c00,                              // and  x0, x0, #0xff        (slot = aff0)
			movz(1, uint32(layout.CtrlBase>>16), 16), // movz x1, #(CtrlBase>>16), lsl #16
			0x8b003021,                              // add  x1, x1, x0, lsl #12  (eigen ctrl-page)
			movz(4, v+1, 0),                         // movz x4, #(v+1)
			strX(4, 1, layout.CtrlFaultVec),         // str  x4, [x1, #CtrlFaultVec]
			0xd53c5202,                              // mrs  x2, esr_el2
			strX(2, 1, layout.CtrlFaultESR),         // str  x2, [x1, #CtrlFaultESR]
			0xd53c6003,                              // mrs  x3, far_el2
			strX(3, 1, layout.CtrlFaultFAR),         // str  x3, [x1, #CtrlFaultFAR]
			0xd5033fbf,                              // dmb  sy                    (publiceer vóór CPU_OFF)
			movz(0, uint32(psci.CPU_OFF>>16), 16),   // movz x0, #(CPU_OFF>>16), lsl #16
			movk(0, uint32(psci.CPU_OFF&0xffff), 0), // movk x0, #(CPU_OFF&0xffff)
			0xd4000003,                              // smc  #0
			0x14000000,                              // b .  (onbereikbaar)
		}
	}
	dev.Clear(uintptr(layout.Stage2Base), 0x800)
	for v := uintptr(0); v < 16; v++ {
		for w, ins := range handler(uint32(v)) {
			dev.Write32(uintptr(layout.Stage2Base)+v*0x80+uintptr(w)*4, ins)
		}
	}
	dev.MB()
}

// Minimale AArch64-encoders voor de vector-generator: één bron van waarheid
// (de constanten) i.p.v. hand-gebakken instructiewoorden. Zie ARM ARM C6.2.
//
//	movz Xd, #imm16, lsl #shift   (shift ∈ {0,16,32,48})
//	movk Xd, #imm16, lsl #shift
//	str  Xt, [Xn, #off]           (off veelvoud van 8, 64-bit)
func movz(rd, imm16, shift uint32) uint32 {
	return 0xD2800000 | (shift/16)<<21 | (imm16&0xFFFF)<<5 | rd&0x1F
}

func movk(rd, imm16, shift uint32) uint32 {
	return 0xF2800000 | (shift/16)<<21 | (imm16&0xFFFF)<<5 | rd&0x1F
}

func strX(rt, rn, off uint32) uint32 {
	return 0xF9000000 | (off/8)<<10 | (rn&0x1F)<<5 | rt&0x1F
}

// Build schrijft de stage-2-tabellen voor slot i en geeft het fysieke adres
// van de L1-tabel terug (voor VTTBR_EL2, gezet door de EL2-trampoline).
// ipaBase is het linkadres-bereik van de image; paBase/size is de fysieke
// partitie die HOP voor deze task alloceerde (variabel per job). Het
// IPA-bereik [ipaBase, ipaBase+size) wordt op [paBase, paBase+size) gelegd.
// size ≤ één 1GB-blok vanaf ipaBase (aanroeper begrenst dit) → één L2-tabel.
func Build(i int, ipaBase, paBase, size uint64) (uint64, error) {
	if i < 1 || i > layout.MaxSlots {
		return 0, fmt.Errorf("slot %d buiten bereik", i)
	}
	base := layout.Stage2Table(i)
	dev.Clear(base, layout.Stage2Stride)

	l1 := uint64(base + l1Off)
	l2Part := uint64(base + l2PartOff)
	l2Dev := uint64(base + l2DevOff)
	l3Ctrl := uint64(base + l3CtrlOff)
	l3Ring := uint64(base + l3RingOff)

	// L1: 1GB-entries. Een IPA-bereik in het GB van de ctrl/ring-regio deelt
	// zijn L2 met de device-L3's (indexes botsen niet: partitie ≤ idx 351,
	// ctrl/ring op 384/392, net-ringen op 408+).
	partL2 := l2Part
	if ipaBase>>30 == uint64(layout.CtrlBase)>>30 {
		partL2 = l2Dev
	}
	dev.Write64(base+l1Off+uintptr(ipaBase>>30)*8, partL2|descTable)
	dev.Write64(base+l1Off+uintptr(uint64(layout.CtrlBase)>>30)*8, l2Dev|descTable)

	// Partitie als 2MB-blokken: IPA (linkadres) → PA (gealloceerde partitie).
	gbBase := ipaBase &^ ((1 << 30) - 1)
	for off := uint64(0); off < size; off += 2 << 20 {
		idx := (ipaBase + off - gbBase) >> 21
		dev.Write64(uintptr(partL2)+uintptr(idx)*8, (paBase+off)|blockRW)
	}

	// L2dev → L3's voor de ctrl- en ring-regio (pagina-granulariteit).
	devGB := uint64(layout.CtrlBase) &^ ((1 << 30) - 1)
	dev.Write64(uintptr(l2Dev)+uintptr((uint64(layout.CtrlBase)-devGB)>>21)*8, l3Ctrl|descTable)
	ringPA := uint64(layout.RingOutbox(i)) &^ ((2 << 20) - 1)
	dev.Write64(uintptr(l2Dev)+uintptr((ringPA-devGB)>>21)*8, l3Ring|descTable)

	// Het eigen 2MB net-ring-blok (frame-ringen app↔switch) als één blok RW;
	// andermans blokken staan nergens in deze map.
	netPA := uint64(layout.NetRingTX(i))
	dev.Write64(uintptr(l2Dev)+uintptr((netPA-devGB)>>21)*8, netPA|blockRW)

	// L3ctrl: boot-scratch read-only (conduitkeuze), eigen ctrl-page RW.
	dev.Write64(uintptr(l3Ctrl)+0*8, uint64(layout.BootScratch)|pageRO)
	ctrl := uint64(layout.CtrlPage(i))
	dev.Write64(uintptr(l3Ctrl)+uintptr((ctrl-uint64(layout.CtrlBase))>>12)*8, ctrl|pageRW)

	// L3ring: de eigen 64KB ring-regio.
	ring := uint64(layout.RingOutbox(i))
	for off := uint64(0); off < layout.RingStride; off += 4 << 10 {
		pa := ring + off
		dev.Write64(uintptr(l3Ring)+uintptr((pa-ringPA)>>12)*8, pa|pageRW)
	}

	dev.MB()
	return l1, nil
}
