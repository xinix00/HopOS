// ChainloadM: de kern-flip-sprong op RISC-V (docs/kern-flip.md).
//
// Veel eenvoudiger dan de ARM-kant, en dat is geen toeval maar het privilege-
// model: HOP draait hier zélf in machine mode zonder MMU (node_riscv64.go
// weigert alles behalve M-mode), dus er is geen vertaalregime om af te breken
// en geen exception-niveau om over te steken. Wat er wél moet gebeuren:
//
//   - interrupts uit (mie): de nieuwe kern zet zijn eigen wereld op, en een
//     geërfde timer-interrupt die halverwege binnenkomt zou in zijn trap-entry
//     landen vóór mtvec van hem is;
//   - fence + fence.i: de bytes die we net als DATA schreven moeten als
//     INSTRUCTIE zichtbaar zijn voor dit hart. Zonder fence.i kan de I-cache
//     nog de vorige huurder van die adressen serveren.
//
// a0 = entry (fysiek), a1 = het argument dat de nieuwe kern in a0 verwacht
// (hier ongebruikt door de riscv64-boot, maar meegegeven zodat de vorm gelijk
// is aan ARM). Keert nooit terug.

//go:build tamago && riscv64

#include "textflag.h"

TEXT ·ChainloadM(SB),NOSPLIT,$0-16
	MOV	entry+0(FP), X5
	MOV	x0arg+8(FP), X10	// a0 voor de nieuwe kern
	WORD	$0x30401073		// csrw mie, x0 — geen interrupt-erfenis
	FENCE
	WORD	$0x0000100f		// fence.i — de nieuwe bytes zijn nu ook code
	JALR	X0, (X5)
