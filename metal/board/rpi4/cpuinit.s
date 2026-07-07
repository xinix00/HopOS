// EL2-capabele CPU-init voor de Pi 4 — vervangt tamago's cpuinit (bouw met
// -tags linkcpuinit). Met TF-A als armstub (verplicht op dit board, zie
// docs/rpi4.md) levert de firmware ons op EL2 af; dit is de rpi4-variant
// van board/rpi5/cpuinit.s — identiek op de #defines na (PL011 op
// 0xFE201000 i.p.v. de Pi 5-debug-connector):
//
//   - vóór álles gaan er twee levenstekens naar de UART ("P" + het boot-EL
//     als cijfer): op bare metal zonder werkende boot is de UART het enige
//     debugkanaal, dus een zwart scherm ≠ nul informatie;
//   - de boot-EL-scratch is een RAM-adres onder de kernel (raspi.BootScratch;
//     kernel_address=0x200000 maakt het plan gelijk aan de Pi 5).
//
// EL2-systeemregisters via WORD-encodings (zelfde stijl als tamago's init.s).

//go:build linkcpuinit

#include "textflag.h"

#define BOOT_SCRATCH 0x1FF000
#define UART_DR 0xFE201000
#define UART_FR 0xFE201018

TEXT cpuinit(SB),NOSPLIT|NOFRAME,$0
	// Levensteken 1: 'P' (Pi) — de bootloader heeft de UART al geconfigureerd.
	MOVD	$UART_FR, R2
wait1:
	MOVWU	(R2), R3
	TBNZ	$5, R3, wait1	// FR.TXFF: FIFO vol → poll
	MOVD	$UART_DR, R2
	MOVD	$0x50, R3	// 'P'
	MOVW	R3, (R2)

	// Levensteken 2: het boot-EL als cijfer ('1'/'2'/'3').
	MRS	CurrentEL, R0
	LSR	$2, R0, R0
	AND	$0b11, R0, R0
	ADD	$0x30, R0, R3	// '0' + EL
	MOVW	R3, (R2)

	CMP	$2, R0
	BEQ	el2
	CMP	$3, R0
	BEQ	el3
	// EL1-boot (onverwacht op de Pi): scratch blijft 0 ⇒ conduit HVC.
	B	·cpuinitEL1(SB)

el2:
	// boot-EL naar de scratch (MMU uit; gewone DRAM-write).
	MOVD	$BOOT_SCRATCH, R1
	MOVD	R0, (R1)
	// HCR_EL2: RW(31)=1 — EL1 draait AArch64. Stage-2 (VM-bit) blijft uit;
	// fase P1 hangt hier de app-core-variant met VTTBR_EL2 + VM aan.
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
	// EL3-pad (volledigheid; TF-A levert EL2 af).
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
