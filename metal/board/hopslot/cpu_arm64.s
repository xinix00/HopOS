// MPIDR_EL1-read: alleen de aff0-terugval van CoreID (images buiten slots
// om); het echte slotnummer komt uit de door HOP gepatchte slotHint.

//go:build tamago && arm64

#include "textflag.h"

TEXT ·mpidr(SB),NOSPLIT,$0-8
	WORD	$0xd53800a0	// mrs x0, mpidr_el1
	MOVD	R0, ret+0(FP)
	RET
