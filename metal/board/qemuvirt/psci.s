// PSCI-conduits volgens SMCCC (args in R0-R3, resultaat in R0):
// HVC #0 bij een EL1-boot (hypervisor boven ons), SMC #0 bij een
// EL2/EL3-boot (firmware onder ons). Keuze in psci.go op basis van het
// boot-EL dat cpuinit op de scratch schreef.

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
