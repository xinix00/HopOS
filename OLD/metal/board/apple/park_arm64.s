// De parkeerlus voor secundaire cores tijdens de bring-up-probe — kloon van
// board/rk3566/park_arm64.s, andere aanleverroute: hier is het geen PSCI
// CPU_ON maar m1n1's spin-table die de core hier afzet (Release in params.go).
// De conventie is dezelfde: x0 = ctx, hier het levenstekenwoord. De core komt
// op EL2 binnen met MMU uit; hij schrijft zijn MPIDR en gaat eeuwig WFE —
// geen stack, geen runtime, niets wat op nieuw silicium kan misgaan behalve
// de vraag die we stellen.

//go:build tamago && arm64

#include "textflag.h"

TEXT parkEntry(SB),NOSPLIT|NOFRAME,$0
	MRS	MPIDR_EL1, R1
	MOVD	R1, (R0)	// ctx-woord = mijn MPIDR: levensteken én meting
	WORD	$0xd5033f9f	// dsb sy — zichtbaar voor de HOP-core (die leest ongecached)
loop:
	WFE
	JMP	loop

// ParkEntryPC geeft het fysieke adres van de parkeerlus (het image is
// identity-geladen, dus symbooladres = fysiek adres).
TEXT ·ParkEntryPC(SB),NOSPLIT,$0-8
	MOVD	$parkEntry(SB), R0
	MOVD	R0, ret+0(FP)
	RET
