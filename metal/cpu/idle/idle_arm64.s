// WFE + CNTKCTL_EL1 (event-stream-configuratie). Encodings via WORD, zoals
// overal in dit repo: MSR = 0xd5180000 | op1<<16 | CRn<<12 | CRm<<8 | op2<<5 | Rt.

//go:build tamago && arm64

#include "textflag.h"

// wfeIdle: counterstand om de WFE heen — de retourwaarde is de écht in WFE
// doorgebrachte tijd in generic-timer-ticks. CNTVCT_EL0 (virtueel: trapt
// nooit op EL1, telt door in WFE).
TEXT ·wfeIdle(SB),NOSPLIT,$0-8
	WORD	$0xd53be040	// mrs x0, cntvct_el0
	WFE
	WORD	$0xd53be041	// mrs x1, cntvct_el0
	SUB	R0, R1, R0
	MOVD	R0, ret+0(FP)
	RET

// hvcYield: coöperatieve yield op een gedeelde core (fase 6). De governor
// roept dit i.p.v. WFE als HOP de core als gedeeld markeerde (CtrlShared).
// HVC #1 trapt naar de EL2-switch (cpu/el2/switch.s): die slaat onze GP- en
// sysreg-staat op, laat de core slapen, geeft mede-bewoners hun beurt en
// hervat ons hier. Anders dan WFE trapt HVC OOK op QEMU-TCG (waar WFE een
// no-op is) — dus deze weg is op QEMU testbaar, precies wat de bring-up eist.
//
// FP bewaren we ZELF, hier op de EL1-stack (Normal cacheable), NIET in de
// EL2-switch: EL2 draait met MMU uit → Device-geheugen → een SIMD/FP-store
// naar Device faultt op ijzer (QEMU verhult dat). De yield is een gewone
// functie-aanroep, dus alleen de callee-saved V8–V15 (+ FPCR) moeten de
// wissel overleven; de mede-bewoner die tussendoor draait klobbert ze. x0
// (counterstand vóór de yield) overleeft via de GP-save in de switch; de
// retourwaarde is de idle-wall-tijd (co-resident-runtijd + slaap) in ticks.
TEXT ·hvcYield(SB),NOSPLIT,$144-8
	FMOVQ	F8, 0(RSP)
	FMOVQ	F9, 16(RSP)
	FMOVQ	F10, 32(RSP)
	FMOVQ	F11, 48(RSP)
	FMOVQ	F12, 64(RSP)
	FMOVQ	F13, 80(RSP)
	FMOVQ	F14, 96(RSP)
	FMOVQ	F15, 112(RSP)
	WORD	$0xd53b4401	// mrs x1, fpcr
	MOVD	R1, 128(RSP)
	WORD	$0xd53be040	// mrs x0, cntvct_el0
	WORD	$0xd4000022	// hvc #1
	WORD	$0xd53be041	// mrs x1, cntvct_el0
	SUB	R0, R1, R0
	MOVD	128(RSP), R1
	WORD	$0xd51b4401	// msr fpcr, x1
	FMOVQ	0(RSP), F8
	FMOVQ	16(RSP), F9
	FMOVQ	32(RSP), F10
	FMOVQ	48(RSP), F11
	FMOVQ	64(RSP), F12
	FMOVQ	80(RSP), F13
	FMOVQ	96(RSP), F14
	FMOVQ	112(RSP), F15
	MOVD	R0, ret+0(FP)
	RET

TEXT ·cntkctlSet(SB),NOSPLIT,$0-8
	MOVD	v+0(FP), R0
	WORD	$0xd518e100	// msr cntkctl_el1, x0
	ISB	$15
	RET

TEXT ·cntfrq(SB),NOSPLIT,$0-8
	WORD	$0xd53be000	// mrs x0, cntfrq_el0
	MOVD	R0, ret+0(FP)
	RET
