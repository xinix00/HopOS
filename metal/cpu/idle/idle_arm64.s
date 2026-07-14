// WFE + CNTKCTL_EL1 (event-stream-configuratie). Encodings via WORD, zoals
// overal in dit repo: MSR = 0xd5180000 | op1<<16 | CRn<<12 | CRm<<8 | op2<<5 | Rt.

//go:build tamago && arm64

#include "textflag.h"

TEXT ·wfe(SB),NOSPLIT,$0
	WFE
	RET

TEXT ·cntkctlSet(SB),NOSPLIT,$0-8
	MOVD	v+0(FP), R0
	WORD	$0xd518e100	// msr cntkctl_el1, x0
	ISB	$15
	RET
