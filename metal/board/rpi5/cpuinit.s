// EL2-capabele CPU-init voor de Pi 5 — vervangt tamago's cpuinit (bouw met
// -tags linkcpuinit). De firmware levert ons op EL2 af (TF-A/armstub op
// EL3); dit pad is de rpi5-variant van board/qemuvirt/cpuinit.s met twee
// verschillen:
//
//   - vóór álles gaan er twee levenstekens naar de debug-UART ("P" + het
//     boot-EL als cijfer): op bare metal zonder werkende boot is de UART het
//     enige debugkanaal, dus een zwart scherm ≠ nul informatie;
//   - de boot-EL-scratch is een RAM-adres onder de kernel (bootScratch in
//     rpi5.go), niet een device-page — de Pi heeft ons ctrl-page-plan nog niet.
//
// EL2-systeemregisters via WORD-encodings (zelfde stijl als tamago's init.s).

//go:build linkcpuinit

#include "textflag.h"

#define BOOT_SCRATCH 0x1FF000
#define DTB_PTR      0x1FF008
#define UART_DR 0x107d001000
#define UART_FR 0x107d001018
#define GIO_AON 0x107d517c00	// brcmstb-GIO bank 0: +0 ODEN, +4 DATA, +8 IODIR
#define LED_BIT $0x200		// ACT-LED = pin 9, active-low (DTB: led-act)

TEXT cpuinit(SB),NOSPLIT|NOFRAME,$0
	MOVD	R0, R9		// x0 = DTB-pointer bij firmware-boot; bewaren vóór clobber

	// Levensteken 0 — de ACT-LED, vóór álles: de UART hieronder kan zonder
	// JST-sessie dood zijn (ongeklokt → FR-poll hangt), de always-on-GPIO
	// niet. 3× traag knipperen = "onze eerste instructies draaien".
	// Delay = domme SUBS-lus (~0,1-0,5s per fase, frequentie-afhankelijk):
	// nul systeemregister-gokken in het blindste stuk van de boot.
	MOVD	$GIO_AON, R2
	MOVWU	0(R2), R3
	BIC	LED_BIT, R3
	MOVW	R3, 0(R2)	// ODEN: push-pull
	MOVWU	8(R2), R3
	BIC	LED_BIT, R3
	MOVW	R3, 8(R2)	// IODIR: output (1 = input)
	MOVD	$3, R4
blink:
	MOVWU	4(R2), R3
	BIC	LED_BIT, R3
	MOVW	R3, 4(R2)	// DATA bit 9 laag = LED aan
	MOVD	$0x400000, R5
ledon:
	SUBS	$1, R5
	BNE	ledon
	MOVWU	4(R2), R3
	ORR	LED_BIT, R3
	MOVW	R3, 4(R2)	// LED uit
	MOVD	$0x400000, R5
ledoff:
	SUBS	$1, R5
	BNE	ledoff
	SUBS	$1, R4
	BNE	blink

	// Levensteken 1: 'P' (Pi) — ALLEEN mét debug-sessie. Zonder sessie is de
	// PL011 mogelijk ongeklokt en kan zelfs de FR-read de bus laten stallen;
	// daar helpt geen poll-limiet tegen (de eerste read komt nooit terug).
	// Weghalen zodra de Debug Probe er is: verwijder de B hieronder.
	B	uartklaar

	MOVD	$UART_FR, R2
	MOVD	$100000, R4
wait1:
	SUBS	$1, R4
	BEQ	uartklaar	// FR.TXFF blijft vol → UART dood: overslaan
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
uartklaar:
	MRS	CurrentEL, R0	// (opnieuw: het UART-pad kan overgeslagen zijn)
	LSR	$2, R0, R0
	AND	$0b11, R0, R0

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
	// DTB-pointer opslaan (metal/fdt → board.MemTotal). Alleen de primary komt
	// op el2; app-cores entreren cpuinit op EL1 via de trampoline.
	MOVD	$DTB_PTR, R1
	MOVD	R9, (R1)
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
	// EL3-pad (volledigheid; de Pi-firmware levert EL2 af).
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
	// Levensteken 3: één korte LED-puls — de ERET naar het absolute
	// linkadres is gelukt (firmware laadde ons écht op het load-adres) en we
	// draaien op EL1. Daarna: 3 traag + 1 kort gezien = asm-keten compleet.
	MOVD	$GIO_AON, R2
	MOVWU	4(R2), R3
	BIC	LED_BIT, R3
	MOVW	R3, 4(R2)
	MOVD	$0x200000, R5
led1on:
	SUBS	$1, R5
	BNE	led1on
	MOVWU	4(R2), R3
	ORR	LED_BIT, R3
	MOVW	R3, 4(R2)

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
