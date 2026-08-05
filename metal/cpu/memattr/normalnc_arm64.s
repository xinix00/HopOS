//go:build tamago && arm64

#include "textflag.h"

// readMAIR: MRS x0, MAIR_EL1 — de acht attribuut-bytes van de stage-1-map.
TEXT ·readMAIR(SB),NOSPLIT,$0-8
	WORD	$0xd538a200	// mrs x0, mair_el1
	MOVD	R0, ret+0(FP)
	RET

// writeMAIR: MSR MAIR_EL1, x0 + ISB. Het ISB is er niet voor de sier: zonder
// context-synchronisatie mag de core een volgende toegang nog met de óude
// attribuut-tabel vertalen.
TEXT ·writeMAIR(SB),NOSPLIT,$0-8
	MOVD	v+0(FP), R0
	WORD	$0xd518a200	// msr mair_el1, x0
	WORD	$0xd5033fdf	// isb
	RET

// flushTLB: DSB ISH (de tabel-writes zichtbaar voor de walker) → TLBI VMALLE1
// (alle stage-1-vertalingen van deze EL/VMID weg) → DSB ISH (de invalidatie
// afgerond) → ISB (geen speculatieve toegang met een oude vertaling meer).
// Deze volgorde is de architectuur-eis, niet voorzichtigheid.
TEXT ·flushTLB(SB),NOSPLIT,$0
	WORD	$0xd5033b9f	// dsb ish
	WORD	$0xd508871f	// tlbi vmalle1
	WORD	$0xd5033b9f	// dsb ish
	WORD	$0xd5033fdf	// isb
	RET
