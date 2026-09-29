//go:build tamago && arm64

#include "textflag.h"

// ICC-systeemregisters van de GICv3-CPU-interface (ARM IHI 0069): de
// Group-1-set. Namen zoals de Go-assembler ze kent (tamago gebruikt dezelfde
// familie voor Group 0).

// func writeICCSRE(v uint64)
TEXT ·writeICCSRE(SB),NOSPLIT,$0-8
	MOVD	v+0(FP), R0
	MSR	R0, ICC_SRE_EL1
	ISB	$15
	RET

// func readICCSRE() uint64
TEXT ·readICCSRE(SB),NOSPLIT,$0-8
	MRS	ICC_SRE_EL1, R0
	MOVD	R0, ret+0(FP)
	RET

// func writeICCPMR(v uint64)
TEXT ·writeICCPMR(SB),NOSPLIT,$0-8
	MOVD	v+0(FP), R0
	MSR	R0, ICC_PMR_EL1
	ISB	$15
	RET

// func writeICCIGRPEN1(v uint64)
TEXT ·writeICCIGRPEN1(SB),NOSPLIT,$0-8
	MOVD	v+0(FP), R0
	MSR	R0, ICC_IGRPEN1_EL1
	ISB	$15
	RET

// func readICCIAR1() uint64
TEXT ·readICCIAR1(SB),NOSPLIT,$0-8
	ISB	$15
	MRS	ICC_IAR1_EL1, R0
	MOVD	R0, ret+0(FP)
	RET

// func writeICCEOIR1(v uint64)
TEXT ·writeICCEOIR1(SB),NOSPLIT,$0-8
	MOVD	v+0(FP), R0
	MSR	R0, ICC_EOIR1_EL1
	ISB	$15
	RET

// func writeICCSGI1R(v uint64) — ICC_SGI1R_EL1: één SGI naar één core.
TEXT ·writeICCSGI1R(SB),NOSPLIT,$0-8
	MOVD	v+0(FP), R0
	WORD	$0xd518cba0	// msr icc_sgi1r_el1, x0
	RET
