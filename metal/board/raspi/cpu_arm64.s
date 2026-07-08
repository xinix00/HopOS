// MPIDR_EL1-read: de eigen core-identiteit. Waar het corenummer zit is
// boardspecifiek (A72: aff0, A76: aff1) — het maskeren gebeurt in Go
// (CoreID in het board-pakket).

//go:build tamago && arm64

#include "textflag.h"

TEXT ·mpidr(SB),NOSPLIT,$0-8
	WORD	$0xd53800a0	// mrs x0, mpidr_el1
	MOVD	R0, ret+0(FP)
	RET

// CNTFRQ_EL0: de counterfrequentie zoals de firmware hem zette (54MHz op de
// Pi). Lezen trapt nooit; 0 = firmware vergat hem → timer/Sleep dood.
TEXT ·cntfrq(SB),NOSPLIT,$0-8
	WORD	$0xd53be000	// mrs x0, cntfrq_el0
	MOVD	R0, ret+0(FP)
	RET

// CNTPCT_EL0: de fysieke counter. LET OP: kán trappen op EL1 als de
// hypervisor-laag EL1PCTEN niet zette — de aanroeper hoort zich eerst op
// het scherm aan te kondigen (probe-patroon), dan wijst een bevriezing
// deze read aan.
TEXT ·cntpct(SB),NOSPLIT,$0-8
	WORD	$0xd53be020	// mrs x0, cntpct_el0
	MOVD	R0, ret+0(FP)
	RET
