// De quirk-instructies achter layout.Prep* (idle.go corePrep): wat een app-core
// aan zijn eigen silicium rechtzet omdat alleen EL1 erbij kan.

//go:build tamago && arm64

#include "textflag.h"

// func cycOvrdWFIUp() — Apple SYS_IMP_APL_CYC_OVRD (s3_5_c15_c5_0):
// WFI_MODE (bits 25:24) = 2, "up". Read-modify-write, de rest van het
// register blijft staan. Encoding: MSR = 0xd5000000 | o0<<19 | op1<<16 |
// CRn<<12 | CRm<<8 | op2<<5 | Rt, met o0=1 (op0=3), op1=5, CRn=15, CRm=5,
// op2=0 → 0xd51df500; MRS = +bit 20 → 0xd53df500. Dezelfde woorden als de
// meting van 02-09 (vitals, slot 1 en 2 op de M4).
TEXT ·cycOvrdWFIUp(SB),NOSPLIT,$0-0
	WORD	$0xd53df500	// mrs x0, s3_5_c15_c5_0
	MOVD	$(3<<24), R1
	BIC	R1, R0, R0
	ORR	$(2<<24), R0, R0
	WORD	$0xd51df500	// msr s3_5_c15_c5_0, x0
	ISB	$15
	RET
