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

// flushTLB: DSB ISH (de tabel-writes zichtbaar voor de walker) → TLBI VMALLE1IS
// (alle stage-1-vertalingen van deze EL/VMID weg, op élke core van het inner-
// shareable domein: de tabellen zijn gedeeld — HOP's EL2-map draait de
// switcher op alle app-cores, en een SMP-app deelt zijn map met zijn
// secundairen) → DSB ISH (de invalidatie
// afgerond) → ISB (geen speculatieve toegang met een oude vertaling meer).
// Deze volgorde is de architectuur-eis, niet voorzichtigheid.
TEXT ·flushTLB(SB),NOSPLIT,$0
	WORD	$0xd5033b9f	// dsb ish
	WORD	$0xd508831f	// tlbi vmalle1is — inner-shareable: óók de andere cores
	WORD	$0xd5033b9f	// dsb ish
	WORD	$0xd5033fdf	// isb
	RET

// func readTCR() uint64 — TCR_EL1 (met VHE op EL2: TCR_EL2).
TEXT ·readTCR(SB),NOSPLIT,$0-8
	MRS	TCR_EL1, R0
	MOVD	R0, ret+0(FP)
	RET

// func readTTBR0() uintptr — de wortel van de eigen vertaling.
TEXT ·readTTBR0(SB),NOSPLIT,$0-8
	MRS	TTBR0_EL1, R0
	MOVD	R0, ret+0(FP)
	RET
