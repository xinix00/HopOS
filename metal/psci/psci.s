// PSCI-conduits volgens SMCCC (args in R0-R3, resultaat in R0). Gedeeld door
// alle boards; de keuze HVC (hypervisor boven ons, bv. QEMU zonder
// virtualization=on) vs SMC (firmware onder ons, bv. TF-A op de Pi/O6N) maakt
// het board op grond van het boot-EL.

//go:build tamago && arm64

#include "textflag.h"

TEXT ·HVC(SB),NOSPLIT,$0-40
	MOVD	fn+0(FP), R0
	MOVD	a1+8(FP), R1
	MOVD	a2+16(FP), R2
	MOVD	a3+24(FP), R3
	WORD	$0xd4000002	// HVC #0
	MOVD	R0, ret+32(FP)
	RET

TEXT ·SMC(SB),NOSPLIT,$0-40
	MOVD	fn+0(FP), R0
	MOVD	a1+8(FP), R1
	MOVD	a2+16(FP), R2
	MOVD	a3+24(FP), R3
	WORD	$0xd4000003	// SMC #0
	MOVD	R0, ret+32(FP)
	RET
