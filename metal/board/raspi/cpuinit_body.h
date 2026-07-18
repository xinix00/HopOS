// cpuinit_body.h — de gedeelde EL2-capabele CPU-init voor de Raspberry Pi 4 en
// Pi 5 (BCM2711/BCM2712). Op instructieniveau is dit pad 99% identiek tussen
// beide boards; de enige twee board-verschillen worden buiten dit bestand
// geregeld:
//
//   - UART_DR/UART_FR: als #define in de per-board cpuinit.s (rpi4 0xFE201000,
//     rpi5 0x107d001000) — de printk/faultdump-poke;
//   - de A76-SMPEN-write (CPUECTLR_EL1, S3_0_C15_C1_4 bit6): alleen de Pi 5,
//     geguard met #ifdef RPI5. De A72 (Pi 4) heeft een ándere SMPEN-encoding
//     die TF-A al zet — dáár is deze write UNDEF-risico en dus weggelaten.
//
// Dit bestand wordt door beide board-cpuinit.s'en ge#include'd NÁ hun
// #define's van BOOT_SCRATCH/DTB_PTR/UART_DR/UART_FR (+ #define RPI5 op de Pi 5)
// en na #include "textflag.h". Wijzig het instructiepad hier met beleid: dit is
// het duurst-gedebugde bestand van de boot.

TEXT cpuinit(SB),NOSPLIT|NOFRAME,$0
	MOVD	R0, R9		// x0 = DTB-pointer bij firmware-boot; bewaren vóór clobber

	// App-cores (fase P1) entreren hier op EL1, via de EL2-trampoline en
	// ónder stage-2: UART/scratch zijn daar niet gemapt — één MMIO-poke
	// en de EL2-vector zet de core uit. Dus vóór álles: EL1 → het schone
	// app-pad (geen MMIO, geen noodvectoren, geen dcinv). De primary komt op
	// de Pi altijd op EL2 binnen (TF-A/armstub) en krijgt hieronder de volle
	// diagnostiek.
	MRS	CurrentEL, R0
	LSR	$2, R0, R0
	AND	$0b11, R0, R0
	CMP	$1, R0
	BNE	primary
	B	·cpuinitEL1App(SB)

primary:
	// Levensteken 1: 'P' (Pi). Begrensd gepolld: een dode FIFO-vol-vlag kost
	// hooguit de poll, nooit de boot. (Blijft de boot stil hangen zónder
	// debug-sessie, dan stalt de FR-read de bus — meet dat mét de Debug Probe
	// aangesloten, zie docs/archief/rpi5.md.)
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

	// VBAR_EL2 van de HOP-core wordt verderop op de faultdump2-tabel
	// (0x8B000) gezet — dat ís layout.RevokeVecPA van het rpi5-plan:
	// stage2.InitVectors plugt daar de revoke-HVC-handler in (offset 0x400),
	// de rest blijft de Y-dump-diagnostiek. App-cores entreren op EL1 via de
	// trampoline en krijgen hun eigen VBAR_EL2 (CtrlVecPA) van de trampoline.

	// HCR_EL2: RW(31)=1 — EL1 draait AArch64. Stage-2 (VM-bit) blijft uit;
	// de EL2-trampoline (metal/el2) zet VTTBR_EL2 + VM op de app-cores.
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
	// op EL2 (boot-meting 2026-07-09: 'P2R' en dan niets, géén EL1-fault).
	MOVD	$0x33FF, R0
	WORD	$0xd51c1140	// msr cptr_el2, x0

	// Linux init_el2-pariteit (el2_setup.h, afgekeken 2026-07-09): de
	// registers die Linux óók init en die bij EL2-entry garbage zijn.
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

#ifdef RPI5
	// COHERENTIE vóór de MMU — de tamago-Broadcom-volgorde (soc/bcm2835:
	// EnableSMP() vóór InitMMU/EnableCache; "must be ensured before caches
	// and MMU are enabled or any TLB maintenance"). Op 32-bit is dat
	// ACTLR.SMP; de A76-tegenhanger is CPUECTLR_EL1.SMPEN (bit 6),
	// S3_0_C15_C1_4. LET OP: op DynamIQ-A76 mogelijk RES0/no-op (firmware
	// doet de DSU) — dit is de laatste goedkope hefboom voor de multi-level-
	// PTW-wedge; markers zeggen of het 'm was. Read-modify-write, één bit.
	WORD	$0xd538f180	// mrs x0, S3_0_C15_C1_4 (CPUECTLR_EL1)
	ORR	$1<<6, R0, R0	// SMPEN
	WORD	$0xd518f180	// msr S3_0_C15_C1_4, x0
	ISB	$15
#endif

	// De rest van het U-Boot/Circle-recept (armv8_switch_to_el1_m) — op
	// Pi 5-silicium bewezen; bij EL2-entry zijn deze EL1-gezichten anders
	// architectureel UNKNOWN (2026-07-09, afgekeken na de P2R-hang):
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
	// EL3-pad (volledigheid; de Pi-firmware levert EL2 af).
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
	// zet): elke EL1-exceptie → ·faultdump (X + ESR/ELR/FAR in hex op de
	// UART) i.p.v. een stille hang. Tabel op 0x8A000: 2KB-gealigneerd,
	// in het vrije gat tussen tamago's pagetables (ramStart+0x4000..0x9000)
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


	// Levensteken 4: 'R' — op weg naar de runtime (bisect-marker: P2..R =
	// EL-drop/stack, R..H = rt0 + Hwinit0/MMU + vroege runtime, zie hwinit1).
	MOVD	$UART_DR, R2
	MOVD	$0x52, R3	// 'R'
	MOVW	R3, (R2)

	// PC-dump 'p<pc>': het onweerlegbare laadadres-bewijs (ADR = pure
	// hardware-PC). Gelinkt op de werkelijkheid hoort hier ~0xExxxx te
	// staan; stond het image verkeerd, dan wijkt dit af van het linkadres.
	MOVD	$UART_DR, R8
	MOVD	$UART_FR, R9
	MOVD	$0x70, R3	// 'p'
	BL	·uputc(SB)
	WORD	$0x10000004	// adr x4, . (de échte PC)
	MOVD	$16, R5
	BL	·uhex(SB)

	B	_rt0_tamago_start(SB)

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

// cpuinitEL1App: het schone EL1-pad voor een app-core onder stage-2 (fase
// P1). Géén MMIO (LED/UART/noodvectoren zijn in de kooi niet gemapt — élke
// fout wordt al door de EL2-vectoren gerapporteerd + CPU_OFF), géén dcinv
// (HOP veegde de partitie met dev.CleanInv vóór het laden en de core komt
// uit reset met schone L1-caches). Alleen: SCTLR schoon, stack uit de
// (door HOP gepatchte) RAM-declaratie, en door naar de runtime — identiek
// aan het QEMU-app-pad (board/qemuvirt/cpuinit.s).
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
