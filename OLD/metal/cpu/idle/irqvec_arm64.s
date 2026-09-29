//go:build tamago && arm64

#include "textflag.h"

// hopIRQVector: de IRQ-vector van HOP's core. Zet irqFlag, maskeer I en F in
// de SPSR (de ISR-goroutine opent I weer) en keer terug. Geen aanroep de
// runtime in — zie irqdoor_arm64.go. Alleen x0/x1 geraakt, bewaard onder SP.
TEXT ·hopIRQVector(SB),NOSPLIT|NOFRAME,$0
	STP	(R0, R1), -16(RSP)
	MOVD	$·irqFlag(SB), R0
	MOVW	$1, R1
	MOVW	R1, (R0)
	MRS	SPSR_EL1, R0
	ORR	$(1<<7|1<<6), R0, R0
	MSR	R0, SPSR_EL1
	LDP	-16(RSP), (R0, R1)
	ERET

// syncVector: een net geschreven vector-ingang zichtbaar maken voor de
// instructie-fetch (dc cvau, dsb, ic ivau, dsb, isb).
TEXT ·syncVector(SB),NOSPLIT,$0-8
	MOVD	addr+0(FP), R0
	WORD	$0xd50b7b20	// dc cvau, x0
	WORD	$0xd5033b9f	// dsb ish
	WORD	$0xd50b7520	// ic ivau, x0
	WORD	$0xd5033b9f	// dsb ish
	WORD	$0xd5033fdf	// isb
	RET
