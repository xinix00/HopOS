// PSCI-conduits volgens SMCCC (args in R0-R3, resultaat in R0). Op de Pi's
// zit de provider (TF-A BL31, de armstub) op EL3 onder ons → SMC #0. De
// HVC-variant bestaat voor het (theoretische) EL1-boot-pad, zelfde patroon
// als board/qemuvirt.

//go:build tamago && arm64

#include "textflag.h"

TEXT ·hvc4(SB),NOSPLIT,$0-40
	MOVD	fn+0(FP), R0
	MOVD	a1+8(FP), R1
	MOVD	a2+16(FP), R2
	MOVD	a3+24(FP), R3
	WORD	$0xd4000002	// HVC #0
	MOVD	R0, ret+32(FP)
	RET

TEXT ·smc4(SB),NOSPLIT,$0-40
	MOVD	fn+0(FP), R0
	MOVD	a1+8(FP), R1
	MOVD	a2+16(FP), R2
	MOVD	a3+24(FP), R3
	WORD	$0xd4000003	// SMC #0
	MOVD	R0, ret+32(FP)
	RET
