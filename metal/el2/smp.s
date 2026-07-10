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
//     (IPA). Het zet de eigen stage-1 MMU aan met de GEDEELDE tabel van de
//     primaire (cacheable inner-shareable → coherent met de primaire heap),
//     en springt in mstart. Vóór MMU-aan leest het niets uit geheugen — alle
//     context komt via registers x0..x4 door de ERET heen.
//
// EL2-sysregs via WORD-encodings (Go-assembler kent ze niet bij naam), exact
// dezelfde encodings als el2.s.

//go:build tamago && arm64

#include "textflag.h"

// smpEL2Tramp: entry voor een secundaire SMP-core (EL2, MMU uit). x0 = het
// FYSIEKE adres van de control-page van de primaire (ctx van PSCI CPU_ON —
// de app las 'm van zijn eigen page, layout.CtrlSelfPA, door HOP bij Start
// gezet: de app kent alleen IPA's). Data-gedreven zoals el2.s: geen #defines,
// board-neutraal onder elk PA-plan.
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

	// EL2-vectoren (zelfde als de app-cores: een stage-2-fault parkeert/CPU_OFF't).
	MOVD	0x50(R1), R3	// layout.CtrlVecPA
	WORD	$0xd51cc003	// msr vbar_el2, x3

	// VTCR_EL2: 4KB-granule, 32-bit IPA, PS=40-bit (identiek aan el2.s).
	MOVD	$0x80023560, R4
	WORD	$0xd51c2144	// msr vtcr_el2, x4

	// VTTBR_EL2 = gedeelde L1 | VMID(primair)<<48 → deelt de partitie van de
	// primaire = gedeelde heap. Oude vertalingen vegen.
	LSL	$48, R6, R5
	ORR	R2, R5, R5
	WORD	$0xd51c2105	// msr vttbr_el2, x5
	WORD	$0xd50c87df	// tlbi vmalls12e1
	DSB	$15

	// HCR_EL2: RW(31) | VM(0, stage-2 aan). Geen IMO(4)/GIC: de hard-kill loopt
	// via stage-2-intrekking (Revoke), niet via een IRQ — een secundaire SMP-core
	// deelt tabel én VMID met de primaire, dus dezelfde TLBI velt ook hem.
	MOVD	$1<<31, R4
	ORR	$1, R4, R4
	WORD	$0xd51c1104	// msr hcr_el2, x4

	// Timers vrij voor EL1.
	WORD	$0xd53ce104	// mrs x4, cnthctl_el2
	ORR	$0b11, R4, R4
	WORD	$0xd51ce104	// msr cnthctl_el2, x4
	MOVD	$0, R4
	WORD	$0xd51ce064	// msr cntvoff_el2, x4

	// SPSR_EL2: EL1h, DAIF gemaskeerd.
	MOVD	$0, R4
	ORR	$0b1111<<6, R4
	ORR	$0b0101<<0, R4
	WORD	$0xd51c4004	// msr spsr_el2, x4

	// ELR_EL2 = EL1-stub (IPA). De context voor de stub in x0..x4 zetten; die
	// overleven de ERET (die verandert alleen PC/PSTATE).
	WORD	$0xd51c402f	// msr elr_el2, x15
	MOVD	R10, R0		// x0 = sp
	MOVD	R11, R1		// x1 = mp
	MOVD	R12, R2		// x2 = g0
	MOVD	R13, R3		// x3 = fn (mstart)
	MOVD	R14, R4		// x4 = ttbr0
	ISB	$15
	ERET

// smpEL1Stub: draait op EL1 onder de gedeelde stage-2 (stage-1 nog uit). Zet de
// eigen stage-1 MMU aan met de GEDEELDE tabel en springt in mstart. Leest niets
// uit geheugen tot de MMU aan is — context komt uit x0..x4.
//   x0 = sp, x1 = mp, x2 = g0, x3 = fn (mstart), x4 = ttbr0 (stage-1 L1, IPA)
TEXT smpEL1Stub(SB),NOSPLIT|NOFRAME,$0
	// MAIR_EL1: attr0 = Device (0x00), attr1 = Normal WB (0xFF) — exact zoals
	// tamago's InitMMU. 0xFF00 past in één MOVZ (geen literal-pool → geen lees).
	MOVD	$0xFF00, R5
	MSR	R5, MAIR_EL1

	// TCR_EL1 = 0x2_0000_3519 (IPS=40b, 4KB-granule, inner-shareable, WB
	// inner/outer, T0SZ=25) — identiek aan tamago's tcr-constante. Opgebouwd uit
	// een MOVZ (0x3519) + ORR met een enkel-bit-bitmask (bit 33) → geen lees.
	MOVD	$0x3519, R5
	ORR	$0x200000000, R5, R5
	MSR	R5, TCR_EL1

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
	//   - VBAR_EL1 = RamStart: de EL1-exceptievectoren die de primaire dáár
	//     bouwde (arm64.initVectorTable legt ze op goos.RamStart; gedeelde
	//     partitie → zelfde tabel). Zonder dit crasht élke EL1-exceptie naar 0x200.
	//   - FP/SIMD aan (CPACR_EL1.FPEN): anders trapt de eerste float-instructie.
	// R4 = ttbr0 = RamStart+0x4000, dus RamStart = R4 - 0x4000.
	SUB	$0x4000, R4, R5
	MSR	R5, VBAR_EL1
	MRS	CPACR_EL1, R5
	ORR	$(3<<20), R5
	MSR	R5, CPACR_EL1

	// ARM event-stream aan (CNTKCTL_EL1 = EVNTEN | EVNTI=15), net als metal/idle
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
