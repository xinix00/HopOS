// cpuinit_body.h — de gedeelde boot voor de Raspberry Pi 4 en Pi 5 (BCM2711/
// BCM2712): de generieke boot (cpu/el2/boot.h) met de haken die de Pi's eigen
// zijn. Beide board-cpuinit.s'en #include'n dit ná hun #define's van UART_DR/
// UART_FR (de printk/faultdump-poke: rpi4 0xFE201000, rpi5 0x107d001000) en
// na #include "textflag.h". De vijftien instructies ertussen staan in boot.h
// en drop.h. Wijzig wat hier staat met beleid: dit was het duurst-gedebugde
// bestand van de boot, en de lessen staan bij de haken.
//
//   - BOARD_EARLY (earlyPi): 'P' + boot-EL op de UART vóór alles, en de
//     EL2-vectortabel voor faultdump2 op 0x8B000 — dat ís layout.TrapVecPA
//     van het Pi-plan: boot.h zet VBAR_EL2 erheen, stage2.InitVectors plugt
//     er de revoke-HVC-handler in (offset 0x400), de rest blijft de
//     Y-dump-diagnostiek.
//   - BOARD_EL2 (el2Pi): de Linux-init_el2-pariteit — SCTLR_EL2, VTTBR=0,
//     MDCR, MDSCR, VPIDR/VMPIDR, HSTR, CPACR_EL1: de registers die bij
//     EL2-entry garbage zijn (de P2R-hang van 2026-07-09).
//   - BOARD_EL1 (el1Pi): de EL1-faultdump-tabel op 0x8A000 (VBAR_EL1), de
//     D-cache-invalidatie over de hele RAM-declaratie, en 'R','p' + PC.
//
// CPUECTLR_EL1 (S3_0_C15_C1_4, SMPEN) WORDT NIET AANGERAAKT — zie el2Pi.

#include "../../cpu/el2/sysreg.h"
#include "../../cpu/el2/drop.h"

#define BOOT_SCRATCH 0x7F000	// = raspi.BootScratch; +8 = DTB (raspi.DTB)
#define TRAP_VEC     0x8B000	// = revokeVecAsm (plan.go): de faultdump2-tabel die earlyPi bouwt
#define BOARD_EARLY  BL ·earlyPi(SB)
#define BOARD_EL2    BL ·el2Pi(SB)
#define BOARD_EL1    BL ·el1Pi(SB)
#include "../../cpu/el2/boot.h"

// earlyPi: vóór alles, op het boot-EL. Klobbert R0, R2–R7; x9 (de DTB) blijft.
TEXT ·earlyPi(SB),NOSPLIT|NOFRAME,$0
	// 'P' + boot-EL: het eerste levensteken, vóór welke sysreg dan ook.
	// Begrensde poll op FR.TXFF — een dode UART mag de boot niet ophouden.
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
	MRS	CurrentEL, R0
	LSR	$2, R0, R0
	AND	$0b11, R0, R0
	ADD	$0x30, R0, R3	// '0' + EL
	MOVW	R3, (R2)
uartklaar:
	// EL2-vectortabel → ·faultdump2: mocht er tóch iets naar EL2 trappen,
	// dan een 'Y'-dump (ESR/ELR/FAR_EL2) i.p.v. een stille hang. Tabel op
	// 0x8B000, zelfde vrije gat als de EL1-tabel (0x8A000). Hier gebouwd,
	// vóór boot.h VBAR_EL2 erheen zet. B-encoding: 0x14000000 |
	// ((doel-entry)>>2 & imm26).
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
	RET

// el2Pi: op EL2, ná HCR = RW en vóór de drop. Linux init_el2-pariteit
// (el2_setup.h, afgekeken 2026-07-09): de registers die Linux óók init en die
// bij EL2-entry garbage zijn. CPTR_EL2 (de TFP-hang van 09-07: 'P2R' en dan
// niets) en SCTLR_EL1 (een geërfde WXN=1 maakt tamago's RW+X-mapping
// executable-never zodra de MMU aangaat) doet de drop, drop.h.
//
// CPUECTLR_EL1 (S3_0_C15_C1_4, SMPEN) WORDT HIER NIET AANGERAAKT. Stond hier
// als "laatste goedkope hefboom" met de kanttekening dat het op DynamIQ-A76
// vermoedelijk een no-op is omdat de firmware de DSU doet — en dat blijkt te
// kloppen én gevaarlijk: de EEPROM-bootloader van 2026-05 brengt een nieuwe
// BL31 mee (v2.6-240, Dec 2024) die EL2 geen toegang meer geeft tot dit
// IMPDEF-register. De `mrs` trapt dan naar EL3 en komt nooit terug — een
// STILLE hang (gemeten 04-08 met stapmarkers: P2abc en dan niets). Een
// register dat de firmware al beheert, hoort een OS niet af te pakken. De A72
// (Pi 4) heeft bovendien een ándere SMPEN-encoding die TF-A al zet.
TEXT ·el2Pi(SB),NOSPLIT|NOFRAME,$0
	// SCTLR_EL2 = INIT_SCTLR_EL2_MMU_OFF (RES1-bits, rest uit).
	MOVD	$0x30C50830, R0
	WORD	$0xd51c1000	// msr sctlr_el2, x0
	ISB	$15
	// VTTBR_EL2 = 0: óók met stage-2 uit tagt VTTBR's VMID álle
	// EL1&0-TLB-entries — garbage-VMID + A76-TLB/PTW is de errata-hoek
	// (o.a. 1165522). Dé kanshebber voor de multi-level-PTW-wedge.
	WORD	$0xd51c211f	// msr vttbr_el2, xzr
	// MDCR_EL2: HPMN = PMCR_EL0.N (Linux-recept), alle debug-traps uit.
	WORD	$0xd53b9c00	// mrs x0, pmcr_el0
	LSR	$11, R0, R0
	AND	$0x1F, R0, R0
	WORD	$0xd51c1120	// msr mdcr_el2, x0
	// MDSCR_EL1 = 0: geen achtergebleven debug-enable-bits (__cpu_setup).
	WORD	$0xd510025f	// msr mdscr_el1, xzr
	// De rest van het U-Boot/Circle-recept (armv8_switch_to_el1_m) — op
	// Pi 5-silicium bewezen; bij EL2-entry zijn deze EL1-gezichten anders
	// architectureel UNKNOWN. VPIDR/VMPIDR: wat EL1 ziet bij midr/mpidr.
	WORD	$0xd5380000	// mrs x0, midr_el1
	WORD	$0xd51c0000	// msr vpidr_el2, x0
	WORD	$0xd53800a0	// mrs x0, mpidr_el1
	WORD	$0xd51c00a0	// msr vmpidr_el2, x0
	// HSTR_EL2 = 0: geen aarch32-CP15-traps.
	WORD	$0xd51c117f	// msr hstr_el2, xzr
	// CPACR_EL1: FP/SIMD aan vóór de runtime (tamago's fp_enable komt pas
	// in hwinit0 — maar niets vóór die tijd mag al trappen).
	MOVD	$(3<<20), R0
	MSR_CPACR_EL1(0)
	RET

// el1Pi: op EL1, vóór SCTLR en de stack. De banner BL't (uputc/uhex, die
// R11 als LR-klad gebruiken), dus LR in R13.
TEXT ·el1Pi(SB),NOSPLIT|NOFRAME,$0
	MOVD	R30, R13
	// EL1-vectoren → ·faultdump (X-dump: ESR/ELR/FAR_EL1, LR, SP en een
	// stukje stack) op 0x8A000, vóór tamago zijn eigen vectoren zet.
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

	// Cache-maintenance vóór de runtime — de fase-P-les die QEMU verhulde:
	// de firmware draaide mét caches; stale (schone) lines boven onze RAM
	// overleven de handoff. Tamago's set_ttbr0_el1 zet MMU+I+D aan zónder
	// invalidatie → de table-walker en eerste cached reads zien firmware-
	// spoken i.p.v. onze (MMU-uit, dus direct naar DRAM geschreven) data.
	// Dus: D-cache per 64B-lijn invalideren over de hele RAM-declaratie
	// (de firmware heeft het image zelf naar PoC gecleand — DRAM is de
	// waarheid, bewezen door de uncached 'P2R'-executie), en I-cache leeg.
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

	// 'R' (rauw), dan 'p' + de échte PC via de helpers: staat de runtime
	// straks stil, dan is dit het laatste woord van de boot.
	MOVD	$UART_DR, R2
	MOVD	$0x52, R3	// 'R'
	MOVW	R3, (R2)
	MOVD	$UART_DR, R8
	MOVD	$UART_FR, R9
	MOVD	$0x70, R3	// 'p'
	BL	·uputc(SB)
	WORD	$0x10000004	// adr x4, . (de échte PC)
	MOVD	$16, R5
	BL	·uhex(SB)
	MOVD	R13, R30
	RET

// faultdump: het boot-venster-exceptiehandler — print "XE<esr>L<elr>F<far>"
// (hex) op de UART en parkeert. UART is op dit punt bewezen ('P2R').
// Registers vrij: we keren nooit terug.
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
