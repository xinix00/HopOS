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
// een stage-2-fault, of de app nu spontaan buiten zijn kooi greep óf HOP zijn
// stage-2-map introk (de hard-kill, zie Revoke) — rapporteert eerst wáárom hij
// viel op de eigen control-page (vectorindex+1, ESR_EL2, FAR_EL2) en zet dan de
// core uit via PSCI CPU_OFF. HOP ziet "core off zonder StatusExited" mét
// syndroom: hard gestopt, slot direct herbruikbaar.
//
// Het slot komt uit VTTBR_EL2.VMID — door onze eigen trampoline op het
// slotnummer gezet, door de app onaantastbaar, en board-neutraal (géén
// MPIDR-decodering: QEMU codeert het corenummer in aff0, de Pi-A76 in aff1 —
// de VMID is op beide identiek). Bovendien rapporteert een secundaire SMP-core
// zo op de page van zíjn app (VMID = primair slot), niet op de ctrl-page van
// zijn eigen core-index, die aan niemand toebehoort.
//
// Tevens de revoke-vectoren van de HOP-core zelf op RevokeVecBase: één handler
// (op de HVC-offset) die TLBI ALLE1IS doet. HOP draait op EL1 en kan die
// EL2-instructie niet direct uitvoeren; Revoke doet er een HVC voor. Zie Revoke.
func InitVectors() {
	// De fysieke ctrl-basis en het parkeeradres komen uit het board-plan en
	// kunnen élk 48-bit-adres zijn: altijd movz+movk+movk (bits 32-47/16-31/
	// 0-15). De 4KB-uitlijning van ctrl (bits 0-11 vrij voor slot<<12) bewaakt
	// layout.UsePlan.
	ctrlPA := uint64(layout.CtrlPagePA(0))
	parkPA := uint64(layout.ParkCodePA())

	// parkTo(rd): laad parkPA in x<rd> en spring erheen (BR). HopOS bezit zijn
	// cores — een gevelde/gestopte app-core gaat NIET terug naar de firmware
	// (PSCI CPU_OFF is op de Pi 5-stock een one-way door) maar parkeert op EL2
	// in de WFE-lus op ParkCodePA. HOP dispatcht 'm later via zijn mailbox.
	parkTo := func(rd uint32) []uint32 {
		return []uint32{
			movz(rd, uint32(parkPA>>32), 32),
			movk(rd, uint32(parkPA>>16), 16),
			movk(rd, uint32(parkPA), 0),
			0xD61F0000 | (rd&0x1F)<<5, // br  x<rd>
		}
	}
	// report: schrijf vec/esr/far op de eigen control-page (slot = VTTBR.VMID),
	// gevolgd door parkeren. Clobbert x0-x5.
	report := func(v uint32) []uint32 {
		ins := []uint32{
			0xd53c2100,                      // mrs  x0, vttbr_el2
			0xd370fc00,                      // lsr  x0, x0, #48          (slot = VMID)
			movz(1, uint32(ctrlPA>>32), 32), // movz x1, #(ctrlPA>>32), lsl #32
			movk(1, uint32(ctrlPA>>16), 16), // movk x1, #(ctrlPA>>16), lsl #16
			movk(1, uint32(ctrlPA), 0),      // movk x1, #(ctrlPA&0xffff)
			0x8b003021,                      // add  x1, x1, x0, lsl #12  (eigen ctrl-page)
			movz(4, v+1, 0),                 // movz x4, #(v+1)
			strX(4, 1, layout.CtrlFaultVec), // str  x4, [x1, #CtrlFaultVec]
			0xd53c5202,                      // mrs  x2, esr_el2
			strX(2, 1, layout.CtrlFaultESR), // str  x2, [x1, #CtrlFaultESR]
			0xd53c6003,                      // mrs  x3, far_el2
			strX(3, 1, layout.CtrlFaultFAR), // str  x3, [x1, #CtrlFaultFAR]
			0xd5033fbf,                      // dmb  sy   (publiceer vóór parkeren)
		}
		return append(ins, parkTo(5)...)
	}
	handler := func(v uint32) []uint32 {
		if v != 8 {
			return report(v)
		}
		// Index 8 = synchrone exception vanuit EL1: óf een stage-2-fault
		// (kooi-overtreding / Revoke's ingetrokken map — rapporteren) óf de
		// coöperatieve exit-HVC van applib (EC=0x16 — de app zette al
		// StatusExited, dus niet als fault rapporteren, meteen parkeren).
		// De HVC-tak springt naar het parkTo-blok = de laatste 4 instructies
		// van report(v). Vanaf de b.eq (index 3) is dat offset
		// 4 + (len(rep)-4) - 3 = len(rep)-3 instructies vooruit.
		rep := report(v)
		off := uint32(len(rep) - 3)
		head := []uint32{
			0xd53c5202,                    // mrs  x2, esr_el2
			0xD35AFC43,                    // lsr  x3, x2, #26          (EC)
			0xF100587F,                    // cmp  x3, #0x16            (HVC64?)
			0x54000000 | (off&0x7FFFF)<<5, // b.eq +off (EQ) → parkeren, geen report
		}
		return append(head, rep...)
	}
	// App-core-vectoren op VecBasePA: elke EL2-exception → rapporteer + parkeer.
	// Geen aparte IRQ-vector: de hard-kill loopt via een stage-2-fault (Revoke
	// trekt de map in), die net als een spontane kooi-overtreding op idx 8 landt.
	vecs := layout.VecBasePA()
	dev.Clear(vecs, 0x800)
	for v := uintptr(0); v < 16; v++ {
		for w, ins := range handler(uint32(v)) {
			dev.Write32(vecs+v*0x80+uintptr(w)*4, ins)
		}
	}

	// De parkeerlus op ParkCodePA: TPIDR_EL2 wijst (door de trampoline gezet)
	// naar de eigen mailbox {ctx, doel-PC}. Meld "geparkeerd" (word0=1), wek
	// HOP, en WFE tot HOP een ctx schrijft; spring dan de trampoline in
	// (idempotent — zet stage-2/VBAR/TPIDR opnieuw). Board-neutraal: geen
	// MPIDR-decodering, de identiteit zit in TPIDR_EL2.
	park := []uint32{
		0xd53cd048, // mrs  x8, tpidr_el2        (x8 = mailbox-PA)
		0xd2800029, // mov  x9, #1
		0xf9000109, // str  x9, [x8]             (word0 = 1: geparkeerd, idle)
		0xd5033f9f, // dsb  sy
		0xd503209f, // sev                        (wek HOP's waitStopped)
		0xd503205f, // wfe                        ← lus
		0xf9400100, // ldr  x0, [x8]             (word0)
		0xf100041f, // cmp  x0, #1
		0x54ffffa0, // b.eq -3 (→ wfe)           (nog geen dispatch)
		0xd5033fbf, // dmb  sy
		0xf9400501, // ldr  x1, [x8, #8]         (doel-PC)
		0xd61f0020, // br   x1                    (→ trampoline, x0 = ctx)
	}
	pc := layout.ParkCodePA()
	for w, ins := range park {
		dev.Write32(pc+uintptr(w)*4, ins)
	}
	// Mailboxen schoon: verse DRAM is geen nul (Pi-meting) — word0=0 betekent
	// "cold" (nooit geparkeerd → eerste bring-up via PSCI).
	dev.Clear(pc+0x100, uint64(layout.MaxSlots+1)*layout.ParkMboxLen)

	// De revoke-handler van de HOP-core: ingeplugd op offset 0x400 (synchrone
	// exception vanuit een lager EL) van de vectortabel waar cpuinit VBAR_EL2
	// van core 0 op zette (Plan.RevokeVecPA). Alleen dát slot — de rest van de
	// tabel blijft van het board (rpi5: de faultdump-bootdiagnostiek; QEMU:
	// leeg). Daar landt de HVC uit Revoke; de handler doet TLBI ALLE1IS
	// (invalideert álle EL1&0 stage-1+2-vertalingen inner-shareable) en keert
	// terug. De andere apps her-walken meteen hun geldige tabel (nanoseconden);
	// het net-ingetrokken slot walkt zijn genulde tabel → stage-2-fault →
	// CPU_OFF.
	rvecs := layout.RevokeVecPA()
	revoke := []uint32{
		0xd50c839f, // tlbi alle1is
		0xd5033f9f, // dsb  sy
		0xd5033fdf, // isb
		0xd69f03e0, // eret
	}
	for w, ins := range revoke {
		dev.Write32(rvecs+0x400+uintptr(w)*4, ins)
	}
	// Vectoren worden als instructies gefetcht (EL2); ongecached geschreven,
	// dus vegen zodat ook een cacheable fetch (SCTLR_EL2.I-staat is
	// firmware-afhankelijk) ze vers uit DRAM haalt. Eénmalig bij boot.
	dev.CleanInv(vecs, 0x800)
	dev.CleanInv(rvecs, 0x800)
	dev.MB()
}

// Revoke voert de hard-kill uit op slot i: HOP nult de stage-2-tabel van het
// slot en doet één HVC → TLBI ALLE1IS (via de revoke-vector, want de TLBI is
// EL2-only en HOP draait op EL1). Elke core van dít slot — bij een SMP-app delen
// ze één tabel en één VMID — faultt daarna op zijn eerstvolgende vertaalde
// toegang op zijn eigen EL2-vectoren en zet zichzelf via CPU_OFF uit. Geen
// interrupt-controller nodig. De aanroeper (slots.Stop) polt daarna AFFINITY_INFO.
//
// EERLIJKE GRENS: dit vangt elke core die geheugen aanraakt of instructies
// fetcht met een verse vertaling — dat is alles wat vooruitgang boekt. Een
// pathologische self-branch-lus (`for {}` → `b .`) die de front-end mogelijk uit
// een loop-buffer serveert zónder te hertranslateren is de enige twijfel; dat is
// per silicium te meten (op QEMU dwingt de HANG=spin-test het af).
func Revoke(i int) {
	// De hele tabel-blok van het slot nullen: de L1-entries worden ongeldig, dus
	// élke IPA in dit slot faultt. Volgorde is hier heilig — de walker van de
	// app drááit nog en leest de tabellen cacheable:
	//
	//  1. eerst de zeros schrijven (DRAM is dan al ongeldig),
	//  2. dan CleanInv: gooit de tabel-lines die de walker cachede weg (altijd
	//     clean — walkers schrijven niet), zodat een her-walk uit DRAM = zeros
	//     leest. Andersom (vegen vóór het nullen) kon de nog-draaiende walker
	//     tussen de veeg en de zeros de óúde tabel opnieuw cachen → de app zou
	//     de kill overleven op echt silicium (QEMU verhult dit).
	//  3. dan pas de TLBI (via de HVC): ook de al-vertaalde TLB-entries weg.
	dev.Clear(layout.Stage2TablePA(i), layout.Stage2Stride)
	dev.CleanInv(layout.Stage2TablePA(i), layout.Stage2Stride)
	dev.MB()
	hvcRevoke()
}

// hvcRevoke doet HVC #0 vanuit EL1 → de revoke-vector op EL2 (TLBI ALLE1IS).
// De handler raakt geen GP-registers, dus niets te bewaren. Zie revoke_arm64.s.
func hvcRevoke()

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
	base := layout.Stage2TablePA(i)
	dev.Clear(base, layout.Stage2Stride)

	l1 := uint64(base + l1Off)
	l2Part := uint64(base + l2PartOff)
	l2Dev := uint64(base + l2DevOff)
	l3Ctrl := uint64(base + l3CtrlOff)
	l3Ring := uint64(base + l3RingOff)

	// De tabel is de IPA→PA-vertaling: alle índexen hieronder komen uit het
	// IPA-beeld (de universele layout-constanten die de app ziet), alle
	// wáárden zijn fysiek (de partitie uit de pool, de plan-PA's van
	// ctrl/ringen). Op QEMU wijken die bewust van elkaar af — zo bewijst de
	// regressie de splitsing.
	//
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
	ringIPA := uint64(layout.RingOutbox(i)) &^ ((2 << 20) - 1)
	dev.Write64(uintptr(l2Dev)+uintptr((ringIPA-devGB)>>21)*8, l3Ring|descTable)

	// Het eigen 2MB net-ring-blok (frame-ringen app↔switch) als één blok RW —
	// IPA-blok → fysiek plan-blok (2MB-aligned, bewaakt door UsePlan);
	// andermans blokken staan nergens in deze map.
	netIPA := uint64(layout.NetRingTX(i))
	dev.Write64(uintptr(l2Dev)+uintptr((netIPA-devGB)>>21)*8, uint64(layout.NetRingTXPA(i))|blockRW)

	// L3ctrl: boot-scratch read-only op zijn IPA (conduitkeuze), eigen
	// ctrl-page RW — elk naar hun fysieke plek uit het plan.
	dev.Write64(uintptr(l3Ctrl)+0*8, uint64(layout.BootScratchPA())|pageRO)
	ctrlIPA := uint64(layout.CtrlPage(i))
	dev.Write64(uintptr(l3Ctrl)+uintptr((ctrlIPA-uint64(layout.CtrlBase))>>12)*8,
		uint64(layout.CtrlPagePA(i))|pageRW)

	// L3ring: de eigen 64KB ring-regio, pagina voor pagina IPA → plan-PA.
	ring := uint64(layout.RingOutbox(i))
	for off := uint64(0); off < layout.RingStride; off += 4 << 10 {
		ipa := ring + off
		dev.Write64(uintptr(l3Ring)+uintptr((ipa-ringIPA)>>12)*8,
			(uint64(layout.RingOutboxPA(i))+off)|pageRW)
	}

	// Coherentie ná de tabel-writes: de page-table-walker van de app-core leest
	// deze tabellen cacheable (VTCR IRGN/ORGN=WB), HOP schreef ze ongecached.
	// Een stale (clean) line van een eerdere huurder van dit tabelblok zou de
	// walker een oude tabel laten walken. Vegen vóór CPU_ON; er draait nu geen
	// walker op dit blok, dus niets kan tussen de veeg en de start hercachen.
	dev.CleanInv(base, layout.Stage2Stride)
	dev.MB()
	return l1, nil
}
