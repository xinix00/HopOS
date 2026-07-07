// ICC_SGI1R_EL1-write: genereert een groep-1-SGI (GICv3, systeemregister-
// interface; QEMU's GICv3 is SRE-only dus altijd beschikbaar). Encoding via
// WORD — de Go-assembler kent de ICC-registers niet bij naam:
// MSR = 0xd5180000 | op1<<16 | CRn<<12 | CRm<<8 | op2<<5 | Rt;
// ICC_SGI1R_EL1 = op1=0, CRn=12, CRm=11, op2=5.

//go:build tamago && arm64

#include "textflag.h"

TEXT ·icc_sgi1r(SB),NOSPLIT,$0-8
	MOVD	v+0(FP), R0
	WORD	$0xd518cba0	// msr icc_sgi1r_el1, x0
	WORD	$0xd5033fdf	// isb
	RET
