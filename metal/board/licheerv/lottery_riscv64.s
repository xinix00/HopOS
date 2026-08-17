// De boot-hart-loterij, asm-kant — contract in lottery.go. M-mode, vers uit
// de FSBL, geen stack; stores gaan met caches-uit rechtstreeks DRAM in.
//
// GEEN mhartid: beide cores lezen daar 0 (gemeten boot 5, 01-08 — zie
// hop/hart.go). De eerlijke discriminator is het reset-blok zelf: draait de
// C906L (bit 6 hoog), dan kan alleen ík dat zijn — het enige hart dat de
// FSBL start is de C906B, en die komt hier met de C906L nog in reset.
//
// Register-recept (vendor-FSBL, reset_c906l — zie hop/hart.go):
//	0x020B0020/24  boot-vector lo/hi van de C906L
//	0x020B0004     bit 13 = vector-override aan
//	0x03003024     bit 6  = C906L-reset (laag = assert, hoog = draait)

//go:build tamago

#include "textflag.h"

#define SCRATCH   0x8FE00000 // boot-scratch (hop/plan.go; init-check daar)
#define SECVECLO  0x020B0020
#define SECVECHI  0x020B0024
#define SECCTRL   0x020B0004
#define RSTN      0x03003024
#define RESCUETK  250000000  // zelfredding: 10s op de 25MHz-timebase

// func hartLottery()
TEXT ·hartLottery(SB), NOSPLIT|NOFRAME, $0
	MOV	·hopCoreMirror(SB), X5	// dé knop (layout.HopCore via DATA)
	BEQ	X5, ZERO, klaar		// 0: HOP woont op de firmware-core

	MOV	$RSTN, X10
	MOVW	(X10), X11
	AND	$(1<<6), X11, X11
	BEQ	X11, ZERO, wissel	// C906L in reset → ik ben de C906B

	// De C906L draait en dit image draait erop: dat ben ik. Levensteken
	// voor de geparkeerde buurman, en doorbooten als HOP.
	MOV	$SCRATCH, X10
	MOV	$1, X12
	MOV	X12, 88(X10)		// +88 LotteryHopAlive
	FENCE
	JMP	klaar

wissel:
	// Ik ben de firmware-core en HOP hoort op de C906L: start hem op onze
	// eigen entry en parkeer mezelf tot adoptie als app-hart.
	MOV	$_rt0_riscv64_tamago(SB), X7

	MOV	$SECVECLO, X10		// vector = onze entry
	MOVW	X7, (X10)
	SRL	$32, X7, X12
	MOV	$SECVECHI, X10
	MOVW	X12, (X10)
	MOV	$SECCTRL, X10		// override-bit aan
	MOVW	(X10), X11
	OR	$(1<<13), X11, X11
	MOVW	X11, (X10)
	FENCE

	MOV	$SCRATCH, X10		// loterij-blok: geparkeerd, niets ontvangen
	MOV	$0, X12
	MOV	X12, 72(X10)		// +72 adoptie-PC leeg
	MOV	X12, 88(X10)		// +88 levensteken leeg
	MOV	$1, X12
	MOV	X12, 64(X10)		// +64 voortgang: geparkeerd
	FENCE

	MOV	$RSTN, X10		// deassert: de C906L boot dit image
	MOVW	(X10), X11
	OR	$(1<<6), X11, X11
	MOVW	X11, (X10)
	FENCE

	// Parkeren. Tot het levensteken er is loopt de zelfreddings-klok: komt
	// de C906L niet op, dan nemen we onze oude rol terug (doorbooten als
	// HOP) — een mislukte wissel is een console-regel, geen baksteen.
	// Geen wfi (wek-keten onbetrouwbaar, 01-08); spinnen kost hier niets.
	MOV	$SCRATCH, X10
	RDTIME	X13
	MOV	$RESCUETK, X14
	ADD	X14, X13, X13		// deadline = nu + 10s
wacht:
	MOV	72(X10), X12		// adoptie-PC?
	BNE	X12, ZERO, adoptie
	MOV	88(X10), X11		// levensteken?
	BNE	X11, ZERO, geparkeerd
	RDTIME	X14
	BLT	X14, X13, wacht
	// Zelfredding: HopHart kwam nooit op. C906L terug in reset, rol terug.
	MOV	$RSTN, X10
	MOVW	(X10), X11
	AND	$~(1<<6), X11, X11
	MOVW	X11, (X10)
	FENCE
	MOV	$SCRATCH, X10
	MOV	$2, X12
	MOV	X12, 64(X10)		// +64 voortgang: 2 = zelfredding
	FENCE
	JMP	klaar

geparkeerd:
	// HOP leeft op zijn nieuwe hart; nu onbegrensd wachten op werk.
	MOV	72(X10), X12
	BEQ	X12, ZERO, geparkeerd
adoptie:
	MOV	$1, X11			// LotteryAbort → toch terug als HOP
	BEQ	X12, X11, klaar
	JMP	(X12)			// geadopteerd: trampoline-entry in

klaar:
	RET
