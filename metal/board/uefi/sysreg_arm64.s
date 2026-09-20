#include "textflag.h"

// func ReadESR() uint64 — ESR_EL1 van deze core (na een EL1-exception).
TEXT ·ReadESR(SB),NOSPLIT|NOFRAME,$0-8
	WORD	$0xd5385200	// mrs x0, esr_el1
	MOVD	R0, ret+0(FP)
	RET

// func ReadFAR() uint64 — FAR_EL1 van deze core.
TEXT ·ReadFAR(SB),NOSPLIT|NOFRAME,$0-8
	WORD	$0xd5386000	// mrs x0, far_el1
	MOVD	R0, ret+0(FP)
	RET
