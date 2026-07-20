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
//	L1[4]    1GB/entry: [ipa>>30]→L2part, [2]→L2dev (0x80000000-),
//	         [3]→L2net (0xC0000000-: het net-ring-GB)
//	L2part   2MB-blokken: canoniek IPA-bereik → eigen slot-partitie (PA)
//	L2dev    [384]→L3ctrl (2MB rond CtrlBase), [392+..]→L3ring
//	L2net    [i-1] = eigen 2MB net-ring-blok als blockRW (frame-ringen; een
//	         eigen GB — 128×2MB paste niet in het ctrl-GB: slot ≥105 liep
//	         over de 1GB-grens, Altra 15-07)
//	L3ctrl   scratch-page read-only (PSCI-conduitkeuze), eigen ctrl-page RW
//	L3ring   de eigen 64KB ring-regio RW
//
// Per slot leeft het blok op layout.Stage2Table(i), met op +CtxOff het
// switch-contextblok van de coöperatieve core-deling (cpu/el2/switch.s).
// Stage2Base+0 draagt de gedeelde EL2-vectoren van de app-cores: dunne
// thunks naar el2entry — een fault zet de bewoner op dead (rapport op zijn
// ctrl-page) en de core draait door met zijn mede-bewoners, of parkeert als
// er niemand meer is (heartbeat stopt ⇒ HOP's hang-detectie ziet het).
package stage2

import (
	"fmt"

	"hop-os/metal/abi/a64"
	"hop-os/metal/abi/layout"
	"hop-os/metal/cpu/el2"
	"hop-os/metal/dev"
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
	attrNormNC  = 0x5 << 2 // MemAttr: normal non-cacheable (framebuffer-grant:
	// de scanout leest mee, dus geen cache-contract met de app)

	blockRW   = descBlock | attrAF | attrSHInner | attrRW | attrNormal
	blockRWNC = descBlock | attrAF | attrSHInner | attrRW | attrNormNC
	pageRW    = descPage | attrAF | attrSHInner | attrRW | attrNormal
	pageRO    = descPage | attrAF | attrSHInner | attrRO | attrNormal

	l1Off     = 0x0000
	l2PartOff = 0x1000
	l2DevOff  = 0x2000
	l3CtrlOff = 0x3000
	l3RingOff = 0x4000
	l2NetOff  = 0x5000 // het net-ring-GB (NetRingBase heeft een eigen GB —
	// 128×2MB past niet in het ctrl-GB; slot ≥105 liep daar over de
	// 1GB-L2-grens: de Altra-vondst van 15-07)
	// CtxOff (0x6000, abi/layout) is het switch-contextblok — NIET herbruiken.
	l2FbOff = 0x7000 // FB-grant-L2 (GrantWindow): identity-venster op de
	// firmware-framebuffer, alleen gevuld voor het slot dat de grant houdt
)

// InitVectors schrijft de gedeelde EL2-vectoren op Stage2Base (2KB-aligned
// per architectuur-eis; 16 entries met stride 0x80). Elke entry is een dunne
// thunk: x2/x3 naar de per-core sched-scratch (via SP_EL2, door de
// trampolines op mailbox+SchedScratch gezet), de vectorindex in x2, en spring
// naar el2entry (cpu/el2/switch.s). Dáár wonen de drie paden, onderscheiden
// op de HVC-immediate:
//
//   - yield-HVC (#1): de coöperatieve yield van de idle-governor op een
//     gedeelde core — staat saven, één EL2-WFE (de idle-slaap van de core),
//     volgende bewoner hervatten of cold-booten (rotatie over het sched-blok).
//   - exit-HVC (#0): bewoner klaar (applib zette al StatusExited) — ctx-staat
//     dead, roteren zonder rapport.
//   - al het andere (stage-2-abort, getrapte SMC, ...): fault-rapport op de
//     eigen ctrl-page (vectorindex+1, ESR_EL2, FAR_EL2) — zowel een spontane
//     kooi-overtreding als HOP's hard-kill (stage-2-intrekking, zie Revoke) —
//     dan dead en roteren.
//
// Draait er daarna niemand meer op de core, dan parkeert de rotatie hem op de
// parkeerlus: HOP ziet "geparkeerd zonder StatusExited" mét syndroom — hard
// gestopt, slot direct herbruikbaar. Eén bewoner op een core gedraagt zich
// dus exact als vanouds, alleen loopt zijn idle-WFE nu via één EL2-rondje.
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
	// De fysieke plan-adressen die de switch nodig heeft, gaan per core het
	// sched-blok in (hieronder): switch.s is daardoor volledig SP/TPIDR-
	// relatief. De 4KB-uitlijning van ctrl (bits 0-11 vrij voor slot<<12)
	// bewaakt layout.UsePlan; switch.s rekent daarop bij het fault-rapport.
	ctrlPA := uint64(layout.CtrlPagePA(0))
	s2PA := uint64(layout.VecBasePA())
	entryPA := el2.EntryPC()

	// De 16 thunks. LET OP: [sp,#16/#24] is de scratch-indeling die switch.s
	// verwacht (x2/x3; el2entry parkeert daar zelf x0/x1 op +0/+8).
	vecs := layout.VecBasePA()
	dev.Clear(vecs, 0x800)
	for v := uintptr(0); v < 16; v++ {
		code := []uint32{
			0xA9010FE2, // stp x2, x3, [sp, #16]
			a64.Movz(2, uint32(v), 0),
		}
		code = a64.Mov64(code, 3, entryPA)
		code = append(code, 0xD61F0060) // br x3
		for w, ins := range code {
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
	// Sched-blokken (mailbox + core-delingsstaat) schoon: verse DRAM is geen
	// nul (Pi-meting) — word0=0 betekent "cold" (nooit geparkeerd → eerste
	// bring-up via PSCI), en een lege bewonerslijst mag geen rommel dragen.
	// Daarna de plan-PA's die switch.s per core nodig heeft: zo blijft de
	// switch volledig SP/TPIDR-relatief, zonder #defines of Go-globals
	// (die zouden cache-coherentie-zorg meebrengen — dit gebied is al
	// ongecached van beide kanten).
	// Sched-blokken zijn per CORE (mailbox + core-delingsstaat, ParkMboxPA(core)):
	// tot NumAppCores, niet de kooi-cap. Verse DRAM is geen nul, en de plan-PA's
	// (die switch.s per core nodig heeft) moeten er staan vóór de eerste dispatch.
	nc := layout.NumAppCores()
	dev.Clear(pc+0x100, uint64(nc+1)*layout.ParkMboxLen)
	for c := 0; c <= nc; c++ {
		mb := layout.ParkMboxPA(c)
		dev.Write64(mb+layout.SchedS2PA, s2PA)
		dev.Write64(mb+layout.SchedCtrlPA, ctrlPA)
	}
	// De ctx-staat van elke KOOI expliciet op Empty (per-slot, tot de kooi-cap):
	// HOP leest dit woord (Get/waitCtxDead in kern/slots) al vóór de eerste
	// Build van dat slot, en verse DRAM is geen nul.
	for s := 1; s <= layout.MaxSlots; s++ {
		dev.Write64(layout.Stage2TablePA(s)+layout.CtxOff+layout.CtxState, layout.CtxEmpty)
	}

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
		0xd5033f9f, // dsb  sy                    (invalidatie compleet vóór de wek)
		0xd503209f, // sev — wek élke WFE-slaper: een core die in WFE hangt (een
		//             gecrashte runtime in zijn halt-lus, een idle-governor)
		//             doet geen vertaalde toegang en overleefde de intrekking
		//             (verbrande core, gemeten 19-07: browser 100%-cpu-crash →
		//             dedicated core voorgoed weg tot powercycle). Na de wek is
		//             zijn eerstvolgende instructie-fetch een her-walk van de
		//             genulde tabel → stage-2-fault → parkeren. Geparkeerde
		//             cores en yield-wachters weken spurious, checken hun
		//             mailbox en slapen door — daar zijn ze op gebouwd.
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
// WFE-slaper (gecrashte runtime in zijn halt-lus, idle-governor) doet geen
// toegangen en overleefde de intrekking (verbrande core, 19-07); daarom
// eindigt de revoke-handler sinds 20-07 met een SEV — de wek maakt van de
// slaper weer een fetcher, en de fetch faultt. Resteert als enige twijfel de
// pathologische self-branch-lus (`for {}` → `b .`) die de front-end mogelijk
// uit een loop-buffer serveert zónder te hertranslateren; dat is per
// silicium te meten (op QEMU dwingt de HANG=spin-test het af).
func Revoke(i int) {
	// Alleen de TABELLEN van het slot nullen (tot CtxOff — het switch-
	// contextblok erachter blijft staan): de L1-entries worden ongeldig, dus
	// élke IPA in dit slot faultt. Het contextblok mag hier NIET mee: een
	// bewoner van een gedeelde core kan op dit moment precies zijn staat aan
	// het saven zijn (WFE-yield op zijn core) — een clear eroverheen zou een
	// halve context achterlaten die de rotatie daarna als "saved" hervat. De
	// intrekking bereikt hem toch: zijn eerstvolgende hervatting faultt op de
	// genulde tabel en zet de ctx-staat op dead (switch.s, fault-pad).
	// Volgorde is hier heilig — de walker van de app drááit nog en leest de
	// tabellen cacheable:
	//
	//  1. eerst de zeros schrijven (DRAM is dan al ongeldig),
	//  2. dan CleanInv: gooit de tabel-lines die de walker cachede weg (altijd
	//     clean — walkers schrijven niet), zodat een her-walk uit DRAM = zeros
	//     leest. Andersom (vegen vóór het nullen) kon de nog-draaiende walker
	//     tussen de veeg en de zeros de óúde tabel opnieuw cachen → de app zou
	//     de kill overleven op echt silicium (QEMU verhult dit).
	//  3. dan pas de TLBI (via de HVC): ook de al-vertaalde TLB-entries weg.
	dev.Clear(layout.Stage2TablePA(i), layout.CtxOff)
	dev.CleanInv(layout.Stage2TablePA(i), layout.CtxOff)
	dev.MB()
	hvcRevoke()
}

// Build schrijft de stage-2-tabellen voor slot i en geeft het fysieke adres
// van de L1-tabel terug (voor VTTBR_EL2, gezet door de EL2-trampoline).
// ipaBase is het linkadres-bereik van de image; paBase/size is de fysieke
// partitie die HOP voor deze task alloceerde (variabel per job). Het
// IPA-bereik [ipaBase, ipaBase+size) wordt op [paBase, paBase+size) gelegd.
// size ≤ één 1GB-blok vanaf ipaBase (aanroeper begrenst dit) → één L2-tabel.
// netRingPA is de fysieke net-ring van dít slot (de partitie-staart,
// base+appRAM — kern/slots berekent hem per lifecycle, er is geen register):
// één 2MB-blok, dus 2MB-aligned (partAlloc's korrel garandeert dat; hier de
// wacht, want een scheve basis wordt een scheve MMU-map).
func Build(i int, ipaBase, paBase, size, netRingPA uint64) (uint64, error) {
	if i < 1 || i > layout.MaxSlots {
		return 0, fmt.Errorf("slot %d buiten bereik", i)
	}
	if netRingPA == 0 || netRingPA&(layout.NetRingStride-1) != 0 {
		return 0, fmt.Errorf("net-ring-PA %#x ontbreekt of niet 2MB-aligned", netRingPA)
	}
	// Het hele blok schoon, inclusief het switch-contextblok op +CtxOff: een
	// Start gebeurt per contract op een niet-draaiend slot, dus de ctx-staat
	// mag (en moet) hier vers op Empty — de aanroeper zet 'm daarna op
	// Running/BootPending bij de dispatch.
	base := layout.Stage2TablePA(i)
	dev.Clear(base, layout.Stage2Stride)

	l1 := uint64(base + l1Off)
	l2Part := uint64(base + l2PartOff)
	l2Dev := uint64(base + l2DevOff)
	l3Ctrl := uint64(base + l3CtrlOff)
	l3Ring := uint64(base + l3RingOff)
	l2Net := uint64(base + l2NetOff)

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

	// Het eigen 2MB net-ring-blok (frame-ringen app↔switch) als één blok RW,
	// in het eigen net-ring-GB (L2net): IPA-blok → partitie-staart (2MB-
	// aligned, hierboven bewaakt); andermans blokken staan nergens in deze
	// map. De index wordt begrensd: layout-drift hoort hier hard te vallen,
	// niet stil in een buurtabel te schrijven (de slot-105-les van 15-07).
	netIPA := uint64(layout.NetRingTX(i))
	netGB := uint64(layout.NetRingBase) &^ ((1 << 30) - 1)
	netIdx := (netIPA - netGB) >> 21
	if netIdx > 511 {
		return 0, fmt.Errorf("net-ring-IPA %#x buiten het net-ring-GB (index %d)", netIPA, netIdx)
	}
	dev.Write64(base+l1Off+uintptr(netGB>>30)*8, l2Net|descTable)
	dev.Write64(uintptr(l2Net)+uintptr(netIdx)*8, netRingPA|blockRW)

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

// GrantWindow mapt een fysiek venster als 2MB-blokken Normal-NC op het vaste
// IPA-venster layout.FbIPA in de bestaande kooi van slot i — de FB-grant
// (kern/slots/fbgrant.go): een lineaire pixelbuffer, geen registers/DMA.
// Identity kan niet: de kooi-IPA-ruimte is 32-bit (VTCR.T0SZ=32) en een
// firmware-framebuffer mag fysiek boven de 4GB liggen (QEMU-ramfb:
// 0x1bc7a0000 — de vondst van 19-07). Aanroepen ná Build en vóór de dispatch
// (zelfde walker-regime als Build zelf).
//
// Bewust begrensd: het venster moet binnen één GB liggen (een framebuffer is
// ≤ tientallen MB) en het FbIPA-GB (GB0) is in het canonieke beeld van
// niemand — een gevulde L1-entry daar is een plan-fout, geen bedrijfsgeval.
func GrantWindow(i int, pa, size uint64) error {
	if i < 1 || i > layout.MaxSlots {
		return fmt.Errorf("slot %d buiten bereik", i)
	}
	if pa == 0 || size == 0 {
		return fmt.Errorf("fb-grant: leeg venster (%#x, %d)", pa, size)
	}
	lo := pa &^ ((2 << 20) - 1)
	hi := (pa + size + (2 << 20) - 1) &^ ((2 << 20) - 1)
	if hi-lo > (1<<30)-uint64(layout.FbIPA)&((1<<30)-1) {
		return fmt.Errorf("fb-grant: venster %#x..%#x past niet in het FbIPA-GB", lo, hi)
	}

	base := layout.Stage2TablePA(i)
	l2fb := uint64(base + l2FbOff)
	gb := uint64(layout.FbIPA) >> 30
	l1e := base + l1Off + uintptr(gb)*8
	switch cur := dev.Read64(l1e); cur {
	case 0:
		dev.Write64(l1e, l2fb|descTable)
	case l2fb | descTable:
		// idempotent (hergrant op hetzelfde slot)
	default:
		return fmt.Errorf("fb-grant: GB %d van de kooi is al gemapt (%#x) — venster botst met het IPA-beeld", gb, cur)
	}
	gbBase := gb << 30
	for off := lo; off < hi; off += 2 << 20 {
		ipa := uint64(layout.FbIPA) + (off - lo)
		dev.Write64(uintptr(l2fb)+uintptr((ipa-gbBase)>>21)*8, off|blockRWNC)
	}

	// Zelfde coherentie-contract als Build: de walker leest cacheable.
	dev.CleanInv(base+l1Off, 0x1000)
	dev.CleanInv(uintptr(l2fb), 0x1000)
	dev.MB()
	return nil
}
