// De syndroomregisters van EL1 voor het exception-rapport (exc_arm64.go).

//go:build tamago && arm64

#include "textflag.h"

TEXT ·esrEL1(SB),NOSPLIT,$0-8
	WORD	$0xd5385200	// mrs x0, esr_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·farEL1(SB),NOSPLIT,$0-8
	WORD	$0xd5386000	// mrs x0, far_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·elrEL1(SB),NOSPLIT,$0-8
	WORD	$0xd5384020	// mrs x0, elr_el1
	MOVD	R0, ret+0(FP)
	RET
