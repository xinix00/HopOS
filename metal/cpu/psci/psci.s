// PSCI-conduit volgens SMCCC (args in R0-R3, resultaat in R0). Altijd SMC:
// HopOS eist een EL2-boot, dus de provider (TF-A op de Pi/O6N, of QEMU met
// virtualization=on) zit per definitie onder ons.

//go:build tamago && arm64

#include "textflag.h"

TEXT ·SMC(SB),NOSPLIT,$0-40
	MOVD	fn+0(FP), R0
	MOVD	a1+8(FP), R1
	MOVD	a2+16(FP), R2
	MOVD	a3+24(FP), R3
	WORD	$0xd4000003	// SMC #0
	MOVD	R0, ret+32(FP)
	RET

// SMC4 — als SMC, maar leest R0-R3 terug (TRNG_RND levert entropie in R1-R3).
TEXT ·SMC4(SB),NOSPLIT,$0-64
	MOVD	fn+0(FP), R0
	MOVD	a1+8(FP), R1
	MOVD	a2+16(FP), R2
	MOVD	a3+24(FP), R3
	WORD	$0xd4000003	// SMC #0
	MOVD	R0, r0+32(FP)
	MOVD	R1, r1+40(FP)
	MOVD	R2, r2+48(FP)
	MOVD	R3, r3+56(FP)
	RET
