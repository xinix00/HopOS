// EL2-capabele CPU-init — vervangt tamago's cpuinit (bouw met
// -tags linkcpuinit). tamago's eigen init.s kent alleen EL3→EL1 en EL1,
// maar onze firmware levert EL2 af: QEMU -M virt,virtualization=on, TF-A als
// armstub8 op de Pi 5, en TF-A op de Orion O6N. Bij EL2:
//
//   - EL1 op AArch64 (HCR_EL2.RW), fysieke timer/counter vrij voor EL1
//     (CNTHCTL_EL2), CNTVOFF op 0;
//   - het boot-EL naar de scratch (layout.BootScratch) voor de
//     PSCI-conduitkeuze (EL2-boot ⇒ SMC, EL1-boot ⇒ HVC);
//   - drop naar EL1 en door naar de tamago-runtime.
//
// Dit is tevens de plek waar fase-4-isolatie aanhaakt: op app-cores
// programmeert dit pad straks stage-2 (VTTBR_EL2/HCR_EL2.VM) vóór de drop,
// zodat de app op EL1 alleen zijn eigen slot-partitie kan zien.
//
// EL2-systeemregisters via WORD-encodings (zelfde stijl als tamago's init.s):
// MSR S3_op1_Cn_Cm_op2 = 0xd5180000 | op1<<16 | CRn<<12 | CRm<<8 | op2<<5 | Rt.

//go:build linkcpuinit

#include "textflag.h"

#define BOOT_SCRATCH 0xB0000000

TEXT cpuinit(SB),NOSPLIT|NOFRAME,$0
	MRS	CurrentEL, R0
	LSR	$2, R0, R0
	AND	$0b11, R0, R0

	CMP	$2, R0
	BEQ	el2
	CMP	$3, R0
	BEQ	el3
	// EL1-boot: scratch blijft 0 ⇒ PSCI-conduit HVC. Níét schrijven — een
	// app-core onder stage-2 heeft de scratch read-only gemapt.
	B	·cpuinitEL1(SB)

el2:
	// boot-EL naar de scratch (alleen HOP-core komt hier; MMU uit).
	MOVD	$BOOT_SCRATCH, R1
	MOVD	R0, (R1)
	// HCR_EL2: RW(31)=1 — EL1 draait AArch64. Stage-2 (VM-bit) blijft uit;
	// de app-core-variant zet hier straks VTTBR_EL2 + VM.
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
	// EL3-pad, één-op-één uit tamago's init.s (volledigheid; onze doelen
	// leveren EL2 of EL1 af).
	MOVD	$BOOT_SCRATCH, R1
	MOVD	R0, (R1)
	MOVD	$0, R0
	ORR	$1<<10, R0	// lagere levels AArch64
	ORR	$1<<5, R0	// reserved
	ORR	$1<<4, R0	// reserved
	ORR	$1<<0, R0	// Non-secure
	WORD	$0xd51e1100	// msr scr_el3, x0

	MOVD	$1<<31, R0
	WORD	$0xd51c1100	// msr hcr_el2, x0

	MOVD	$0, R0
	ORR	$0b1111<<6, R0
	ORR	$0b0101<<0, R0
	WORD	$0xd51e4000	// msr spsr_el3, x0

	MOVD	$·cpuinitEL1(SB), R0
	WORD	$0xd51e4020	// msr elr_el3, x0
	ISB	$15
	ERET

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
