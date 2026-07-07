// MPIDR_EL1-read: de eigen core-identiteit (aff0 = core-index op virt).
// Op EL1 leesbaar; de EL2-vectoren gebruiken hetzelfde register (el2.s).

//go:build tamago && arm64

#include "textflag.h"

TEXT ·mpidr(SB),NOSPLIT,$0-8
	WORD	$0xd53800a0	// mrs x0, mpidr_el1
	MOVD	R0, ret+0(FP)
	RET
