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
// core (layout.Sched*: Stage2PA en CtrlPA, door InitVectors neergelegd) —
// geen #defines, board-neutraal onder elk PA-plan. De layout.Sched*/Ctx*-
// offsets staan hier als literals; layout.go benoemt die koppeling.
//
// EL2/EL1-sysregs via WORD-encodings (Go-assembler kent ze niet bij naam):
// MRS = 0xd5300000 | (op0-2)<<19 | op1<<16 | CRn<<12 | CRm<<8 | op2<<5 | Rt,
// MSR = idem met 0xd5100000 — dezelfde vorm als el2.s/smp.s.

//go:build tamago && arm64

#include "textflag.h"

TEXT el2entry(SB),NOSPLIT|NOFRAME,$0
	// x0/x1 óók naar de scratch; daarna zijn x0..x3 werkregisters. De
	// scratch-indeling (SP-relatief): +0 x0, +8 x1, +16 x2, +24 x3.
	STP	(R0, R1), (RSP)

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
	AND	$0xFFFF, R0, R3
	CBZ	R3, exited

yield:
	// Idle-yield (HVC #1): bewoner = VMID; zijn contextblok = Stage2PA + slot<<16 + CtxOff.
	// Stage2PA komt uit het sched-blok (SP = scratch = blok+16, dus
	// veld-offset − 16: SchedS2PA(224) → 208).
	WORD	$0xd53c2100	// mrs x0, vttbr_el2
	LSR	$48, R0, R0	// x0 = slot
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
	LDP	16(RSP), (R2, R3)	// originele x2/x3
	STP	(R2, R3), 40(R1)

	// Hervat-PC = ELR_EL2 (bij een HVC wijst die al ná de hvc-instructie —
	// geen +4 zoals bij een getrapte WFE). SPSR ernaast.
	WORD	$0xd53c4022	// mrs x2, elr_el2
	WORD	$0xd53c4003	// mrs x3, spsr_el2
	STP	(R2, R3), 288(R1)	// layout.CtxELR
	WORD	$0xd5384102	// mrs x2, sp_el0
	WORD	$0xd53c4103	// mrs x3, sp_el1
	STP	(R2, R3), 272(R1)	// layout.CtxSPEL0

	// EL1-sysregs (volgorde = layout.CtxSysregs, 304..448): het volledige
	// vertaal/context-regime dat de volgende bewoner NIET mag erven.
	WORD	$0xd5381002	// mrs x2, sctlr_el1
	WORD	$0xd5382043	// mrs x3, tcr_el1
	STP	(R2, R3), 304(R1)
	WORD	$0xd5382002	// mrs x2, ttbr0_el1
	WORD	$0xd5382023	// mrs x3, ttbr1_el1
	STP	(R2, R3), 320(R1)
	WORD	$0xd538a202	// mrs x2, mair_el1
	WORD	$0xd538a303	// mrs x3, amair_el1
	STP	(R2, R3), 336(R1)
	WORD	$0xd538c002	// mrs x2, vbar_el1
	WORD	$0xd53bd043	// mrs x3, tpidr_el0
	STP	(R2, R3), 352(R1)
	WORD	$0xd53bd062	// mrs x2, tpidrro_el0
	WORD	$0xd538d083	// mrs x3, tpidr_el1
	STP	(R2, R3), 368(R1)
	WORD	$0xd538d022	// mrs x2, contextidr_el1
	WORD	$0xd5381043	// mrs x3, cpacr_el1
	STP	(R2, R3), 384(R1)
	WORD	$0xd538e102	// mrs x2, cntkctl_el1
	WORD	$0xd53a0003	// mrs x3, csselr_el1 (op0=3,op1=2,C0,C0,0)
	STP	(R2, R3), 400(R1)
	WORD	$0xd5387402	// mrs x2, par_el1
	WORD	$0xd5384023	// mrs x3, elr_el1
	STP	(R2, R3), 416(R1)
	WORD	$0xd5384002	// mrs x2, spsr_el1
	WORD	$0xd5385203	// mrs x3, esr_el1
	STP	(R2, R3), 432(R1)
	WORD	$0xd5386002	// mrs x2, far_el1
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
	WORD	$0xd53c2100	// mrs x0, vttbr_el2
	LSR	$48, R0, R0
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
	WORD	$0xd53c2100	// mrs x0, vttbr_el2
	LSR	$48, R0, R0	// x0 = slot
	MOVD	216(RSP), R1	// layout.SchedCtrlPA
	ADD	R0<<12, R1, R1	// + slot*CtrlStride = eigen ctrl-page
	ADD	$1, R2, R2
	MOVD	R2, 0x68(R1)	// layout.CtrlFaultVec = vec+1
	WORD	$0xd53c5202	// mrs x2, esr_el2
	MOVD	R2, 0x58(R1)	// layout.CtrlFaultESR
	WORD	$0xd53c6002	// mrs x2, far_el2
	MOVD	R2, 0x60(R1)	// layout.CtrlFaultFAR
	MOVD	208(RSP), R1	// staat → dead
	ADD	R0<<16, R1, R1
	ADD	$0x6000, R1, R1
	MOVD	$4, R2
	MOVD	R2, (R1)
	DSB	$15
	B	rotate

sleep:
	// De idle-slaap van de core: één WFE op EL2. De event stream van de
	// zojuist geyielde app (CNTKCTL_EL1, door de governor altijd aan) loopt
	// door, dus dit wekt ~elke ms; een al-gearriveerd event valt er meteen
	// doorheen — geen verloren wekker, en het vermogen blijft dat van een
	// dedicated idle core (er wordt exact één keer per tik geroteerd).
	WFE

rotate:
	// Round-robin over de bewonerslijst van deze core (SP-relatief; SP =
	// sched-blok+16): vanaf cursor+1 de eerste bewoner met staat
	// boot-pending (1) of saved (2). x0..x30 zijn hier vrij — de huidige
	// bewoner is gesaved of dood.
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
	CMP	$1, R9		// boot-pending?
	BEQ	boot
	CMP	$2, R9		// saved?
	BEQ	resume
skip:
	SUBS	$1, R6, R6
	BNE	next
	B	park		// iedereen dood/leeg → parkeren

boot:
	// Cold boot van een boot-pending bewoner: exact het mailbox-dispatchpad
	// (x0 = ctrl-page, spring de trampoline in), maar dan EL2→EL2 vanaf de
	// rotatie. Staat → running vóór de sprong (HOP's bootPending-poll leest 'm).
	MOVD	R4, 64(RSP)	// cursor bijwerken
	MOVD	$3, R9
	MOVD	R9, (R1)
	DSB	$15
	MOVD	8(R1), R0	// layout.CtxBootCtx → x0 (zoals PSCI 'm zou geven)
	MOVD	16(R1), R2	// layout.CtxBootPC (s2tramp)
	JMP	(R2)

resume:
	MOVD	R4, 64(RSP)	// cursor bijwerken
	MOVD	$3, R9		// staat → running
	MOVD	R9, (R1)
	DSB	$15

	// VTTBR omzetten naar dít slot: L1 = Stage2PA + slot<<16 (l1Off = 0),
	// VMID = slot. GEEN TLBI: entries zijn VMID-getagd, de vertalingen van
	// beide bewoners bestaan naast elkaar — dát maakt de wissel goedkoop.
	MOVD	208(RSP), R2
	ADD	R8<<16, R2, R2
	ORR	R8<<48, R2, R2
	WORD	$0xd51c2102	// msr vttbr_el2, x2

	// EL1-sysregs terug (spiegel van de save; volgorde vrij — de ERET is
	// het synchronisatiepunt).
	LDP	304(R1), (R2, R3)
	WORD	$0xd5181002	// msr sctlr_el1, x2
	WORD	$0xd5182043	// msr tcr_el1, x3
	LDP	320(R1), (R2, R3)
	WORD	$0xd5182002	// msr ttbr0_el1, x2
	WORD	$0xd5182023	// msr ttbr1_el1, x3
	LDP	336(R1), (R2, R3)
	WORD	$0xd518a202	// msr mair_el1, x2
	WORD	$0xd518a303	// msr amair_el1, x3
	LDP	352(R1), (R2, R3)
	WORD	$0xd518c002	// msr vbar_el1, x2
	WORD	$0xd51bd043	// msr tpidr_el0, x3
	LDP	368(R1), (R2, R3)
	WORD	$0xd51bd062	// msr tpidrro_el0, x2
	WORD	$0xd518d083	// msr tpidr_el1, x3
	LDP	384(R1), (R2, R3)
	WORD	$0xd518d022	// msr contextidr_el1, x2
	WORD	$0xd5181043	// msr cpacr_el1, x3
	LDP	400(R1), (R2, R3)
	WORD	$0xd518e102	// msr cntkctl_el1, x2
	WORD	$0xd51a0003	// msr csselr_el1, x3 (op0=3,op1=2,C0,C0,0)
	LDP	416(R1), (R2, R3)
	WORD	$0xd5187402	// msr par_el1, x2
	WORD	$0xd5184023	// msr elr_el1, x3
	LDP	432(R1), (R2, R3)
	WORD	$0xd5184002	// msr spsr_el1, x2
	WORD	$0xd5185203	// msr esr_el1, x3
	MOVD	448(R1), R2
	WORD	$0xd5186002	// msr far_el1, x2
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
	// Geen bewoner meer te draaien: core naar de parkeerlus (Stage2PA +
	// 0x1000). TPIDR_EL2 (het sched-blok) staat nog — de lus meldt zich
	// daar als geparkeerd en wacht op een mailbox-dispatch van HOP.
	MOVD	208(RSP), R2
	ADD	$0x1000, R2, R2
	JMP	(R2)

// EntryPC geeft het fysieke adres van el2entry (HOP-image is identity-
// geladen: symbooladres = fysiek adres) — het sprongdoel van de door
// kern/stage2 gegenereerde vector-thunks.
TEXT ·EntryPC(SB),NOSPLIT,$0-8
	MOVD	$el2entry(SB), R0
	MOVD	R0, ret+0(FP)
	RET
