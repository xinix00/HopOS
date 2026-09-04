// EL2-switch voor coöperatieve core-deling (fase 6): meerdere apps op één
// fysieke core, elk met zijn eigen stage-2-kooi. Geen timer, geen GIC — het
// wisselmoment is een EXPLICIETE HVC-yield van de idle-governor
// (metal/cpu/idle, alleen op een core die HOP als gedeeld markeerde). Hier
// wordt de volledige EL1-staat van de yielder gesaved, de core geslapen (één
// EL2-WFE op de event stream — het vermogen van een dedicated core blijft) en
// de volgende bewoner hervat of cold-geboot. Bewust een HVC en geen getrapte
// WFE: WFE is op QEMU-TCG een no-op (zou daar nooit trappen — onbewijsbaar in
// de bring-up) en een WFE-trap is op ijzer een heisenbug. Een app die nooit
// yieldt starft zijn buren — dat is per ontwerp (compute hoort op een eigen
// core; HOP's liveness ziet de gestokte heartbeat).
//
// De vectoren (kern/stage2 genereert dunne thunks) springen hierheen met:
//
//	SP_EL2       = eigen sched-scratch (mailbox+SchedScratch, door de
//	               trampolines gezet) — [sp,#16/#24] dragen al x2/x3
//	x2           = vectorindex (0..15)
//	x3           = geklobberd (thunk-sprongdoel)
//	al het overige = live app-staat
//
// Alle adressen zijn TPIDR/SP-relatief of komen uit het sched-blok van de
// core (layout.Sched*: CagePA en CtrlPA, door InitVectors neergelegd) —
// geen #defines, board-neutraal onder elk PA-plan. De layout.Sched*/Ctx*-
// offsets staan hier als literals; layout.go benoemt die koppeling.
//
// EL2/EL1-sysregs via WORD-encodings (Go-assembler kent ze niet bij naam):
// MRS = 0xd5300000 | (op0-2)<<19 | op1<<16 | CRn<<12 | CRm<<8 | op2<<5 | Rt,
// MSR = idem met 0xd5100000 — dezelfde vorm als el2.s/smp.s.

//go:build tamago && arm64

#include "textflag.h"
#include "sysreg.h"

TEXT el2entry(SB),NOSPLIT|NOFRAME,$0
	// x0/x1 óók naar de scratch; daarna zijn x0..x3 werkregisters. De
	// scratch-indeling (SP-relatief): +0 x0, +8 x1, +16 x2, +24 x3.
	STP	(R0, R1), (RSP)

#ifdef VHE
	// Idx 10 (FIQ vanuit EL1): op Apple is dat HOP's kick — de fast IPI
	// waarmee de wekker een app-core wekt die op EL1 draait (yieldSleep). Acken
	// en terug; de app ziet alleen zijn WFI terugkeren.
	CMP	$10, R2
	BEQ	fiq
#endif
	// Alleen idx 8 (synchroon vanuit EL1) draagt een bruikbare ESR; elke
	// andere vector is per definitie een fault-rapport.
	CMP	$8, R2
	BNE	fault
	WORD	$0xd53c5200	// mrs x0, esr_el2
	LSR	$26, R0, R1	// EC
	CMP	$0x16, R1	// HVC vanuit EL1? (het enige coöperatieve pad)
	BNE	fault		// nee → stage-2-fault / SMC / abort: rapporteren
	// HVC-immediate (ESR.ISS, laagste 16 bits) kiest de bedoeling:
	//   #0 = coöperatieve exit (applib zette StatusExited al)
	//   #1 = idle-yield van de governor op een gedeelde core
	//   #4 = wek een sibling-core van dezelfde app (zie wake:)
	AND	$0xFFFF, R0, R3
	CBZ	R3, exited
	CMP	$4, R3
	BEQ	wake
#ifdef VHE
	CMP	$5, R3	// doorbell-ack: alleen waar de fast-IPI een vFIQ werd (Apple)
	BEQ	doorack
#endif

yield:
	// Idle-yield (HVC #1): de bewoner van DEZE core staat in het sched-blok
	// (layout.SchedCurrent; HOP zet hem bij de eerste dispatch, resume/boot
	// houden hem bij). NIET de VMID uit VTTBR: een secundaire core van een
	// SMP-app deelt de VMID van zijn primaire, en schreef zo zijn staat in
	// het ctx-blok van de primaire — waarna zijn eigen blok op "running"
	// bleef staan, zijn rotatie geen levende bewoner zag en de core parkeerde
	// (QEMU + M4, 03-09). Zijn contextblok = CagePA + slot<<16 + CtxOff;
	// CagePA komt uit het sched-blok (SP = scratch = blok+16, dus
	// veld-offset − 16: SchedS2PA(224) → 208, SchedCurrent(48) → 32).
	MOVD	32(RSP), R0	// layout.SchedCurrent = slot van de bewoner
	MOVD	208(RSP), R1	// layout.SchedS2PA
	ADD	R0<<16, R1, R1
	ADD	$0x6000, R1, R1	// x1 = ctx (layout.CtxOff)

	// GPRs: x4..x29 in paren, x30 los; x0..x3 uit de scratch. CtxGPRs=24.
	STP	(R4, R5), 56(R1)
	STP	(R6, R7), 72(R1)
	STP	(R8, R9), 88(R1)
	STP	(R10, R11), 104(R1)
	STP	(R12, R13), 120(R1)
	STP	(R14, R15), 136(R1)
	STP	(R16, R17), 152(R1)
	STP	(R18_PLATFORM, R19), 168(R1)
	STP	(R20, R21), 184(R1)
	STP	(R22, R23), 200(R1)
	STP	(R24, R25), 216(R1)
	STP	(R26, R27), 232(R1)
	STP	(g, R29), 248(R1)
	MOVD	R30, 264(R1)
	LDP	(RSP), (R2, R3)	// originele x0/x1
	STP	(R2, R3), 24(R1)
	// x1 draagt de WEKTIJD (layout.CtxWake): vóór deze CNTVCT-stand hoeft de
	// rotatie deze bewoner niet te hervatten. 0 = nu — een yield zonder
	// wektijd gedraagt zich dus exact als vóór dit veld bestond.
	MOVD	R3, 464(R1)
	LDP	16(RSP), (R2, R3)	// originele x2/x3
	STP	(R2, R3), 40(R1)
	// Wie hier slaapt (layout.CtxKickTarget): het MPIDR-affiniteitswoord van
	// deze core, zodat een sibling hem via HVC #4 kan aanwijzen.
	WORD	$0xd53800a2	// mrs x2, mpidr_el1
	AND	$0xFFFFFF, R2, R2
	MOVD	R2, 480(R1)

	// Hervat-PC = ELR_EL2 (bij een HVC wijst die al ná de hvc-instructie —
	// geen +4 zoals bij een getrapte WFE). SPSR ernaast.
	WORD	$0xd53c4022	// mrs x2, elr_el2
	WORD	$0xd53c4003	// mrs x3, spsr_el2
	STP	(R2, R3), 288(R1)	// layout.CtxResume
	WORD	$0xd5384102	// mrs x2, sp_el0
	WORD	$0xd53c4103	// mrs x3, sp_el1
	STP	(R2, R3), 272(R1)	// layout.CtxSP

	// EL1-sysregs (volgorde = layout.CtxRegime, 304..448): het volledige
	// vertaal/context-regime dat de volgende bewoner NIET mag erven.
	MRS_SCTLR_EL1(2)
	MRS_TCR_EL1(3)
	STP	(R2, R3), 304(R1)
	MRS_TTBR0_EL1(2)
	MRS_TTBR1_EL1(3)
	STP	(R2, R3), 320(R1)
	MRS_MAIR_EL1(2)
	MRS_AMAIR_EL1(3)
	STP	(R2, R3), 336(R1)
	MRS_VBAR_EL1(2)
	WORD	$0xd53bd043	// mrs x3, tpidr_el0
	STP	(R2, R3), 352(R1)
	WORD	$0xd53bd062	// mrs x2, tpidrro_el0
	WORD	$0xd538d083	// mrs x3, tpidr_el1
	STP	(R2, R3), 368(R1)
	MRS_CONTEXTIDR_EL1(2)
	MRS_CPACR_EL1(3)
	STP	(R2, R3), 384(R1)
	MRS_CNTKCTL_EL1(2)
	WORD	$0xd53a0003	// mrs x3, csselr_el1 (op0=3,op1=2,C0,C0,0)
	STP	(R2, R3), 400(R1)
	WORD	$0xd5387402	// mrs x2, par_el1
	MRS_ELR_EL1(3)
	STP	(R2, R3), 416(R1)
	MRS_SPSR_EL1(2)
	MRS_ESR_EL1(3)
	STP	(R2, R3), 432(R1)
	MRS_FAR_EL1(2)
	MOVD	R2, 448(R1)

	// GEEN FP in de EL2-switch. EL2 draait met MMU uit (SCTLR_EL2.M=0,
	// board-cpuinit) → al het EL2-geheugen is Device-nGnRnE, en een SIMD/FP-
	// toegang naar Device is op ijzer een alignment-fault (CONSTRAINED
	// UNPREDICTABLE; QEMU-TCG verhult dit). GP- en sysreg-stores (8-byte-
	// aligned) mógen wél naar Device — die blijven hier. De FP-staat wordt
	// coöperatief bewaard door idle.hvcYield zelf, op de EL1-stack (Normal
	// cacheable): de yield is een gewone functie-aanroep, dus alleen de
	// callee-saved V8–V15 (+ FPCR) hoeven de wissel te overleven, en die
	// zet hvcYield om de HVC heen weg. Zo raakt EL2 nooit een FP-register.

	// Staat → saved (2). DSB: HOP polt deze staat (kern/slots) en de write
	// moet vóór de rotatie/park zichtbaar zijn.
	MOVD	$2, R2
	MOVD	R2, (R1)
	DSB	$15
	B	sleep

exited:
	// Coöperatieve exit: staat → dead (4) en meteen roteren (geen slaap —
	// er is net een slot vrijgekomen, een boot-pending buur mag direct).
	MOVD	32(RSP), R0	// layout.SchedCurrent (zie yield: niet de VMID)
	MOVD	208(RSP), R1	// layout.SchedS2PA
	ADD	R0<<16, R1, R1
	ADD	$0x6000, R1, R1
	MOVD	$4, R2
	MOVD	R2, (R1)
	DSB	$15
	B	rotate

fault:
	// Fault-rapport op de eigen ctrl-page (vec+1, ESR, FAR) — wat de oude
	// vector-encodings inline deden, nu hier met ruimte. x2 = vectorindex.
	// Zowel een kooi-overtreding als HOP's revoke landen hier; daarna is de
	// bewoner dood en draait de rest van de core gewoon door.
	MOVD	32(RSP), R0	// layout.SchedCurrent (zie yield: niet de VMID)
	MOVD	208(RSP), R1	// layout.SchedS2PA
	ADD	R0<<16, R1, R1
	ADD	$0x6000, R1, R1	// x1 = ctx-blok van de bewoner (layout.CtxOff)
	MOVD	8(R1), R3	// layout.CtxCtrlPA = zijn control-page (door HOP gezet)
	ADD	$1, R2, R2
	MOVD	R2, 0x68(R3)	// layout.CtrlFaultVec = vec+1
	WORD	$0xd53c5202	// mrs x2, esr_el2
	MOVD	R2, 0x58(R3)	// layout.CtrlFaultESR
	WORD	$0xd53c6002	// mrs x2, far_el2
	MOVD	R2, 0x60(R3)	// layout.CtrlFaultFAR
	MOVD	$4, R2
	MOVD	R2, (R1)	// ctx-staat → dead (layout.CtxDead)
	DSB	$15
	B	rotate

sleep:
	// De idle-slaap van de core: één WFE op EL2. De event stream van de
	// zojuist geyielde app (CNTKCTL_EL1, door de governor altijd aan) loopt
	// door, dus dit wekt ~elke ms; een al-gearriveerd event valt er meteen
	// doorheen — geen verloren wekker, en het vermogen blijft dat van een
	// dedicated idle core (er wordt exact één keer per tik geroteerd).
	//
	// Hier komt de rotatie ook terug als iedereen leeft maar niemand due is
	// (CtxWake in de toekomst): de event stream is dan de wekker — ARM's
	// mtimecmp, maar zonder armen en zonder probe, want hij staat al aan en is
	// op dit silicium bewezen. Slaapt dus in tikken van ~1ms tot de vroegste
	// wektijd; op QEMU-TCG is WFE een no-op en spint dit warm (by design, zie
	// de QEMU-notitie) maar de heractivering blijft er correct.
	// Tellen op de bewoner in x1 (layout.CtxSleeps): de meetlat van deze
	// slaap — tientallen per seconde is slaap, miljoenen is een spin.
	MOVD	472(R1), R0
	ADD	$1, R0, R0
	MOVD	R0, 472(R1)
#ifdef VHE
	// Apple: WFE slaapt hier niet (CYC_OVRD, buiten bereik op de M4) en de
	// event stream wekt dus niets. m1n1's recept op dit silicium: wfi, en een
	// fast IPI die hem wekt — HOP's wekker stuurt die zodra een bewoner due
	// is of RX heeft (kern/slots waker.go, board.Cores.Kick). Pending IPI
	// wissen ná de wek (IPI_SR_EL1), anders keert de volgende WFI meteen.
	WFI
	WORD	$0xd53df120	// mrs x0, s3_5_c15_c1_1 (IPI_SR_EL1)
	CBZ	R0, rotate
	WORD	$0xd51df120	// msr s3_5_c15_c1_1, x0 (ack)
#else
	WFE
#endif

rotate:
	// Round-robin over de bewonerslijst van deze core (SP-relatief; SP =
	// sched-blok+16): vanaf cursor+1 de eerste bewoner met staat
	// boot-pending (1) of saved (2) die ook aan de beurt ÍS (zijn wektijd is
	// verstreken, layout.CtxWake). x0..x30 zijn hier vrij — de huidige
	// bewoner is gesaved of dood — maar x11 moet de hele scan overleven.
	MOVD	$0, R11		// nog geen levende-maar-niet-due bewoner gezien
	MOVD	64(RSP), R4	// layout.SchedCursor
	MOVD	72(RSP), R5	// layout.SchedCount
	CBZ	R5, park	// geen bewoners: core naar de parkeerlus
	MOVD	R5, R6		// maximaal count stappen
next:
	ADD	$1, R4, R4
	CMP	R5, R4
	BLT	scan
	MOVD	$0, R4		// wrap
scan:
	ADD	$80, RSP, R7	// layout.SchedList
	MOVBU	(R7)(R4), R8	// kandidaat-slot (0 = gat)
	CBZ	R8, skip
	MOVD	208(RSP), R1	// ctx van de kandidaat
	ADD	R8<<16, R1, R1
	ADD	$0x6000, R1, R1
	MOVD	(R1), R9
	CMP	$1, R9		// boot-pending? een verse start wacht nooit
	BEQ	boot
	CMP	$2, R9		// saved?
	BNE	skip
	// Saved — maar al aan de beurt? Vóór zijn wektijd hervatten is nooit fout
	// (hij kijkt, vindt niets, yieldt opnieuw), erna laten liggen wél.
	MOVD	464(R1), R9	// layout.CtxWake
	CBZ	R9, resume	// 0 = nu
	// Bit 63 (layout.CtxWakeNoPeek): een wachter zonder P — alleen zijn
	// wektijd telt, de doorbell hieronder niet (idle.waitSleep).
	TBZ	$63, R9, timed
	AND	$0x7FFFFFFFFFFFFFFF, R9, R9
	CBZ	R9, resume
	WORD	$0xd53be04a	// mrs x10, cntvct_el0
	CMP	R9, R10
	BHS	resume
	B	notdue
timed:
	WORD	$0xd53be04a	// mrs x10, cntvct_el0
	CMP	R9, R10
	BHS	resume		// zijn tijd is gekomen
	// De wektijd wordt VERTROUWD — wie onzin vraagt benadeelt alleen zichzelf.
	// Wel een eis aan de applib: een yield van vóór de wektijd draagt residu
	// in x1 (hier: de fpcr) en zo'n bewoner hoort niet op een gedeelde core —
	// zie de RISC-V-switcher voor de gemeten les (01-08, dove welcome).
	//
	// Niet due — maar de DOORBELL dan? De bewoner wapent hem vlak vóór zijn
	// slaap (idle/rxdoor.go: CtrlRXDoor = gezien-head | bit 63); is het live
	// head-woord van zijn RX-ring (CtxRingHeadPA) daar inmiddels voorbij, dan
	// kwam er verkeer en is de wektijd irrelevant. Ongewapend (bit 63 = 0:
	// pomp wakker, of een app zonder netstack) peekt hier niets — dat houdt
	// een kooi die zijn ring nooit leest uit de resume-lus (ARP-floods!).
	MOVD	8(R1), R12	// layout.CtxCtrlPA
	MOVD	0x110(R12), R12	// layout.CtrlRXDoor
	TBZ	$63, R12, notdue	// niet gewapend: de wektijd regeert
	MOVD	520(R1), R13	// layout.CtxRingHeadPA (0 = geen ring)
	CBZ	R13, notdue
	MOVD	(R13), R13	// live head (producer: hopswitch)
	AND	$0x7FFFFFFFFFFFFFFF, R12, R12	// drempel zonder het gewapend-teken
	CMP	R12, R13
	BNE	resume		// de ring groeide voorbij de drempel: due
notdue:
	MOVD	$1, R11		// levend, alleen nog niet due
skip:
	SUBS	$1, R6, R6
	BNE	next
	CBZ	R11, park	// iedereen dood/leeg → parkeren
	B	sleep		// levend maar niemand due: slaap één tik en kijk opnieuw

boot:
	// Cold boot van een boot-pending bewoner: exact het mailbox-dispatchpad
	// (x0 = ctrl-page, spring de trampoline in), maar dan EL2→EL2 vanaf de
	// rotatie. Staat → running vóór de sprong (HOP's bootPending-poll leest 'm).
	MOVD	R4, 64(RSP)	// cursor bijwerken
	MOVD	R8, 32(RSP)	// layout.SchedCurrent = deze bewoner
	MOVD	$3, R9
	MOVD	R9, (R1)
	DSB	$15
	MOVD	8(R1), R0	// layout.CtxCtrlPA → x0 (zoals PSCI 'm zou geven)
	MOVD	16(R1), R2	// layout.CtxBootPC (s2tramp)
	JMP	(R2)

resume:
	MOVD	R4, 64(RSP)	// cursor bijwerken
	MOVD	R8, 32(RSP)	// layout.SchedCurrent = deze bewoner
	MOVD	$3, R9		// staat → running
	MOVD	R9, (R1)
	DSB	$15

	// VTTBR omzetten naar de EENHEID van deze bewoner (layout.CtxUnitSlot,
	// door HOP gezet): L1 = CagePA + unit<<16 (l1Off = 0), VMID = unit. Voor
	// een gewone bewoner is dat zijn eigen slot; een secundaire core van een
	// SMP-app deelt tabel én VMID van zijn primaire. GEEN TLBI: entries zijn
	// VMID-getagd, de vertalingen van beide bewoners bestaan naast elkaar —
	// dát maakt de wissel goedkoop.
	MOVD	496(R1), R9	// layout.CtxUnitSlot
	MOVD	208(RSP), R2
	ADD	R9<<16, R2, R2
	ORR	R9<<48, R2, R2
	WORD	$0xd51c2102	// msr vttbr_el2, x2

	// EL1-sysregs terug (spiegel van de save; volgorde vrij — de ERET is
	// het synchronisatiepunt).
	LDP	304(R1), (R2, R3)
	MSR_SCTLR_EL1(2)
	MSR_TCR_EL1(3)
	LDP	320(R1), (R2, R3)
	MSR_TTBR0_EL1(2)
	MSR_TTBR1_EL1(3)
	LDP	336(R1), (R2, R3)
	MSR_MAIR_EL1(2)
	MSR_AMAIR_EL1(3)
	LDP	352(R1), (R2, R3)
	MSR_VBAR_EL1(2)
	WORD	$0xd51bd043	// msr tpidr_el0, x3
	LDP	368(R1), (R2, R3)
	WORD	$0xd51bd062	// msr tpidrro_el0, x2
	WORD	$0xd518d083	// msr tpidr_el1, x3
	LDP	384(R1), (R2, R3)
	MSR_CONTEXTIDR_EL1(2)
	MSR_CPACR_EL1(3)
	LDP	400(R1), (R2, R3)
	MSR_CNTKCTL_EL1(2)
	WORD	$0xd51a0003	// msr csselr_el1, x3 (op0=3,op1=2,C0,C0,0)
	LDP	416(R1), (R2, R3)
	WORD	$0xd5187402	// msr par_el1, x2
	MSR_ELR_EL1(3)
	LDP	432(R1), (R2, R3)
	MSR_SPSR_EL1(2)
	MSR_ESR_EL1(3)
	MOVD	448(R1), R2
	MSR_FAR_EL1(2)
	LDP	272(R1), (R2, R3)
	WORD	$0xd5184102	// msr sp_el0, x2
	WORD	$0xd51c4103	// msr sp_el1, x3
	LDP	288(R1), (R2, R3)
	WORD	$0xd51c4022	// msr elr_el2, x2 (hervat-PC: ná de HVC-yield)
	WORD	$0xd51c4003	// msr spsr_el2, x3

	// Geen FP-herstel hier: hvcYield (EL1) herstelt zijn eigen V8–V15/FPCR.

	// GPRs als allerlaatste; x1 (de ctx-pointer zelf) helemaal aan het eind.
	LDP	56(R1), (R4, R5)
	LDP	72(R1), (R6, R7)
	LDP	88(R1), (R8, R9)
	LDP	104(R1), (R10, R11)
	LDP	120(R1), (R12, R13)
	LDP	136(R1), (R14, R15)
	LDP	152(R1), (R16, R17)
	LDP	168(R1), (R18_PLATFORM, R19)
	LDP	184(R1), (R20, R21)
	LDP	200(R1), (R22, R23)
	LDP	216(R1), (R24, R25)
	LDP	232(R1), (R26, R27)
	LDP	248(R1), (g, R29)
	MOVD	264(R1), R30
	LDP	40(R1), (R2, R3)
	MOVD	24(R1), R0
	MOVD	32(R1), R1
	ISB	$15
	ERET

park:
	// Geen bewoner meer te draaien: core naar de parkeerlus (CagePA +
	// 0x1000). TPIDR_EL2 (het sched-blok) staat nog — de lus meldt zich
	// daar als geparkeerd en wacht op een mailbox-dispatch van HOP.
	MOVD	208(RSP), R2
	ADD	$0x1000, R2, R2
	JMP	(R2)

#ifdef VHE
fiq:
	// Apple's fast IPI (m1n1 smp.c, Linux irq-apple-aic.c): pending in
	// IPI_SR_EL1 bit 0, wissen door 1 terug te schrijven. Geen IPI pending
	// = een FIQ die we niet kennen → fault-rapport (x2 = 10 staat nog).
	WORD	$0xd53df120	// mrs x0, s3_5_c15_c1_1 (IPI_SR_EL1)
	CBZ	R0, fault
	WORD	$0xd51df120	// msr s3_5_c15_c1_1, x0 (ack)
	// De doorbell als interrupt: draait de app (dit is idx 10, dus ja) en
	// heeft hij zich aangemeld (layout.CtrlDoorIRQ in zijn control-page),
	// dan een virtuele FIQ pending zetten (HCR_EL2.VF; FMO staat al). EL1
	// neemt hem zodra F daar open staat, tamago's vector maakt er zijn
	// interrupt-signaal van, en de ISR van de app wekt zijn RX-pomp — ook
	// als de core druk is met GC en de idle-governor dus niet aan de beurt
	// komt (gemeten 04-09: dan 1ms per call op de poll-timer). Zonder vlag:
	// alleen de ack, precies als voorheen. Eigen ctx via SchedCurrent (zie
	// yield:), control-page via CtxCtrlPA; MMU uit, dus dit leest DRAM — de
	// app publiceert de vlag met een clean.
	MOVD	32(RSP), R1	// layout.SchedCurrent
	MOVD	208(RSP), R2	// layout.SchedS2PA
	ADD	R1<<16, R2, R2
	ADD	$0x6000, R2, R2	// eigen ctx (layout.CtxOff)
	MOVD	8(R2), R3	// layout.CtxCtrlPA
	CBZ	R3, fiqdone
	MOVD	0x118(R3), R3	// layout.CtrlDoorIRQ
	CBZ	R3, fiqdone
	WORD	$0xd53c1100	// mrs x0, hcr_el2
	ORR	$0x40, R0, R0	// VF (bit 6): virtuele FIQ pending voor EL1
	WORD	$0xd51c1100	// msr hcr_el2, x0
fiqdone:
	LDP	(RSP), (R0, R1)
	LDP	16(RSP), (R2, R3)
	ISB	$15
	ERET

// doorack: HVC #5 — de app heeft zijn doorbell-interrupt afgehandeld: de
// virtuele FIQ weer weg (level: anders vuurt hij opnieuw zodra EL1 F opent).
doorack:
	WORD	$0xd53c1100	// mrs x0, hcr_el2
	BIC	$0x40, R0, R0	// VF eraf
	WORD	$0xd51c1100	// msr hcr_el2, x0
	LDP	(RSP), (R0, R1)
	LDP	16(RSP), (R2, R3)
	ISB	$15
	ERET
#endif

// wake: HVC #4 — de app wekt een sibling-core van zichzelf (x0 = diens
// MPIDR-affiniteit, zoals de app hem op EL1 ziet: de trampoline zet VMPIDR
// gelijk aan MPIDR). Dit is de reschedule-IPI van Linux in HopOS-vorm: de
// Go-runtime (semawakeup, preemptM, een timer op andermans heap) roept
// goos.Wake, cpu/smp maakt er deze HVC van, en hier gebeurt het wekken:
//   - zoek in de ctx-blokken van dezelfde app (zelfde CtxCtrlPA — de
//     eenheid, niet iets wat de app zelf kan opgeven) de core met dit
//     affiniteitswoord (CtxKickTarget, door zijn switcher bij zijn yield
//     geschreven);
//   - zet diens CtxWake op "nu": de rotatie op die core hervat hem bij zijn
//     eerstvolgende ronde (QEMU: de WFE-lus spint, dus meteen; HOP's wekker
//     kickt hem hoe dan ook binnen een ms);
//   - op Apple (VHE) ook meteen de fast IPI, m1n1's recept: core | cluster<<16
//     uit aff0 | aff1<<8 — de WFI in sleep: keert dan direct terug.
// Alleen x0..x3 zijn hier klad (scratch); x4/x5 gaan even in het GPR-vak van
// de eigen ctx — de bewoner draait, dat vak is dood tot zijn volgende yield.
// Geen match (de core draait al, of hoort niet bij deze app): niets doen.
wake:
	MOVD	32(RSP), R1	// layout.SchedCurrent (zie yield: niet de VMID)
	MOVD	208(RSP), R2	// layout.SchedS2PA
	ADD	R1<<16, R2, R2
	ADD	$0x6000, R2, R2	// x2 = eigen ctx (layout.CtxOff)
	STP	(R4, R5), 56(R2)	// x4/x5 kladden
	MOVD	(RSP), R0	// x0 = doel (originele x0)
	MOVD	8(R2), R1	// x1 = eigen CtxCtrlPA = de eenheid
	// De eenheid begint bij de primaire (layout.CtxUnitSlot, door HOP
	// gezet), niet bij de eigen slot: een secundaire moet de primaire
	// kunnen wekken.
	MOVD	496(R2), R4	// layout.CtxUnitSlot
	MOVD	208(RSP), R5	// layout.SchedS2PA
	ADD	R4<<16, R5, R4
	ADD	$0x6000, R4, R4	// x4 = ctx_k, vanaf de primaire
	MOVD	$8, R3		// hoogstens 8 cores per app
wakescan:
	MOVD	8(R4), R5
	CMP	R1, R5		// zelfde control-page?
	BNE	wakenext
	MOVD	480(R4), R5	// layout.CtxKickTarget
	CMP	R0, R5		// die core?
	BNE	wakenext
	MOVD	ZR, 464(R4)	// layout.CtxWake = nu
	MOVD	488(R4), R5	// layout.CtxWakes: geteld, voor de meetlat
	ADD	$1, R5, R5
	MOVD	R5, 488(R4)
	DSB	$15
#ifdef VHE
	AND	$0xFF, R0, R5	// core = aff0
	LSR	$8, R0, R4
	AND	$0xFF, R4, R4	// cluster = aff1
	ORR	R4<<16, R5, R5
	WORD	$0xd51df025	// msr s3_5_c15_c0_1, x5 (IPI_RR_GLOBAL_EL1)
#endif
	B	wakedone
wakenext:
	ADD	$0x10000, R4, R4	// volgende slot (layout.CageStride)
	SUBS	$1, R3, R3
	BNE	wakescan
wakedone:
	LDP	56(R2), (R4, R5)
	LDP	(RSP), (R0, R1)
	LDP	16(RSP), (R2, R3)
	ISB	$15
	ERET

// el2entryEnd markeert het einde van el2entry: de blob [el2entry, el2entryEnd)
// wordt door kern/stage2 naar de plan-regio gekopieerd (docs/kern-flip.md) —
// de code hierboven is volledig SP/TPIDR-relatief en draait daar ongewijzigd.
// Direct ná el2entry in dít bestand houden; de install-guard toetst de maat.
TEXT el2entryEnd(SB),NOSPLIT|NOFRAME,$0
	RET

// entryPC/entryEndPC geven de IMAGE-adressen van de blob (HOP-image is
// identity-geladen: symbooladres = fysiek adres). De publieke accessor
// (EntryPC, pc.go) geeft de plan-kopie zodra die geïnstalleerd is.
TEXT ·entryPC(SB),NOSPLIT,$0-8
	MOVD	$el2entry(SB), R0
	MOVD	R0, ret+0(FP)
	RET

TEXT ·entryEndPC(SB),NOSPLIT,$0-8
	MOVD	$el2entryEnd(SB), R0
	MOVD	R0, ret+0(FP)
	RET
