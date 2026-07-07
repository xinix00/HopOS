// MPIDR_EL1-read: de eigen core-identiteit. Op de Cortex-A76 (MT-formaat)
// zit het corenummer in aff1 — het maskeren gebeurt in Go (rpi5.CoreID).

//go:build tamago && arm64

#include "textflag.h"

TEXT ·mpidr(SB),NOSPLIT,$0-8
	WORD	$0xd53800a0	// mrs x0, mpidr_el1
	MOVD	R0, ret+0(FP)
	RET
