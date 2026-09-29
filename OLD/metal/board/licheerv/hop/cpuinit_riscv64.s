// CPU-init van de LicheeRV-KERN (bouw met -tags linkcpuinit): het allereerste
// dat draait, op élk hart dat dit image binnenkomt — vers uit de FSBL of vers
// uit onze eigen vector, M-mode, CACHES UIT, geen stack. Precies de staat
// waarvoor de loterij-asm geschreven is; de vorige plek (Init/Hwinit1, caches
// aan, cores niet coherent) was de stranding van 16-08 (ledger r.53).
//
// Twee taken, in deze volgorde:
//  1. de boot-hart-loterij (contract: licheerv/lottery.go). Discriminator:
//     GEEN mhartid (beide cores lezen 0, gemeten 01-08) en GEEN reset-bit
//     (de FSBL laat de C906L zelf draaiend achter — gemeten boot 5, 17-08:
//     de big hield zichzelf voor de kleine en er wisselde niets), maar de
//     VECTOR-OVERRIDE die alleen wíj zetten: staat bit 13 met onze eigen
//     entry in het vectorregister, dan ben ik de gestarte HopHart;
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
	// ---- 0. cache-discipline voor iedereen ----
	// De FSBL kan ons mét D-cache aan overdragen en de cores zijn niet
	// coherent: al het loterij-verkeer moet DRAM-echt zijn (boot 4 leerde
	// dat markers anders in de cache blijven hangen). Terugschrijven, uit,
	// en pas weer aan op elk pad dat als HOP doorboot.
	WORD	$0x0030000b		// th.dcache.ciall
	WORD	$0x01b0000b		// th.sync.is
	MOV	$(1<<1), T0
	WORD	$0x7c12b073		// csrrc x0, mhcr, t0 — D-cache uit

	// ---- 1. de loterij ----
	MOV	·hopCoreMirror(SB), X5	// dé knop (licheerv.HopHart via DATA)
	BEQ	X5, ZERO, alsHOP	// 0: HOP woont op de firmware-core

	// Ben ik de gestarte HopHart? Alleen wíj zetten de vector-override
	// (bit 13) mét onze eigen entry erin; de FSBL boot de big zonder.
	MOV	$SECCTRL, X10
	MOVW	(X10), X11
	AND	$(1<<13), X11, X11
	BEQ	X11, ZERO, wissel	// geen override: ik ben de firmware-core
	MOV	$SECVECLO, X10
	MOVWU	(X10), X11
	MOV	$_rt0_riscv64_tamago(SB), X7
	SLL	$32, X7, X12		// onderste 32 bits vergelijken
	SRL	$32, X12, X12
	BNE	X11, X12, wissel	// andermans vector: toch de firmware-core

	// Ik ben HopHart, gestart door de loterij: levensteken en door als HOP.
	MOV	$SCRATCH, X10
	MOV	$1, X12
	MOV	X12, 88(X10)		// +88 LotteryHopAlive
	FENCE
	JMP	alsHOP

wissel:
	// Ik ben de firmware-core en HOP hoort elders. Eerst de C906L HARD in
	// reset — de FSBL laat hem draaiend achter met eigen vendor-code, en
	// wat daar loopt mag nooit half meeschrijven aan onze wereld.
	MOV	$RSTN, X10
	MOVW	(X10), X11
	AND	$~(1<<6), X11, X11
	MOVW	X11, (X10)
	FENCE

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

	MOV	$SCRATCH, X10		// loterij-blok: schoon
	MOV	$0, X12
	MOV	X12, 72(X10)		// +72 adoptie-PC leeg
	MOV	X12, 88(X10)		// +88 levensteken leeg
	MOV	X12, 96(X10)		// +96 adoptie-arg leeg
	MOV	$1, X12
	MOV	X12, 64(X10)		// +64 voortgang: geparkeerd
	FENCE

	MOV	$RSTN, X10		// deassert: de C906L boot dit image
	MOVW	(X10), X11
	OR	$(1<<6), X11, X11
	MOVW	X11, (X10)
	FENCE

	// Parkeren (D-cache uit: élke read is DRAM-echt). Tot het levensteken
	// er is loopt de zelfreddings-klok — de C906L bereikt zijn cpuinit in
	// microseconden, dus 10s stilte = echt dood. Tussen de polls ~10k
	// timebase-tikken (400µs) pauze: cache-uit spinnen op DRAM naast HOP's
	// net-DMA op een fanless board is anders 100% duty voor niets; de
	// adoptie-latency blijft onder een halve milliseconde.
	MOV	$SCRATCH, X10
	RDTIME	X13
	MOV	$RESCUETK, X14
	ADD	X14, X13, X13
wacht:
	MOV	72(X10), X12
	BNE	X12, ZERO, adoptie
	MOV	88(X10), X11
	BNE	X11, ZERO, geparkeerd
	RDTIME	X14
	BGE	X14, X13, redding
	ADD	$10000, X14, X14	// poll-pauze
wpauze:
	RDTIME	X5
	BLT	X5, X14, wpauze
	JMP	wacht
redding:
	MOV	$RSTN, X10		// zelfredding: C906L terug in reset
	MOVW	(X10), X11
	AND	$~(1<<6), X11, X11
	MOVW	X11, (X10)
	FENCE
	MOV	$SCRATCH, X10
	MOV	$2, X12
	MOV	X12, 64(X10)		// +64 voortgang: 2 = zelfredding
	FENCE
	JMP	alsHOP

geparkeerd:
	MOV	72(X10), X12		// onbegrensd wachten op werk
	BNE	X12, ZERO, adoptie
	RDTIME	X5			// zelfde poll-pauze als in wacht
	ADD	$10000, X5, X5
gpauze:
	RDTIME	X6
	BLT	X6, X5, gpauze
	JMP	geparkeerd
adoptie:
	MOV	96(X10), X11		// adoptie-arg mee (LotteryParkArg: het
	// sched-blok voor cpu/mmode parkenter; een kooi-stub negeert X11)
	JMP	(X12)			// geadopteerd: entry in (cache uit = reset-staat)

alsHOP:
	// VOLLEDIGE cache/perf-init, niet alleen de D-cache-bit. Dit hart komt
	// kaal uit reset — de FSBL heeft hier nooit gedraaid — dus élke
	// niet-gezette bit blijft 0, en dat betekende: I-CACHE UIT. HopOS
	// draaide zo wekenlang met instructie-fetches uit DRAM (gemeten 18-08,
	// netmeter-A/B: driver 788µs/frame op de C906L tegen 9,3 op de C906B =
	// ~77×, waar de klok maar 1,43× verklaart; time.Now kostte 22,6µs).
	// Eerst álles invalideren (I+D+branch — invalideren schrijft niet terug,
	// maar de loterij-pass hierboven heeft D al geciall'd en uitgezet), dan
	// de regimes: mxstatus = de op de C906B GEMETEN FSBL-waarde (stub-print
	// 0xc0638000), mhcr/mhint = de vendor-initconstanten (T-Head/cv181x
	// 0x11ff en 0x16e30c). Ook op het HopCore=0-pad (de big) is de csrw
	// idempotent: daar staan deze waarden al.
	MOV	$0x70003, T0
	WORD	$0x7c229073		// csrw mcor, t0 — I+D+branch invalideren
	MOV	$0xc0638000, T0
	WORD	$0x7c029073		// csrw mxstatus, t0
	MOV	$0x16e30c, T0
	WORD	$0x7c529073		// csrw mhint, t0 — prefetch/perf (vendor-init)
	MOV	$0x11ff, T0
	WORD	$0x7c129073		// csrw mhcr, t0 — I$ + D$ + WA/WB/RS/BPE/BTB/WBR aan

	// ---- 2. tamago's default-init, M-mode ----
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
