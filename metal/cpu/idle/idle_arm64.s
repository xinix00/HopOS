// WFE + CNTKCTL_EL1 (event-stream-configuratie). Encodings via WORD, zoals
// overal in dit repo: MSR = 0xd5180000 | op1<<16 | CRn<<12 | CRm<<8 | op2<<5 | Rt.

//go:build tamago && arm64

#include "textflag.h"

// wfeIdle: counterstand om de WFE heen — de retourwaarde is de écht in WFE
// doorgebrachte tijd in generic-timer-ticks. CNTVCT_EL0 (virtueel: trapt
// nooit op EL1, telt door in WFE).
TEXT ·wfeIdle(SB),NOSPLIT,$0-8
	WORD	$0xd53be040	// mrs x0, cntvct_el0
	WFE
	WORD	$0xd53be041	// mrs x1, cntvct_el0
	SUB	R0, R1, R0
	MOVD	R0, ret+0(FP)
	RET

TEXT ·cntkctlSet(SB),NOSPLIT,$0-8
	MOVD	v+0(FP), R0
	WORD	$0xd518e100	// msr cntkctl_el1, x0
	ISB	$15
	RET

TEXT ·cntfrq(SB),NOSPLIT,$0-8
	WORD	$0xd53be000	// mrs x0, cntfrq_el0
	MOVD	R0, ret+0(FP)
	RET
