// EL2-capabele CPU-init voor de Pi 4 — vervangt tamago's cpuinit (bouw met
// -tags linkcpuinit). Met TF-A als armstub (verplicht op dit board, zie
// docs/rpi4.md) levert de firmware ons op EL2 af. Dit is de Pi 5-verharde
// variant (board/rpi5/cpuinit.s, 2026-07-09) geport naar de Pi 4:
//
//   - vóór álles een levensteken naar de PL011 ("P" + boot-EL): op bare
//     metal zonder werkende boot is de UART het enige debugkanaal;
//   - volledige Linux-init_el2-pariteit (CPTR_EL2.TFP=0!, SCTLR_EL2, VTTBR,
//     VPIDR/VMPIDR, HSTR, MDCR, MDSCR, CPACR, expliciete SCTLR_EL1) — elk
//     van die registers was op de Pi 5 een gemeten muur of UNKNOWN-risico;
//   - noodvectoren op EL1 én EL2 → faultdump ("XE<esr>L<elr>F<far>R<lr>
//     S<sp>+stack" / "YE..."), geen stille hangs in het boot-venster;
//   - dcache-invalidatie over de RAM-declaratie + ic iallu (firmware-spoken);
//   - PC-dump 'p<pc>' = onweerlegbaar laadadres-bewijs (de Pi 5 bleek
//     kernel_address te NEGEREN en op 0x80000 te laden — vandaar dit plan:
//     load 0x80000, text 0x90000, geen kernel_address meer).
//
// A72-verschil t.o.v. de Pi 5 (A76): GEEN CPUECTLR-write hier — de A76-
// encoding (S3_0_C15_C1_4) is op de A72 een ánder register (A72: SMPEN =
// S3_1_C15_C2_1 bit 6) en TF-A's cortex_a72-reset-handler zet SMPEN al.
// Verkeerde encoding = UNDEF-risico; weglaten is reference-conform.
//
// EL2-systeemregisters via WORD-encodings (zelfde stijl als tamago's init.s).

//go:build linkcpuinit

#include "textflag.h"

#define BOOT_SCRATCH 0x7F000
#define DTB_PTR      0x7F008
#define UART_DR 0xFE201000
#define UART_FR 0xFE201018

TEXT cpuinit(SB),NOSPLIT|NOFRAME,$0
	MOVD	R0, R9		// x0 = DTB-pointer bij firmware-boot; bewaren vóór clobber

	// App-cores (fase P1) entreren hier op EL1, via de EL2-trampoline en
	// ónder stage-2: UART/scratch zijn daar niet gemapt — één MMIO-poke en de
	// EL2-vector velt de core. Dus vóór álles: EL1 → het schone app-pad (geen
	// MMIO, geen noodvectoren, geen dcinv). De primary komt altijd op EL2
	// binnen (TF-A bl31) en krijgt hieronder de volle diagnostiek.
	MRS	CurrentEL, R0
	LSR	$2, R0, R0
	AND	$0b11, R0, R0
	CMP	$1, R0
	BNE	primary
	B	·cpuinitEL1App(SB)

primary:
	// Levensteken 1: 'P' (Pi). Begrensd gepolld: een dode FIFO-vol-vlag
	// kost hooguit de poll, nooit de boot.
	MOVD	$UART_FR, R2
	MOVD	$100000, R4
wait1:
	SUBS	$1, R4
	BEQ	uartklaar	// FR.TXFF blijft vol → UART dood: overslaan
	MOVWU	(R2), R3
	TBNZ	$5, R3, wait1	// FR.TXFF: FIFO vol → poll
	MOVD	$UART_DR, R2
	MOVD	$0x50, R3	// 'P'
	MOVW	R3, (R2)

	// Levensteken 2: het boot-EL als cijfer ('1'/'2'/'3').
	MRS	CurrentEL, R0
	LSR	$2, R0, R0
	AND	$0b11, R0, R0
	ADD	$0x30, R0, R3	// '0' + EL
	MOVW	R3, (R2)
uartklaar:
	MRS	CurrentEL, R0	// (opnieuw: het UART-pad kan overgeslagen zijn)
	LSR	$2, R0, R0
	AND	$0b11, R0, R0

	CMP	$2, R0
	BEQ	el2
	CMP	$3, R0
	BEQ	el3
	// EL1-boot (onverwacht op de Pi): scratch blijft 0 ⇒ de main weigert.
	B	·cpuinitEL1(SB)

el2:
	// boot-EL naar de scratch (MMU uit; gewone DRAM-write).
	MOVD	$BOOT_SCRATCH, R1
	MOVD	R0, (R1)
	// DTB-pointer opslaan (metal/fdt → board.MemTotal). Alleen de primary komt
	// op el2; app-cores entreren cpuinit op EL1 via de trampoline.
	MOVD	$DTB_PTR, R1
	MOVD	R9, (R1)
	// HCR_EL2: RW(31)=1 — EL1 draait AArch64. Stage-2 (VM-bit) blijft uit;
	// fase P1 hangt hier de app-core-variant met VTTBR_EL2 + VM aan.
	MOVD	$1<<31, R0
	WORD	$0xd51c1100	// msr hcr_el2, x0

	// CNTHCTL_EL2: EL1PCTEN|EL1PCEN — timer/counter niet trappen voor EL1.
	WORD	$0xd53ce100	// mrs x0, cnthctl_el2
	ORR	$0b11, R0, R0
	WORD	$0xd51ce100	// msr cnthctl_el2, x0
	MOVD	$0, R0
	WORD	$0xd51ce060	// msr cntvoff_el2, x0

	// CPTR_EL2 = 0x33FF: FP/SIMD- en trace-traps naar EL2 UIT (TFP=0,
	// RES1-bits aan). QEMU reset TFP vriendelijk naar 0, echt silicium
	// (TF-A-stub) niet per se — en tamago's runtime-init begint met FP
	// (runtime·check): een achtergebleven TFP=1 hangt de boot geluidloos
	// op EL2 (Pi 5-boot-meting 2026-07-09: 'P2R' en dan niets).
	MOVD	$0x33FF, R0
	WORD	$0xd51c1140	// msr cptr_el2, x0

	// Linux init_el2-pariteit (el2_setup.h, afgekeken 2026-07-09): de
	// registers die Linux óók init en die bij EL2-entry garbage zijn.
	// SCTLR_EL2 = INIT_SCTLR_EL2_MMU_OFF (RES1-bits, rest uit).
	MOVD	$0x30C50830, R0
	WORD	$0xd51c1000	// msr sctlr_el2, x0
	ISB	$15
	// VTTBR_EL2 = 0: óók met stage-2 uit tagt VTTBR's VMID álle
	// EL1&0-TLB-entries — garbage-VMID is de TLB/PTW-errata-hoek.
	WORD	$0xd51c211f	// msr vttbr_el2, xzr
	// MDCR_EL2: HPMN = PMCR_EL0.N (Linux-recept), alle debug-traps uit.
	WORD	$0xd53b9c00	// mrs x0, pmcr_el0
	LSR	$11, R0, R0
	AND	$0x1F, R0, R0
	WORD	$0xd51c1120	// msr mdcr_el2, x0
	// MDSCR_EL1 = 0: geen achtergebleven debug-enable-bits (__cpu_setup).
	WORD	$0xd510025f	// msr mdscr_el1, xzr

	// U-Boot/Circle-recept (armv8_switch_to_el1_m) — op Pi 5-silicium
	// bewezen; bij EL2-entry zijn deze EL1-gezichten anders UNKNOWN:
	// VPIDR/VMPIDR: wat EL1 ziet bij midr/mpidr-reads.
	WORD	$0xd5380000	// mrs x0, midr_el1
	WORD	$0xd51c0000	// msr vpidr_el2, x0
	WORD	$0xd53800a0	// mrs x0, mpidr_el1
	WORD	$0xd51c00a0	// msr vmpidr_el2, x0
	// HSTR_EL2 = 0: geen aarch32-CP15-traps.
	WORD	$0xd51c117f	// msr hstr_el2, xzr
	// CPACR_EL1: FP/SIMD aan vóór de runtime (tamago's fp_enable komt pas
	// in hwinit0 — maar niets vóór die tijd mag al trappen).
	MOVD	$(3<<20), R0
	MSR	R0, CPACR_EL1
	// SCTLR_EL1 expliciet initialiseren (U-Boot-waarde 0x30d00800: RES1-
	// bits aan, al het andere — WXN, EE, I, C, A, M, nTWx — uit). Erven
	// is dodelijk: een achtergebleven WXN=1 maakt tamago's RW+X-mapping
	// executable-never zodra de MMU aangaat → recursieve abort, stil.
	MOVD	$0x30d00800, R0
	MSR	R0, SCTLR_EL1
	ISB	$15

	// VBAR_EL2 → ·faultdump2: mocht er tóch iets naar EL2 trappen, dan
	// een 'Y'-dump (ESR/ELR/FAR_EL2) i.p.v. een stille hang. Tabel op
	// 0x8B000, zelfde vrije gat als de EL1-tabel (0x8A000).
	MOVD	$0x8B000, R2
	MOVD	$·faultdump2(SB), R3
	MOVD	$16, R4
vecvul2:
	SUB	R2, R3, R6
	LSR	$2, R6, R6
	AND	$0x03FFFFFF, R6, R6
	MOVD	$0x14000000, R7
	ORR	R7, R6, R6
	MOVW	R6, (R2)
	ADD	$0x80, R2
	SUBS	$1, R4
	BNE	vecvul2
	MOVD	$0x8B000, R2
	WORD	$0xd51cc002	// msr vbar_el2, x2
	ISB	$15

	// SPSR_EL2: EL1h, DAIF gemaskeerd.
	MOVD	$0, R0
	ORR	$0b1111<<6, R0
	ORR	$0b0101<<0, R0
	WORD	$0xd51c4000	// msr spsr_el2, x0

	MOVD	$·cpuinitEL1(SB), R0
	WORD	$0xd51c4020	// msr elr_el2, x0
	ISB	$15
	ERET

el3:
	// EL3-pad (volledigheid; TF-A levert EL2 af).
	MOVD	$BOOT_SCRATCH, R1
	MOVD	R0, (R1)
	MOVD	$0, R0
	ORR	$1<<10, R0	// lagere levels AArch64
	ORR	$1<<5, R0	// reserved
	ORR	$1<<4, R0	// reserved
	ORR	$1<<0, R0	// Non-secure
	WORD	$0xd51e1100	// msr scr_el3, x0

	MOVD	$1<<31, R0
	WORD	$0xd51c1100	// msr hcr_el2, x0

	MOVD	$0, R0
	ORR	$0b1111<<6, R0
	ORR	$0b0101<<0, R0
	WORD	$0xd51e4000	// msr spsr_el3, x0

	MOVD	$·cpuinitEL1(SB), R0
	WORD	$0xd51e4020	// msr elr_el3, x0
	ISB	$15
	ERET

TEXT ·cpuinitEL1(SB),NOSPLIT|NOFRAME,$0
	// Noodvectoren voor het boot-venster (tot tamago zijn eigen vectors
	// zet): elke EL1-exceptie → ·faultdump (X + ESR/ELR/FAR/LR/stack op de
	// UART) i.p.v. een stille hang. Tabel op 0x8A000: 2KB-gealigneerd, in
	// het vrije gat tussen tamago's pagetables (ramStart+0x4000..0x9000)
	// en de ELF-inhoud (+0x10000) — post-MMU als executable RAM gemapt.
	MOVD	$0x8A000, R2
	MOVD	$·faultdump(SB), R3
	MOVD	$16, R4
vecvul:
	SUB	R2, R3, R6	// B-encoding: 0x14000000 | ((doel-entry)>>2 & imm26)
	LSR	$2, R6, R6
	AND	$0x03FFFFFF, R6, R6
	MOVD	$0x14000000, R7
	ORR	R7, R6, R6
	MOVW	R6, (R2)
	ADD	$0x80, R2
	SUBS	$1, R4
	BNE	vecvul
	MOVD	$0x8A000, R2
	WORD	$0xd518c002	// msr vbar_el1, x2
	ISB	$15

	// SCTLR_EL1: alignment-check en MMU uit (tamago zet ze zelf weer aan).
	MRS	SCTLR_EL1, R0
	BIC	$1<<1, R0
	BIC	$1<<0, R0
	MSR	R0, SCTLR_EL1
	ISB	$15

	// Stack aan het einde van de eigen RAM-declaratie.
	MOVD	runtime∕goos·RamStart(SB), R1
	MOVD	R1, RSP
	MOVD	runtime∕goos·RamSize(SB), R1
	MOVD	runtime∕goos·RamStackOffset(SB), R2
	ADD	R1, RSP
	SUB	R2, RSP

	// Cache-maintenance vóór de runtime — de fase-P-les die QEMU verhulde:
	// de firmware draaide mét caches; stale (schone) lines boven onze RAM
	// overleven de handoff. D-cache per 64B-lijn invalideren over de hele
	// RAM-declaratie (0x80000..0x8080000), en I-cache leeg.
	MOVD	$0x80000, R0
	MOVD	$0x8080000, R1
dcinv:
	WORD	$0xd5087620	// dc ivac, x0
	ADD	$64, R0
	CMP	R1, R0
	BLT	dcinv
	DSB	$15
	WORD	$0xd508751f	// ic iallu
	DSB	$15
	ISB	$15

	// Levensteken 3: 'R' — op weg naar de runtime (bisect-marker: P2..R =
	// EL-drop/stack, R..banner = rt0 + Hwinit0/MMU + runtime).
	MOVD	$UART_DR, R2
	MOVD	$0x52, R3	// 'R'
	MOVW	R3, (R2)

	// PC-dump 'p<pc>': het onweerlegbare laadadres-bewijs (ADR = pure
	// hardware-PC). Gelinkt op de werkelijkheid (text 0x90000) hoort hier
	// ~0xExxxx te staan; wijkt het af, dan laadt de firmware ons elders —
	// exact de val die op de Pi 5 drie dagen kostte.
	MOVD	$UART_DR, R8
	MOVD	$UART_FR, R9
	MOVD	$0x70, R3	// 'p'
	BL	·uputc(SB)
	WORD	$0x10000004	// adr x4, . (de échte PC)
	MOVD	$16, R5
	BL	·uhex(SB)

	B	_rt0_tamago_start(SB)

// faultdump: het boot-venster-exceptiehandler — print "XE<esr>L<elr>F<far>
// R<lr>S<sp> <16 stackwoorden>" (hex) op de UART en parkeert. UART is op
// dit punt bewezen ('P2R'). Registers vrij: we keren nooit terug.
TEXT ·faultdump(SB),NOSPLIT|NOFRAME,$0
	MOVD	$UART_DR, R8
	MOVD	$UART_FR, R9
	// LR (x30) van het fault-moment veiligstellen vóór de eerste BL:
	// bij een wilde sprong via BL wijst dit naar de aanroeper.
	MOVD	R30, R7
	MOVD	$0x58, R3	// 'X'
	BL	·uputc(SB)
	MOVD	$0x45, R3	// 'E'
	BL	·uputc(SB)
	WORD	$0xd5385204	// mrs x4, esr_el1
	MOVD	$8, R5
	BL	·uhex(SB)
	MOVD	$0x4C, R3	// 'L'
	BL	·uputc(SB)
	WORD	$0xd5384024	// mrs x4, elr_el1
	MOVD	$16, R5
	BL	·uhex(SB)
	MOVD	$0x46, R3	// 'F'
	BL	·uputc(SB)
	WORD	$0xd5386004	// mrs x4, far_el1
	MOVD	$16, R5
	BL	·uhex(SB)
	MOVD	$0x52, R3	// 'R': LR (x30) op het fault-moment
	BL	·uputc(SB)
	MOVD	R7, R4
	MOVD	$16, R5
	BL	·uhex(SB)
	// 'S': SP op het fault-moment + 16 stack-woorden — de Go-frames van
	// het laatste legitieme pad. Alleen dumpen als SP binnen de RAM-
	// declaratie ligt (anders recursieve aborts in de handler zelf).
	MOVD	$0x53, R3	// 'S'
	BL	·uputc(SB)
	MOVD	RSP, R7
	MOVD	R7, R4
	MOVD	$16, R5
	BL	·uhex(SB)
	MOVD	$0x80000, R6
	CMP	R6, R7
	BLO	fdklaar
	MOVD	$0x8080000, R6
	CMP	R6, R7
	BHS	fdklaar
	MOVD	$16, R12
fdstk:
	MOVD	$0x20, R3	// ' '
	BL	·uputc(SB)
	MOVD	(R7), R4
	ADD	$8, R7
	MOVD	$16, R5
	BL	·uhex(SB)
	SUBS	$1, R12
	BNE	fdstk
fdklaar:
faulthang:
	B	faulthang

// faultdump2: idem, maar voor EL2-traps — print "YE<esr>L<elr>F<far>"
// met de _EL2-registers.
TEXT ·faultdump2(SB),NOSPLIT|NOFRAME,$0
	MOVD	$UART_DR, R8
	MOVD	$UART_FR, R9
	MOVD	$0x59, R3	// 'Y'
	BL	·uputc(SB)
	MOVD	$0x45, R3	// 'E'
	BL	·uputc(SB)
	WORD	$0xd53c5204	// mrs x4, esr_el2
	MOVD	$8, R5
	BL	·uhex(SB)
	MOVD	$0x4C, R3	// 'L'
	BL	·uputc(SB)
	WORD	$0xd53c4024	// mrs x4, elr_el2
	MOVD	$16, R5
	BL	·uhex(SB)
	MOVD	$0x46, R3	// 'F'
	BL	·uputc(SB)
	WORD	$0xd53c6004	// mrs x4, far_el2
	MOVD	$16, R5
	BL	·uhex(SB)
faulthang2:
	B	faulthang2

// uputc: teken in R3 naar de UART (R8=DR, R9=FR), met TXFF-poll.
TEXT ·uputc(SB),NOSPLIT|NOFRAME,$0
uputw:
	MOVWU	(R9), R10
	TBNZ	$5, R10, uputw
	MOVW	R3, (R8)
	RET

// uhex: R4 als hex op de UART; R5 = aantal nibbles (8 of 16). Clobbert
// R3/R5/R6/R10/R11; bewaart de link-register rond de geneste BL.
TEXT ·uhex(SB),NOSPLIT|NOFRAME,$0
	MOVD	R30, R11
uhexlus:
	SUB	$1, R5, R5
	LSL	$2, R5, R6
	LSR	R6, R4, R10
	AND	$0xF, R10, R10
	ADD	$0x30, R10, R3
	CMP	$0x3A, R3
	BLT	uhexpr
	ADD	$39, R3	// a-f
uhexpr:
	BL	·uputc(SB)
	CBNZ	R5, uhexlus
	MOVD	R11, R30
	RET

// cpuinitEL1App: het schone EL1-pad voor een app-core onder stage-2 (fase
// P1). Géén MMIO (UART/noodvectoren zijn in de kooi niet gemapt — élke fout
// wordt al door de EL2-vectoren gerapporteerd + geparkeerd), géén dcinv (HOP
// veegde de partitie met dev.CleanInv vóór het laden). Alleen: SCTLR schoon,
// stack uit de (door HOP gepatchte) RAM-declaratie, door naar de runtime —
// identiek aan het rpi5/qemu-app-pad.
TEXT ·cpuinitEL1App(SB),NOSPLIT|NOFRAME,$0
	MRS	SCTLR_EL1, R0
	BIC	$1<<1, R0
	BIC	$1<<0, R0
	MSR	R0, SCTLR_EL1
	ISB	$15

	MOVD	runtime∕goos·RamStart(SB), R1
	MOVD	R1, RSP
	MOVD	runtime∕goos·RamSize(SB), R1
	MOVD	runtime∕goos·RamStackOffset(SB), R2
	ADD	R1, RSP
	SUB	R2, RSP

	B	_rt0_tamago_start(SB)
