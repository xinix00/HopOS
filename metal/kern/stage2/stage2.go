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
// slot, nul relocatiewerk, nul overhead — de MMU doet het.
//
// 4KB-granule, 39-bit IPA (VTCR.T0SZ=25, startlevel 1): L1 entries
// each select one L2 table of 2MB blocks for the private partition.
// The ABI tail belongs to that same map. GB0 stays reserved for an optional
// framebuffer grant; apps have no mapping of node boot scratch or firmware.
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

	"github.com/xinix00/HopOS/metal/v2/abi/a64"
	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/cpu/el2"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

// Descriptor-bits (stage-2): AF, SH=inner, S2AP en MemAttr per gebruik.
const (
	descTable = 0x3 // L1/L2-entry → volgende tabel
	descBlock = 0x1 // L2-entry → 2MB-blok
	descPage  = 0x3 // L3-entry → 4KB-pagina

	attrAF      = 1 << 10
	attrSHInner = 0x3 << 8
	attrRW      = 0x3 << 6 // S2AP: lezen+schrijven
	attrNormal  = 0xF << 2 // MemAttr: normal, WB cacheable (stage-1 wint bij device)
	attrNormNC  = 0x5 << 2 // MemAttr: normal non-cacheable (framebuffer-grant:
	// de scanout leest mee, dus geen cache-contract met de app)

	blockRW   = descBlock | attrAF | attrSHInner | attrRW | attrNormal
	blockRWNC = descBlock | attrAF | attrSHInner | attrRW | attrNormNC
	pageRWNC  = descPage | attrAF | attrSHInner | attrRW | attrNormNC

	// IPALimit is the exclusive address limit of the 39-bit stage-2 regime.
	IPALimit uint64 = 1 << 39

	l1Off = 0x0000
	// Eleven existing table pages avoid extra reservation for small apps:
	// +1000..5000 and +a000..f000. Contexts at +6000/+6800 and the
	// framebuffer pages at +7000..9000 retain their existing locations.
	inlineL2Pages = 11
	l2FbOff       = 0x7000 // FB-grant-L2 (GrantWindow): identity-venster op de
	// firmware-framebuffer, alleen gevuld voor het slot dat de grant houdt
	// De twee rand-L3's van dat venster: een framebuffer is zelden 2MB-aligned,
	// dus kop en staart worden pagina-precies gemapt i.p.v. een heel blok
	// (anders krijgt de grant-houder tot ~4MB firmware-geheugen erbij). Meer dan
	// twee zijn er nooit nodig: het middenstuk gaat als 2MB-blokken. 0x8000..
	// 0xFFFF van het slotblok was vrij (zie de indeling in abi/layout).
	l3FbHeadOff = 0x8000
	l3FbTailOff = 0x9000
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
// A valid empty FLIP can still have powered-on cores waiting in the park loop.
// This is independent of adopting live contexts and their exact switch code.
var preserveParked bool

func PreserveParkedCores() { preserveParked = true }

func checkParkedCores() {
	for c := 1; c <= layout.NumAppCores(); c++ {
		if state := dev.Read64(layout.ParkMboxPA(c)); state > 1 {
			panic(fmt.Sprintf("stage2: empty flip has an unparked core %d (mailbox %#x)", c, state))
		}
	}
}

func InitVectors() {
	if preserveParked && !adoptingNow() {
		checkParkedCores()
	}
	// Eerst de switch-code-kopie de plan-regio in (docs/kern-flip.md): daarná
	// geven de el2-accessors de kopie-adressen, zodat de thunks hieronder én
	// elke dispatch (CtxBootPC/CtrlSMPTramp via de board-accessors) buiten het
	// kern-image wijzen. Op de host een no-op.
	installSwitchCode()

	// Het fysieke plan-adres dat de switch nodig heeft gaat per core het
	// sched-blok in (hieronder): switch.s is daardoor volledig SP/TPIDR-relatief.
	// Eén adres is genoeg — de control-page van een bewoner komt uit zijn
	// ctx-blok (CtxCtrlPA), want die woont in zijn partitie en is dus niet uit
	// een plan-basis te berekenen.
	s2PA := uint64(layout.VecBasePA())
	entryPA := el2.EntryPC()

	// ADOPTIE (kern-flip, docs/kern-flip.md): draaien er al bewoners in deze
	// plan-regio, dan staat alles wat initAppCoreRegion schrijft er al — met
	// exact dezelfde bytes, want installSwitchCode verifieerde de som. Wat dan
	// overblijft is pure schade: de Clears maken vensters waarin een core die
	// net trapt in nullen springt, en het CtxState-Empty zou élke levende
	// bewoner uit de rotatie schrijven. De revoke-vectoren hieronder gaan wél
	// altijd vers: die zijn HOP-core-privé (geen app-core kijkt ernaar) en ze
	// dragen de chainload-handler van DEZE kern.
	if !adoptingNow() {
		initAppCoreRegion(s2PA, entryPA)
	}

	// De revoke-handler van de HOP-core: ingeplugd op offset 0x400 (synchrone
	// exception vanuit een lager EL) van de vectortabel waar cpuinit VBAR_EL2
	// van core 0 op zette (Plan.TrapVecPA). Alleen dát slot — de rest van de
	// tabel blijft van het board (rpi5: de faultdump-bootdiagnostiek; QEMU:
	// leeg). Daar landt de HVC uit Revoke; de handler doet TLBI ALLE1IS
	// (invalideert álle EL1&0 stage-1+2-vertalingen inner-shareable) en keert
	// terug. De andere apps her-walken meteen hun geldige tabel (nanoseconden);
	// het net-ingetrokken slot walkt zijn genulde tabel → stage-2-fault →
	// CPU_OFF.
	// De handler kiest op de HVC-immediate (ESR.ISS): #0 = revoke (TLBI),
	// #3 = kick (een fast IPI naar x0 — board.Cores.Kick op Apple, de wek van
	// een app-core die met WFI slaapt; de encodering is Apple's, maar het
	// pad wordt alleen gelopen door een board dat Kick zo invult), al het
	// andere = chainload — de kern-flip (docs/kern-flip.md). Encodings
	// geverifieerd met clang/objdump (31-08; kick 02-09 uit m1n1 smp.c). Het
	// revoke-pad klobbert alleen x16 (caller-saved; hvcRevoke is een gewone
	// Go-call), het kick-pad niets; het chain-pad klobbert vrij — het keert
	// nooit terug.
	rvecs := layout.TrapVecPA()
	if rvecs == 0 {
		panic("stage2: Plan.TrapVecPA ontbreekt — de HOP-core heeft geen EL2-vectortabel om de revoke-handler in te pluggen")
	}
	revoke := []uint32{
		0xd53c5210, // mrs  x16, esr_el2
		0x92403e10, // and  x16, x16, #0xffff     (HVC-immediate)
		0xb40000d0, // cbz  x16, revoke (+6)
		0xf1000e1f, // cmp  x16, #3
		0x54000121, // b.ne chain (+9)
		0xd51df020, // msr  s3_5_c15_c0_1, x0     (IPI_RR_GLOBAL_EL1 ← aff0 | aff1<<16)
		0xd5033fdf, // isb
		0xd69f03e0, // eret
		// revoke:
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
		// chain: de kern-flip-sprong (ChainloadEL2: x0 = nieuwe entry,
		// x1 = firmware-x0 voor de nieuwe kern). EL1's MMU/caches uit vóór de
		// sprong — de nieuwe kern ligt buiten de RAM-declaratie van de oude,
		// dus élke EL1-fetch erheen zou anders door stale/device-mappings
		// vertalen. 0x30d00800 = SCTLR_EL1 RES1-bits, M/C/I/A/WXN uit — exact
		// de waarde die s2tramp (el2.s) een verse app-core geeft. De nieuwe
		// kern komt op EL2 binnen (dit ís EL2) en doorloopt zijn volle
		// cpuinit → main-pad, net als bij een firmware-boot.
		0xd2810011, // movz x17, #0x0800
		0xf2a61a11, // movk x17, #0x30d0, lsl #16
		0xd5181011, // msr  sctlr_el1, x17
		0xd5033fdf, // isb
		0xaa0003f0, // mov  x16, x0               (entry)
		0xaa0103e0, // mov  x0, x1                (firmware-x0: DTB of 0)
		0xd2800001, // movz x1, #0                (x1 = 0: "geen firmware" voor een UEFI-stub, init.s fwentry)
		// Same I_HYGIENE sequence as app dispatch and the VHE chainload.
		// Invalidate before the first fetch from the new image: its early
		// boot cache maintenance cannot repair instructions already fetched.
		0xd508751f, // ic iallu
		0xd5033f9f, // dsb sy
		0xd5033fdf, // isb
		0xd61f0200, // br   x16
	}
	for w, ins := range revoke {
		dev.Write32(rvecs+0x400+uintptr(w)*4, ins)
	}
	// Vectoren worden als instructies gefetcht (EL2); ongecached geschreven,
	// dus vegen zodat ook een cacheable fetch (SCTLR_EL2.I-staat is
	// firmware-afhankelijk) ze vers uit DRAM haalt. Eénmalig bij boot.
	dev.CleanInv(rvecs, 0x800)
	dev.MB()
}

// initAppCoreRegion vult wat een APP-CORE aanraakt: de vector-thunks, de
// parkeerlus, de sched-blokken en de ctx-staten. Bij een gewone boot is dit
// verse grond; bij een geadopteerde kern-flip draaien er cores in — dan slaat
// InitVectors deze functie over (zie daar).
func initAppCoreRegion(s2PA, entryPA uint64) {
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
	// The parked-core protocol is stable across FLIP. Cores may currently
	// execute this loop even with no residents, so leave those instructions
	// untouched; only a true cold boot installs them.
	if !preserveParked {
		for w, ins := range park {
			dev.Write32(pc+uintptr(w)*4, ins)
		}
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
	initCoreSchedules(s2PA, preserveParked)
	// De ctx-staat van elke KOOI expliciet op Empty (per-slot, tot de kooi-cap):
	// HOP leest dit woord (Get/waitCtxDead in kern/slots) al vóór de eerste
	// Build van dat slot, en verse DRAM is geen nul.
	for s := 1; s <= layout.MaxSlots; s++ {
		dev.Write64(layout.CageTablePA(s)+layout.CtxOff+layout.CtxState, layout.CtxEmpty)
		dev.Write64(layout.CageTablePA(s)+layout.SMPCtxOff+layout.CtxState, layout.CtxEmpty)
	}
	dev.CleanInv(vecs, 0x800)
	dev.MB()
}

func initCoreSchedules(s2PA uint64, keepParked bool) {
	for c := 0; c <= layout.NumAppCores(); c++ {
		mb := layout.ParkMboxPA(c)
		if keepParked {
			// Never transiently clear word0: the existing park loop interprets
			// anything other than 1 as a dispatch and immediately reads word1.
			dev.Clear(mb+8, layout.ParkMboxLen-8)
		} else {
			dev.Clear(mb, layout.ParkMboxLen)
		}
		dev.Write64(mb+layout.SchedS2PA, s2PA)
	}
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
	dev.Clear(layout.CageTablePA(i), layout.CtxOff)
	dev.CleanInv(layout.CageTablePA(i), layout.CtxOff)
	dev.MB()
	hvcRevoke()
}

// TableReserve returns the node-only bytes appended to a partition's physical
// claim. Small partitions use the cage's existing table pages. A single 2MB
// allocation grain holds every L2 page required by the 39-bit IPA regime.
// Invalid ranges are rejected by Build before any memory is touched.
func TableReserve(ipaBase, size uint64) uint64 {
	if size == 0 || ipaBase >= IPALimit || size > IPALimit-ipaBase {
		return 0
	}
	pages := ((ipaBase & ((1 << 30) - 1)) + size + (1 << 30) - 1) >> 30
	if pages > inlineL2Pages {
		return 2 << 20
	}
	return 0
}

// Build schrijft de stage-2-tabellen voor slot i en geeft het fysieke adres
// van de L1-tabel terug (voor VTTBR_EL2, gezet door de EL2-trampoline).
// ipaBase is het linkadres-bereik van de image; paBase/size is de fysieke
// partitie die HOP voor deze task alloceerde (variabel per job). Het
// IPA-bereik [ipaBase, ipaBase+size) wordt op [paBase, paBase+size) gelegd.
// The range may cross GB boundaries within the 39-bit IPA window.
// The caller also owns TableReserve(ipaBase, size) bytes at paBase+size.
// That node-only table reservation is never included in the app map.
// De ABI-staart van het slot (control-page, hop-ABI-ringen, frame-ringen) heeft
// hier geen eigen parameter en geen eigen venster meer: die ligt in de partitie
// (layout, ABIVersion 2) en valt dus binnen dezelfde map.
func Build(i int, ipaBase, paBase, size uint64) (uint64, error) {
	if i < 1 || i > layout.MaxSlots {
		return 0, fmt.Errorf("slot %d buiten bereik", i)
	}
	const blockSize = uint64(2 << 20)
	if size == 0 || (ipaBase|paBase|size)&(blockSize-1) != 0 {
		return 0, fmt.Errorf("stage-2: partition base and nonzero size must be 2MB aligned")
	}
	// Check subtraction bounds before adding or writing: invalid input must
	// neither wrap the address space nor damage an existing table/context.
	if ipaBase < 1<<30 || ipaBase >= IPALimit || size > IPALimit-ipaBase {
		return 0, fmt.Errorf("stage-2: partition %#x + %#x outside IPA RAM window [0x40000000, %#x)", ipaBase, size, IPALimit)
	}
	// Both EL2 trampolines cap VTCR.PS at 44 bits; a board with a smaller
	// physical address space already restricts its own pool to that space.
	const paLimit = uint64(1 << 44)
	extra := TableReserve(ipaBase, size)
	if paBase >= paLimit || size > paLimit-paBase || extra > paLimit-paBase-size {
		return 0, fmt.Errorf("stage-2: physical partition %#x + %#x exceeds 44-bit PA space", paBase, size)
	}

	base := layout.CageTablePA(i)
	// The secondary CPU context may belong to another cage. All other
	// metadata belongs to this stopped cage and starts clean, including any
	// old framebuffer grant and its primary context.
	dev.Clear(base, layout.SMPCtxOff)
	dev.Clear(base+layout.SMPCtxOff+layout.CtxLen, layout.CageStride-layout.SMPCtxOff-layout.CtxLen)
	l1 := uint64(base + l1Off)
	if extra != 0 {
		dev.Clear(uintptr(paBase+size), extra)
	}

	// Exactly one visible partition including its ABI tail; node tables are
	// either in this cage's fixed metadata or beyond the visible partition.
	firstGB := ipaBase >> 30
	for off := uint64(0); off < size; off += blockSize {
		ipa := ipaBase + off
		gb := ipa >> 30
		table := gb - firstGB
		l2 := uintptr(paBase+size) + uintptr(table)*4096
		if extra == 0 {
			page := table + 1
			if table >= 5 {
				page += 4 // skip primary/secondary contexts and framebuffer pages
			}
			l2 = base + uintptr(page)*4096
		}
		dev.Write64(base+l1Off+uintptr(gb)*8, uint64(l2)|descTable)
		dev.Write64(l2+uintptr((ipa>>21)&511)*8, (paBase+off)|blockRW)
	}

	// Coherentie ná de tabel-writes: de page-table-walker van de app-core leest
	// deze tabellen cacheable (VTCR IRGN/ORGN=WB), HOP schreef ze ongecached.
	// Een stale (clean) line van een eerdere huurder van dit tabelblok zou de
	// walker een oude tabel laten walken. Vegen vóór CPU_ON; er draait nu geen
	// walker op dit blok, dus niets kan tussen de veeg en de start hercachen.
	dev.CleanInv(base, layout.CageStride)
	if extra != 0 {
		dev.CleanInv(uintptr(paBase+size), uintptr(extra))
	}
	dev.MB()
	return l1, nil
}

// HasGrantWindow verifies an inherited framebuffer mapping without modifying it.
// Only the cage's own fixed tables may be followed; app memory is never trusted
// as a table pointer. Exact descriptors also check access and memory attributes.
func HasGrantWindow(i int, pa, size uint64) (bool, error) {
	if i < 1 || i > layout.MaxSlots || pa == 0 || size == 0 ||
		pa >= 1<<48 || size > (1<<48)-pa {
		return false, fmt.Errorf("slot %d: inherited framebuffer mapping does not match", i)
	}
	lo := pa &^ ((2 << 20) - 1)
	pgLo, pgHi := pa&^0xfff, (pa+size+0xfff)&^0xfff
	ipa := uint64(layout.FbIPA)
	if pgHi-lo > (1<<30)-(ipa&((1<<30)-1)) {
		return false, fmt.Errorf("slot %d: inherited framebuffer mapping does not match", i)
	}
	base := layout.CageTablePA(i)
	if dev.Read64(base+l1Off+uintptr(ipa>>30)*8) == 0 {
		return false, nil
	}
	if dev.Read64(base+l1Off+uintptr(ipa>>30)*8) != uint64(base+l2FbOff)|descTable {
		return false, fmt.Errorf("slot %d: inherited framebuffer mapping does not match", i)
	}
	// Check every entry, including unmapped neighbors: a broader old grant
	// is not evidence of exclusive ownership of this exact framebuffer.
	first, last := ((ipa+pgLo-lo)>>21)&511, ((ipa+pgHi-lo-1)>>21)&511
	for idx := uint64(0); idx < 512; idx++ {
		e := dev.Read64(base + l2FbOff + uintptr(idx)*8)
		if idx < first || idx > last {
			if e != 0 {
				return false, fmt.Errorf("slot %d: inherited framebuffer has extra mappings", i)
			}
			continue
		}
		p := lo + (idx << 21) - (ipa & ((1 << 30) - 1))
		if p >= pgLo && p+(2<<20) <= pgHi && e == p|blockRWNC {
			continue
		}
		var table uintptr
		switch e {
		case uint64(base+l3FbHeadOff) | descTable:
			table = base + l3FbHeadOff
		case uint64(base+l3FbTailOff) | descTable:
			table = base + l3FbTailOff
		default:
			return false, fmt.Errorf("slot %d: inherited framebuffer mapping does not match", i)
		}
		for j := uintptr(0); j < 512; j++ {
			page := p + uint64(j)*0x1000
			want := uint64(0)
			if page >= pgLo && page < pgHi {
				want = page | pageRWNC
			}
			if dev.Read64(table+j*8) != want {
				return false, fmt.Errorf("slot %d: inherited framebuffer page does not match", i)
			}
		}
	}
	return true, nil
}

// GrantWindow mapt een fysiek venster Normal-NC op het vaste IPA-venster
// layout.FbIPA in de bestaande kooi van slot i — de FB-grant
// (kern/slots/fbgrant.go): een lineaire pixelbuffer, geen registers/DMA.
// A fixed low IPA keeps the framebuffer separate from app RAM, regardless
// of its physical address (QEMU-ramfb:
// 0x1bc7a0000 — de vondst van 19-07). Aanroepen ná Build en vóór de dispatch
// (zelfde walker-regime als Build zelf).
//
// PAGINA-PRECIES aan de randen. Een firmware-framebuffer is zelden 2MB-aligned
// (QEMU-ramfb hierboven is dat niet), en met alleen 2MB-blokken kreeg de
// grant-houder daardoor tot ~4MB fysiek geheugen RÓND de framebuffer erbij —
// RW, en FB_BASE verbergt die bytes niet: de app kan elke gemapte IPA lezen én
// schrijven. Wat daar naast ligt is firmware-terrein, dus dat is geen byte om
// weg te geven. Daarom: het volledig gedekte middenstuk als 2MB-blokken (groot
// en goedkoop) en de twee randen via één L3-tabel elk, waarin alleen de
// pagina's staan die écht bij de buffer horen. Twee extra tabellen, ongeacht de
// venstergrootte — de overmapping is nu maximaal de 4KB-afronding aan elke kant,
// het minimum dat een pagineerde MMU toelaat.
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
	if pa+size < pa {
		return fmt.Errorf("fb-grant: venster %#x + %d overflowt", pa, size)
	}
	// lo is het IPA-anker en blijft 2MB-aligned: de app ziet de buffer op
	// FbIPA + (pa-lo) — hetzelfde contract dat fbgrant.Env als FB_BASE afgeeft,
	// dus hier niet aan rekenen zonder die kant mee te nemen.
	lo := pa &^ ((2 << 20) - 1)
	// Wat we daadwerkelijk mappen: alleen de pagina's van de buffer zelf.
	pgLo := pa &^ 0xFFF
	pgHi := (pa + size + 0xFFF) &^ 0xFFF
	if pgHi-lo > (1<<30)-uint64(layout.FbIPA)&((1<<30)-1) {
		return fmt.Errorf("fb-grant: venster %#x..%#x past niet in het FbIPA-GB", lo, pgHi)
	}

	base := layout.CageTablePA(i)
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
	// Verse tabellen: een hergrant mag geen pagina's van een vorig venster
	// laten staan (l2fb wordt hieronder per blok overschreven, maar de
	// rand-L3's kunnen bij een kleiner venster gaten overhouden).
	dev.Clear(uintptr(l2fb), 0x1000)
	dev.Clear(base+l3FbHeadOff, 0x1000)
	dev.Clear(base+l3FbTailOff, 0x1000)

	gbBase := gb << 30
	ipaOf := func(p uint64) uint64 { return uint64(layout.FbIPA) + (p - lo) }
	// De 2MB-grenzen bínnen [pgLo,pgHi): alles daartussen is volledig van de
	// buffer en gaat als blok.
	bStart := (pgLo + (2 << 20) - 1) &^ ((2 << 20) - 1)
	bEnd := pgHi &^ ((2 << 20) - 1)
	for off := bStart; off < bEnd; off += 2 << 20 {
		idx := (ipaOf(off) - gbBase) >> 21
		dev.Write64(uintptr(l2fb)+uintptr(idx)*8, off|blockRWNC)
	}
	// mapEdge hangt één L3-tabel in het 2MB-blok van [from,to) en zet daarin
	// uitsluitend die pagina's. Aanroepen met een [from,to) binnen één blok.
	mapEdge := func(l3 uintptr, from, to uint64) {
		if from >= to {
			return
		}
		blk := ipaOf(from) &^ ((2 << 20) - 1)
		dev.Write64(uintptr(l2fb)+uintptr((blk-gbBase)>>21)*8, uint64(l3)|descTable)
		for p := from; p < to; p += 0x1000 {
			dev.Write64(l3+uintptr((ipaOf(p)-blk)>>12)*8, p|pageRWNC)
		}
	}
	if bStart > pgLo { // kop: [pgLo, min(bStart,pgHi))
		end := bStart
		if pgHi < end {
			end = pgHi // het hele venster zit in één 2MB-blok
		}
		mapEdge(base+l3FbHeadOff, pgLo, end)
	}
	if bEnd >= bStart && bEnd < pgHi { // staart: [max(bEnd,pgLo), pgHi)
		start := bEnd
		if start < pgLo {
			start = pgLo
		}
		mapEdge(base+l3FbTailOff, start, pgHi)
	}

	// Zelfde coherentie-contract als Build: de walker leest cacheable.
	dev.CleanInv(base+l1Off, 0x1000)
	dev.CleanInv(uintptr(l2fb), 0x1000)
	dev.CleanInv(base+l3FbHeadOff, 0x1000)
	dev.CleanInv(base+l3FbTailOff, 0x1000)
	dev.MB()
	return nil
}
