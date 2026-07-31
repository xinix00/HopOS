// CPU-init van het generieke app-board (bouw met -tags linkcpuinit, zoals
// alle images). Het kanonieke pad is EL1: HOP's EL2-trampoline (cpu/el2,
// s2tramp) heeft stage-2, timers en een schone SCTLR al geregeld en ERET
// hierheen — dan rest alleen: SCTLR opschonen, stack uit de (door HOP
// gepatchte) RAM-declaratie, en door naar de tamago-runtime. Géén MMIO, géén
// scratch, géén firmware: een gekooide core mag dat allemaal niet aanraken —
// en heeft het ook niet nodig. Dit is de raspi-cpuinitEL1App-tak, nu
// board-onafhankelijk.
//
// EL2 is het dev-pad (QEMU -kernel app.elf, buiten HopOS om): een minimale
// drop naar EL1 — RW, timers vrij — zonder de boot-scratch-writes van de
// echte boards (een app-image heeft geen scratch-contract). EL3 wordt niet
// ondersteund: parkeren.
//
// EL2-systeemregisters via WORD-encodings (zelfde stijl als cpu/el2/el2.s):
// MSR S3_op1_Cn_Cm_op2 = 0xd5180000 | op1<<16 | CRn<<12 | CRm<<8 | op2<<5 | Rt.

//go:build linkcpuinit

#include "textflag.h"

TEXT cpuinit(SB),NOSPLIT|NOFRAME,$0
	MRS	CurrentEL, R0
	LSR	$2, R0, R0
	AND	$0b11, R0, R0

	CMP	$2, R0
	BEQ	el2
	CMP	$3, R0
	BEQ	el3
	B	·cpuinitEL1(SB)

el2:
	// HCR_EL2: RW(31)=1 — EL1 draait AArch64.
	MOVD	$1<<31, R0
	WORD	$0xd51c1100	// msr hcr_el2, x0

	// CNTHCTL_EL2: EL1PCTEN|EL1PCEN — timer/counter niet trappen voor EL1.
	WORD	$0xd53ce100	// mrs x0, cnthctl_el2
	ORR	$0b11, R0, R0
	WORD	$0xd51ce100	// msr cnthctl_el2, x0
	MOVD	$0, R0
	WORD	$0xd51ce060	// msr cntvoff_el2, x0

	// SPSR_EL2: EL1h, DAIF gemaskeerd.
	MOVD	$0, R0
	ORR	$0b1111<<6, R0
	ORR	$0b0101<<0, R0
	WORD	$0xd51c4000	// msr spsr_el2, x0

	MOVD	$·cpuinitEL1(SB), R0
	WORD	$0xd51c4020	// msr elr_el2, x0
	ISB	$15
	ERET

el3:
	// Geen EL3-pad: een app-image hoort nooit op EL3 af te trappen.
	WFE
	B	el3

TEXT ·cpuinitEL1(SB),NOSPLIT|NOFRAME,$0
	// SCTLR_EL1: alignment-check en MMU uit (tamago zet ze zelf weer aan).
	MRS	SCTLR_EL1, R0
	BIC	$1<<1, R0
	BIC	$1<<0, R0
	MSR	R0, SCTLR_EL1
	ISB	$15

	// Stack aan het einde van de eigen RAM-declaratie.
	MOVD	runtime∕goos·RamStart(SB), R1
	MOVD	R1, RSP
	MOVD	runtime∕goos·RamSize(SB), R1
	MOVD	runtime∕goos·RamStackOffset(SB), R2
	ADD	R1, RSP
	SUB	R2, RSP

	B	_rt0_tamago_start(SB)
