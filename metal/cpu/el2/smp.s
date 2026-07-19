// SMP-trampoline voor fase 5: één app over meerdere cores met een gedeelde
// heap. Waar el2.s een láng-image-core een verse app-runtime laat booten (rt0 →
// eigen stage-1), brengt dit pad een EXTRA core in een AL DRAAIENDE app-runtime:
// de core deelt de stage-2-tabel van de primaire (dus dezelfde partitie = één
// gedeelde heap) en springt rechtstreeks in runtime.mstart als nieuwe M.
//
// Twee helften, elk in de goede adresruimte omdat zowel de HOP-kern als elk
// app-image dit pakket linkt:
//
//   - smpEL2Tramp draait op de nieuwe core op EL2 (MMU uit), uit HOP's image
//     (identity → symbooladres = fysiek). PSCI CPU_ON springt hierheen met
//     x0 = fysiek adres van de control-page van de primaire. Het leest de door
//     goos.Task neergelegde M-context (en VMID/vectoren) van die page,
//     activeert de gedeelde stage-2, en ERET't naar de EL1-stub.
//   - smpEL1Stub draait op EL1 onder de gedeelde stage-2, uit het APP-image
//     (IPA). Het zet de eigen stage-1 MMU aan met het GEËRFDE vertaalregime
//     van de primaire (cacheable inner-shareable → coherent met de primaire
//     heap), en springt in mstart. Vóór MMU-aan leest het niets uit geheugen —
//     alle context komt via registers x0..x7 door de ERET heen.
//
// EL2-sysregs via WORD-encodings (Go-assembler kent ze niet bij naam), exact
// dezelfde encodings als el2.s.

//go:build tamago && arm64

#include "textflag.h"

// smpEL2Tramp: entry voor een secundaire SMP-core (EL2, MMU uit). x0 = het
// FYSIEKE adres van de control-page van de primaire — de app geeft die PA mee
// als ctx-argument van PSCI CPU_ON, dus de trampoline krijgt 'm rechtstreeks
// in x0 (de app kent verder alleen IPA's). Data-gedreven zoals el2.s: geen
// #defines, board-neutraal onder elk PA-plan.
TEXT smpEL2Tramp(SB),NOSPLIT|NOFRAME,$0
	MOVD	R0, R1		// R1 = control-page van de primaire (fysiek)

	// M-context die goos.Task neerlegde (IPA-waarden; geldig zodra de gedeelde
	// stage-2/stage-1 straks actief is). In callee-saved regs tot na de ERET.
	MOVD	0x88(R1), R10	// layout.CtrlSMPSp    → stacktop
	MOVD	0x90(R1), R11	// layout.CtrlSMPMp    → *m
	MOVD	0x98(R1), R12	// layout.CtrlSMPG0    → *g (g0)
	MOVD	0xA0(R1), R13	// layout.CtrlSMPFn    → mstart
	MOVD	0xB0(R1), R14	// layout.CtrlSMPTtbr0 → stage-1 L1 (IPA)
	MOVD	0xA8(R1), R15	// layout.CtrlSMPStub  → EL1-stub (IPA, ELR-doel)
	MOVD	0x38(R1), R2	// layout.CtrlS2Table  → gedeelde stage-2 L1 (fysiek)
	MOVD	0xB8(R1), R6	// layout.CtrlSlot     → VMID (primair slot)

	// TPIDR_EL2 = fysieke parkeer-mailbox van déze secundaire core. De
	// primaire ctrl-page is gedeeld, dus de secundaire mailbox komt via een
	// eigen veld dat HOP vlak vóór de dispatch zette (CtrlSMPMbox).
	MOVD	0xD0(R1), R7	// layout.CtrlSMPMbox
	WORD	$0xd51cd047	// msr tpidr_el2, x7

	// SP_EL2 = de sched-scratch van deze core (mailbox + layout.SchedScratch):
	// ook een SMP-/node-core deelt de vector-thunks, en die parkeren daar
	// registers vóór welke exception dan ook. Géén TWE hier: een secundaire
	// SMP-core deelt zijn core met niemand, dus zijn WFE mag lokaal slapen.
	ADD	$16, R7, R8
	MOVD	R8, RSP

	// EL2-vectoren (zelfde als de app-cores: een stage-2-fault parkeert/CPU_OFF't).
	MOVD	0x50(R1), R3	// layout.CtrlVecPA
	WORD	$0xd51cc003	// msr vbar_el2, x3

	// Kooi-profiel gekozen op CtrlS2Table (R2) — dit is de gedeelde trampoline
	// voor ZOWEL een app-SMP-core ALS een node-runtime-core (HOP zelf):
	//   R2 == 0  → node-core: GÉÉN stage-2-kooi, HCR zonder VM/TSC (de node mag
	//              SMC/HVC — PSCI, Revoke). Spiegelt bootKernel's HCR van core 0.
	//   R2 != 0  → app-core: stage-2 aan (kooi), SMC getrapt (het bestaande pad).
	// Zo bedient één trampoline beide profielen (Derek: "bijna hergebruiken = een
	// gedeelde functie"); node vult CtrlS2Table=0 + revoke-vectoren in CtrlVecPA.
	CMP	$0, R2
	BEQ	s2none

	// VTCR_EL2: 4KB-granule, 32-bit IPA, PS = min(PARange, 44-bit) —
	// identiek aan el2.s (zie dáár waarom: hoge partities + silicium-klem).
	WORD	$0xd5380705	// mrs x5, id_aa64mmfr0_el1
	AND	$0xF, R5
	CMP	$4, R5
	BLT	vtcrps2		// PARange < 44-bit: het silicium-maximum
	MOVD	$4, R5		// anders klemmen op 44-bit (16TB)
vtcrps2:
	MOVD	$0x80003560, R4	// VTCR zonder PS-veld
	ORR	R5<<16, R4, R4
	WORD	$0xd51c2144	// msr vtcr_el2, x4

	// VTTBR_EL2 = gedeelde L1 | VMID(primair)<<48 → deelt de partitie van de
	// primaire = gedeelde heap. Oude vertalingen vegen.
	LSL	$48, R6, R5
	ORR	R2, R5, R5
	WORD	$0xd51c2105	// msr vttbr_el2, x5
	WORD	$0xd50c87df	// tlbi vmalls12e1
	DSB	$15

	// HCR_EL2: RW(31) | TSC(19, trap SMC — zie el2.s: een app-SMC bestaat
	// niet, dus trap = ontsnappingspoging) | VM(0, stage-2 aan). Geen
	// IMO(4)/GIC: de hard-kill loopt via stage-2-intrekking (Revoke), niet via
	// een IRQ — een secundaire SMP-core deelt tabel én VMID met de primaire,
	// dus dezelfde TLBI velt ook hem.
	MOVD	$1<<31, R4
	ORR	$1<<19, R4, R4
	ORR	$1, R4, R4
	WORD	$0xd51c1104	// msr hcr_el2, x4
	B	s2done

s2none:
	// Node-core (geen kooi): HCR_EL2 = RW(31) alleen — VM=0 (stage-2 uit, IPA=PA)
	// en SMC niet getrapt. Identiek aan wat bootKernel voor core 0 zet.
	// VTTBR = 0: óók bij VM=0 tagt het silicium TLB-entries op VTTBR.VMID, en
	// broadcast-TLBI's (mmu48 MapHigh/UnmapHigh op de primaire) matchen op die
	// VMID — een random geërfde VMID zou deze core stale vertalingen laten
	// houden na een map-wijziging van de node-runtime. 0 = de reset-waarde
	// waar core 0 mee draait; daarna de eigen TLB schoon beginnen.
	MOVD	$0, R4
	WORD	$0xd51c2104	// msr vttbr_el2, x4
	WORD	$0xd50c87df	// tlbi vmalls12e1
	DSB	$15
	MOVD	$1<<31, R4
	WORD	$0xd51c1104	// msr hcr_el2, x4

s2done:
	// Timers vrij voor EL1.
	WORD	$0xd53ce104	// mrs x4, cnthctl_el2
	ORR	$0b11, R4, R4
	WORD	$0xd51ce104	// msr cnthctl_el2, x4
	MOVD	$0, R4
	WORD	$0xd51ce064	// msr cntvoff_el2, x4

	// EL1-staat NIET erven (zelfde silicium-les als el2.s): een warme CPU_ON
	// erft de EL1-staat van de vorige huurder — met MMU aan zou zelfs de
	// fetch van de EL1-stub door stale tabellen vertalen. SCTLR_EL1 schoon
	// (de stub zet 'm daarna zelf op met de gedeelde tabel) en CPTR_EL2
	// zonder FP-traps (mstart begint met FP).
	MOVD	$0x30d00800, R4
	MSR	R4, SCTLR_EL1
	MOVD	$0x33FF, R4
	WORD	$0xd51c1144	// msr cptr_el2, x4

	// I-cache leeg vóór de drop — zelfde warme-herdispatch-hygiëne als
	// el2.s (zie dáár): een secundaire core kan als eerdere huurder stale
	// instructies voor deze canonieke adressen vasthouden.
	WORD	$0xd508751f	// ic iallu
	DSB	$15
	ISB	$15

	// SPSR_EL2: EL1h, DAIF gemaskeerd.
	MOVD	$0, R4
	ORR	$0b1111<<6, R4
	ORR	$0b0101<<0, R4
	WORD	$0xd51c4004	// msr spsr_el2, x4

	// Het geërfde EL1-vertaalregime voor de stub: de dispatchende primaire —
	// app-runtime (goos.Task) óf node (ConfigureNode) — las zijn ÁCTIEVE
	// MAIR/TCR/VBAR_EL1 en legde ze op de control-page; de stub zet ze blind.
	// Eén mechanisme voor beide profielen, geen hardcoded kopieën: de node-
	// primaire kan mmu48's 48-bit-wereld draaien (Altra: UART/watchdog op
	// 16TB) en tamago-defaults zouden die onvertaalbaar laten. R1 is nog de
	// control-page; R5/R6/R7 zijn na hun scratch/VMID/TPIDR-gebruik vrij.
	MOVD	0xF8(R1), R5	// layout.CtrlSMPMair → x5
	MOVD	0xD8(R1), R6	// layout.CtrlSMPTcr  → x6
	MOVD	0x78(R1), R7	// layout.CtrlSMPVbar → x7

	// ELR_EL2 = EL1-stub (IPA). De context voor de stub in x0..x7 zetten; die
	// overleven de ERET (die verandert alleen PC/PSTATE).
	WORD	$0xd51c402f	// msr elr_el2, x15
	MOVD	R10, R0		// x0 = sp
	MOVD	R11, R1		// x1 = mp
	MOVD	R12, R2		// x2 = g0
	MOVD	R13, R3		// x3 = fn (mstart)
	MOVD	R14, R4		// x4 = ttbr0
	ISB	$15
	ERET

// smpEL1Stub: draait op EL1 (app: onder de gedeelde stage-2; node: zonder
// kooi). Zet de eigen stage-1 MMU aan met het GEËRFDE vertaalregime van de
// primaire en springt in mstart. Leest niets uit geheugen tot de MMU aan is —
// alle context komt uit x0..x7.
//   x0 = sp, x1 = mp, x2 = g0, x3 = fn (mstart), x4 = ttbr0 (stage-1 L1)
//   x5 = mair, x6 = tcr, x7 = vbar (de ÁCTIEVE EL1-registers van de primaire)
TEXT smpEL1Stub(SB),NOSPLIT|NOFRAME,$0
	// MAIR/TCR: het geërfde vertaalregime van de dispatchende primaire, door
	// de trampoline in x5/x6 aangeleverd (van de levende registers gelezen —
	// app: tamago's InitMMU-waarden; node: mmu48's 48-bit-wereld). Blind
	// zetten, geen kopieën van constanten: de primaire ís de bron van waarheid.
	MSR	R5, MAIR_EL1
	MSR	R6, TCR_EL1

	// TTBR0_EL1 = gedeelde stage-1 L1 (IPA). Onder de al-actieve stage-2 wordt
	// dit IPA-adres naar de partitie van de primaire vertaald → dezelfde tabellen
	// = dezelfde VA→PA = gedeelde heap.
	MSR	R4, TTBR0_EL1
	ISB	$15

	// MMU + caches aan (SCTLR_EL1), WXN uit — spiegelt tamago's set_ttbr0_el1.
	// Vanaf hier zijn geheugentoegangen cacheable inner-shareable → coherent
	// met de draaiende primaire runtime.
	MRS	SCTLR_EL1, R5
	BIC	$1<<19, R5	// clear WXN
	ORR	$1<<12, R5	// I-cache
	ORR	$1<<2, R5	// D-cache
	ORR	$1<<0, R5	// MMU
	MSR	R5, SCTLR_EL1
	ISB	$15

	// Per-core EL1-init die de primaire in arm64.Init doet maar deze secundaire
	// core oversloeg (hij ging tramp→mstart, niet via rt0/hwinit):
	//   - VBAR_EL1: geërfd (x7) — de exceptievectoren die de primaire bouwde
	//     (arm64.initVectorTable op goos.RamStart; gedeelde map → zelfde
	//     tabel). Expliciet meegegeven, niet afgeleid uit ttbr0: die afleiding
	//     (ttbr0−0x4000) was een toevalligheid van tamago's indeling en klopt
	//     al niet meer zodra de primaire mmu48's L0 draait. Zonder geldige
	//     VBAR crasht élke EL1-exceptie naar 0x200.
	//   - FP/SIMD aan (CPACR_EL1.FPEN): anders trapt de eerste float-instructie.
	MSR	R7, VBAR_EL1
	MRS	CPACR_EL1, R5
	ORR	$(3<<20), R5
	MSR	R5, CPACR_EL1

	// ARM event-stream aan (CNTKCTL_EL1 = EVNTEN | EVNTI=15), net als metal/cpu/idle
	// op de primaire core doet. Zo wekt de WFE-idle-governor deze core ~elke ms
	// zonder interrupt — laag vermogen i.p.v. busy-spin, en geen botsing met de
	// EL2-kill-route (die enige IRQ-route zou een WFI juist fataal maken).
	MOVD	$0xF4, R5
	WORD	$0xd518e105	// msr cntkctl_el1, x5
	ISB	$15

	// De M draaien: g = g0 (g0.m is al door allocm gezet), SP = stacktop, en
	// door naar mstart. Hierna gebruikt deze code geen stack meer, dus RSP
	// herzetten is veilig.
	MOVD	R2, g
	MOVD	R0, RSP
	JMP	(R3)

// S2SMPTrampPC geeft het adres van de EL2 SMP-trampoline. In de HOP-image
// (identity) is dat het fysieke CPU_ON-entrypoint dat HOP op de control-page
// publiceert; in een app-image de IPA (ongebruikt daar).
TEXT ·S2SMPTrampPC(SB),NOSPLIT,$0-8
	MOVD	$smpEL2Tramp(SB), R0
	MOVD	R0, ret+0(FP)
	RET

// SMPStubPC geeft het adres van de EL1-stub. In een app-image is dat de IPA die
// goos.Task als ELR-doel op de control-page zet (de app-copy draait onder de
// gedeelde stage-2); in de HOP-image ongebruikt.
TEXT ·SMPStubPC(SB),NOSPLIT,$0-8
	MOVD	$smpEL1Stub(SB), R0
	MOVD	R0, ret+0(FP)
	RET
