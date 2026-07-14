// MPIDR_EL1-read: de eigen core-identiteit (zelfde als qemuvirt; de
// Altra-affiniteitsindeling wordt in Go geïnterpreteerd, zie CoreID).

//go:build tamago && arm64

#include "textflag.h"

TEXT ·mpidr(SB),NOSPLIT,$0-8
	WORD	$0xd53800a0	// mrs x0, mpidr_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·ramStartAsm(SB),NOSPLIT,$0-8
	MOVD	runtime∕goos·RamStart(SB), R0
	MOVD	R0, ret+0(FP)
	RET

TEXT ·cntfrq(SB),NOSPLIT,$0-4
	WORD	$0xd53be000	// mrs x0, cntfrq_el0
	MOVW	R0, ret+0(FP)
	RET

TEXT ·readTCR(SB),NOSPLIT,$0-8
	WORD	$0xd5382040	// mrs x0, tcr_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·mmfr0(SB),NOSPLIT,$0-8
	WORD	$0xd5380700	// mrs x0, id_aa64mmfr0_el1
	MOVD	R0, ret+0(FP)
	RET

// switchMMU zet TCR_EL1 + TTBR0_EL1 om met de MMU heel even uit: tussen de
// twee writes bestaat anders een venster waarin de core oude en nieuwe
// vertaalconfig kan mengen (writes naar systeemregisters zijn pas ná een
// context-synchronisatie gegarandeerd, maar mógen eerder doorsijpelen).
// In het MMU-uit-venster wordt géén geheugen aangeraakt: argumenten eerst
// in registers, identity-fetch van de code zelf is per definitie goed.
TEXT ·switchMMU(SB),NOSPLIT,$0-16
	MOVD	tcr+0(FP), R1
	MOVD	ttbr0+8(FP), R2
	WORD	$0xd5381000	// mrs x0, sctlr_el1
	BIC	$1<<0, R0, R3
	WORD	$0xd5181003	// msr sctlr_el1, x3 (M=0)
	WORD	$0xd5033fdf	// isb
	WORD	$0xd5182041	// msr tcr_el1, x1
	WORD	$0xd5182002	// msr ttbr0_el1, x2
	WORD	$0xd5033fdf	// isb
	WORD	$0xd508871f	// tlbi vmalle1
	WORD	$0xd5033f9f	// dsb sy
	WORD	$0xd5033fdf	// isb
	WORD	$0xd5181000	// msr sctlr_el1, x0 (MMU weer aan)
	WORD	$0xd5033fdf	// isb
	RET

TEXT ·tlbiAll(SB),NOSPLIT,$0-0
	WORD	$0xd5033f9f	// dsb sy
	WORD	$0xd508871f	// tlbi vmalle1
	WORD	$0xd5033f9f	// dsb sy
	WORD	$0xd5033fdf	// isb
	RET
