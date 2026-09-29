// De parkeerlus voor secundaire cores tijdens de bring-up-probe (trede 2).
//
// Wat een gewekte core hier doet, en waarom zo weinig: hij schrijft zijn eigen
// MPIDR in het woord dat CPU_ON hem als ctx (x0) meegaf, en gaat daarna eeuwig
// WFE. Dat is precies genoeg om de twee dingen te meten die we willen weten —
// kwám hij op, en wélk MPIDR heeft hij — zonder één regel die van de
// Go-runtime, een stack of de MMU afhangt. Een secundaire core heeft bij CPU_ON
// namelijk niets van dat alles: MMU uit, caches uit, geen geldige stack; elke
// Go-aanroep zou daar meteen op stuklopen.
//
// De ctx-conventie is die van PSCI zelf (DEN 0022): CPU_ON(target, entry, ctx)
// levert de core af op entry met ctx in x0. HOP gebruikt dat later voor de
// control-page van een kooi; hier is het één scratch-woord per core.

//go:build tamago && arm64

#include "textflag.h"

TEXT parkEntry(SB),NOSPLIT|NOFRAME,$0
	MRS	MPIDR_EL1, R1
	MOVD	R1, (R0)	// ctx-woord = mijn MPIDR: levensteken én meting
	WORD	$0xd5033f9f	// dsb sy — zichtbaar voor de HOP-core (die leest ongecached)
loop:
	WFE
	JMP	loop

// ParkEntryPC geeft het fysieke adres van de parkeerlus voor PSCI CPU_ON (het
// image is identity-geladen, dus symbooladres = fysiek adres). Zelfde vorm als
// cpu/el2's S2TrampPC.
TEXT ·ParkEntryPC(SB),NOSPLIT,$0-8
	MOVD	$parkEntry(SB), R0
	MOVD	R0, ret+0(FP)
	RET
