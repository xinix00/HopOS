// Systeemregisters die de probe rapporteert. Alleen lezen; WORD-encodering
// omdat de Go-assembler niet elk ID-register bij naam kent.

//go:build tamago && arm64

#include "textflag.h"

TEXT ·ReadTCR(SB),NOSPLIT,$0-8
	WORD	$0xd5382040	// mrs x0, tcr_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·ReadSCTLR(SB),NOSPLIT,$0-8
	WORD	$0xd5381000	// mrs x0, sctlr_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·ReadMMFR0(SB),NOSPLIT,$0-8
	WORD	$0xd5380700	// mrs x0, id_aa64mmfr0_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·ReadMMFR1(SB),NOSPLIT,$0-8
	WORD	$0xd5380720	// mrs x0, id_aa64mmfr1_el1 (VH = bits 11:8)
	MOVD	R0, ret+0(FP)
	RET

TEXT ·ReadMMFR4(SB),NOSPLIT,$0-8
	WORD	$0xd5380780	// mrs x0, id_aa64mmfr4_el1 (E2H0 = bits 27:24; ongeïmplementeerd leest 0)
	MOVD	R0, ret+0(FP)
	RET

TEXT ·ReadCNTKCTL(SB),NOSPLIT,$0-8
	WORD	$0xd538e100	// mrs x0, cntkctl_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·CNTFRQ(SB),NOSPLIT,$0-8
	WORD	$0xd53be000	// mrs x0, cntfrq_el0
	MOVD	R0, ret+0(FP)
	RET

TEXT ·ReadSPRRConfig(SB),NOSPLIT,$0-8
	WORD	$0xd53ef100	// mrs x0, s3_6_c15_c1_0 (SYS_IMP_APL_SPRR_CONFIG_EL1)
	MOVD	R0, ret+0(FP)
	RET

TEXT ·tlbiAll(SB),NOSPLIT,$0-0
	WORD	$0xd5033f9f	// dsb sy
	WORD	$0xd508871f	// tlbi vmalle1
	WORD	$0xd5033f9f	// dsb sy
	WORD	$0xd5033fdf	// isb
	RET

// wfeBurst doet n keer WFE en geeft de verstreken counter-ticks terug: de
// meting die zegt of WFE op dit silicium écht slaapt of alleen een
// klaarstaand event consumeert.
TEXT ·wfeBurst(SB),NOSPLIT,$0-16
	MOVD	n+0(FP), R2
	WORD	$0xd53be040	// mrs x0, cntvct_el0
loopw:
	WFE
	SUB	$1, R2, R2
	CBNZ	R2, loopw
	WORD	$0xd53be041	// mrs x1, cntvct_el0
	SUB	R0, R1, R0
	MOVD	R0, ret+8(FP)
	RET

// wfiTimer zet de fysieke EL1-timer op `ticks` vooruit, doet één WFI en zet de
// timer weer uit; het resultaat is de werkelijk verstreken tijd in ticks. Dit
// is het alternatief voor de WFE-event-stream op een teller waar WFE nauwelijks
// slaapt: een echte deadline i.p.v. een gedeelde event-flank. De interrupt komt
// niet binnen (DAIF gemaskeerd, en niemand routeert de AIC) maar wekt WFI wél —
// dat is precies wat de architectuur belooft voor WFI-wake-up events.
TEXT ·wfiTimer(SB),NOSPLIT,$0-16
	MOVD	ticks+0(FP), R2
	WORD	$0xd53be040	// mrs x0, cntvct_el0
	WORD	$0xd51be202	// msr cntp_tval_el0, x2
	MOVD	$1, R3
	WORD	$0xd51be223	// msr cntp_ctl_el0, x3 (ENABLE, IMASK=0)
	ISB	$15
	WFI
	MOVD	$0, R3
	WORD	$0xd51be223	// msr cntp_ctl_el0, x3 (uit — anders blijft hij pending)
	ISB	$15
	WORD	$0xd53be041	// mrs x1, cntvct_el0
	SUB	R0, R1, R0
	MOVD	R0, ret+8(FP)
	RET
