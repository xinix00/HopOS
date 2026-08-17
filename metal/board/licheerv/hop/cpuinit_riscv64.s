// CPU-init van de LicheeRV-KERN (bouw met -tags linkcpuinit): het allereerste
// dat draait, op élk hart dat dit image binnenkomt — vers uit de FSBL of vers
// uit onze eigen vector, M-mode, CACHES UIT, geen stack. Precies de staat
// waarvoor de loterij-asm geschreven is; de vorige plek (Init/Hwinit1, caches
// aan, cores niet coherent) was de stranding van 16-08 (ledger r.53).
//
// Twee taken, in deze volgorde:
//  1. de boot-hart-loterij (contract: licheerv/lottery.go) — GEEN mhartid
//     (beide cores lezen 0, gemeten 01-08); de discriminator is het
//     reset-blok: draait de C906L, dan bén ik hem;
//  2. tamago's default-init in M-mode: interrupts uit, FPU aan, stack uit de
//     RAM-declaratie, en door naar de runtime.
//
// Register-recept (vendor-FSBL, reset_c906l — zie hart.go):
//	0x020B0020/24  boot-vector lo/hi van de C906L
//	0x020B0004     bit 13 = vector-override aan
//	0x03003024     bit 6  = C906L-reset (laag = assert, hoog = draait)

//go:build linkcpuinit

#include "textflag.h"

#define SCRATCH   0x8FE00000 // boot-scratch (plan.go; spiegel-test bewaakt)
#define SECVECLO  0x020B0020
#define SECVECHI  0x020B0024
#define SECCTRL   0x020B0004
#define RSTN      0x03003024
#define RESCUETK  250000000  // zelfredding: 10s op de 25MHz-timebase

TEXT cpuinit(SB),NOSPLIT|NOFRAME,$0
	// ---- 1. de loterij ----
	MOV	·hopCoreMirror(SB), X5	// dé knop (licheerv.HopHart via DATA)
	BEQ	X5, ZERO, initcpu	// 0: HOP woont op de firmware-core

	MOV	$RSTN, X10
	MOVW	(X10), X11
	AND	$(1<<6), X11, X11
	BEQ	X11, ZERO, wissel	// C906L in reset → ik ben de C906B

	// De C906L draait en dit image draait erop: dat ben ik. Levensteken
	// (caches uit: de store gaat rechtstreeks DRAM in) en doorbooten als HOP.
	MOV	$SCRATCH, X10
	MOV	$1, X12
	MOV	X12, 88(X10)		// +88 LotteryHopAlive
	FENCE
	JMP	initcpu

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

	MOV	$SCRATCH, X10		// loterij-blok schoon: nog niets ontvangen
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

	// Parkeren. Tot het levensteken er is loopt de zelfreddings-klok: de
	// C906L bereikt zíjn cpuinit binnen microseconden, dus 10s stilte =
	// echt dood → C906L terug in reset en zelf doorbooten als HOP. Niets
	// is dan nog aangeraakt (we zijn vóór álles), dus de oude wereld is
	// gewoon schoon. Geen wfi (wek-keten, 01-08); spinnen kost hier niets.
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
	MOV	$RSTN, X10		// zelfredding
	MOVW	(X10), X11
	AND	$~(1<<6), X11, X11
	MOVW	X11, (X10)
	FENCE
	MOV	$SCRATCH, X10
	MOV	$2, X12
	MOV	X12, 64(X10)		// +64 voortgang: 2 = zelfredding
	FENCE
	JMP	initcpu

geparkeerd:
	MOV	72(X10), X12		// onbegrensd wachten op werk
	BEQ	X12, ZERO, geparkeerd
adoptie:
	MOV	$1, X11			// LotteryAbort → toch terug als HOP
	BEQ	X12, X11, initcpu
	JMP	(X12)			// geadopteerd: trampoline-entry in

	// ---- 2. tamago's default-init, M-mode ----
initcpu:
	MOV	$0, T0
	WORD	$0x30401073		// csrw mie, x0 — interrupts uit
	MOV	$(1<<13), T0
	WORD	$0x3002a073		// csrrs x0, mstatus, t0 — FPU aan (FS)

	MOV	runtime∕goos·RamStart(SB), X2
	MOV	runtime∕goos·RamSize(SB), T1
	MOV	runtime∕goos·RamStackOffset(SB), T2
	ADD	T1, X2
	SUB	T2, X2

	JMP	_rt0_tamago_start(SB)
