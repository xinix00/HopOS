// boot.h — de ENE boot: `cpuinit` voor elk EL2-capabel board (docs/LAATSTE_PLAN.md).
//
// Zes boards schreven elk hun eigen cpuinit, en die waren op precies de
// pijnpunten uit elkaar gegroeid (zie drop.h). Wat een board écht eigen heeft
// zijn adressen en een paar haken; de vijftien instructies daartussen zijn
// overal dezelfde. Dus staan die hier één keer, en levert een board vóór de
// include alleen zijn defines:
//
//   #define BOOT_SCRATCH  <PA>   optioneel: +0 = boot-EL, +8 = x0 bij binnenkomst
//                                 (DTB, param-blok); de mains eisen EL ≥ 2 en
//                                 metal/fw/fdt leest +8. Een app-image (hopslot)
//                                 heeft er geen: een gekooide core schrijft niets.
//   #define TRAP_VEC      <PA>   optioneel: VBAR_EL2 van de boot-core
//                                 (= layout.TrapVecPA, pariteit in het board-init)
//   #define BOARD_EARLY   ...    haak: vóór alles, MMU uit, op het boot-EL
//                                 (Apple's hop en parkeren, de Pi-banner)
//   #define BOARD_EL2     ...    haak: op EL2, ná HCR en vóór de drop
//                                 (Pi 5's SMPEN, Apple's HCR/CNTHCTL-teruglezing)
//   #define BOARD_EL1     ...    haak: op EL1, vóór SCTLR en de stack
//                                 (Apple's vroege faultdumper)
//   #include "../../cpu/el2/boot.h"
//
// Elke haak heeft een lege default. Een board zonder eigenaardigheden is dus
// twee defines en een include; een app-image is alleen de include.
//
// Include-volgorde in het .s-bestand: textflag.h, ../../cpu/el2/sysreg.h,
// ../../cpu/el2/drop.h, ../../cpu/el2/boot.h. Go's asm-preprocessor lost
// #include op vanuit de package-map van het .s-bestand, niet vanuit het
// includerende .h — daarom includeert dit bestand niets zelf.
//
// Het pad:
//
//   cpuinit      x0 → x9 (bewaren vóór clobber); BOARD_EARLY; EL bepalen → x10.
//                EL1: geen scratch (die is onder stage-2 read-only), meteen de
//                landing — de main weigert dan (Privilege).
//   EL3          SCR_EL3 (NS, RW, HCE, RES1), ERET naar cpuinitEL2: een EL3-
//                aflevering gaat door EL2 zoals de rest, anders staan VBAR_EL2
//                en de timers nooit goed. Nog nooit gemeten: onze firmware's
//                (TF-A, U-Boot, UEFI, iBoot) leveren allemaal EL2 af.
//   cpuinitEL2   scratch (EL, x0), VBAR_EL2, HCR = RW (stage-2 uit: dit is de
//                HOP-core), BOARD_EL2, DROP_TO_EL1 (drop.h) naar cpuinitEL1.
//   cpuinitEL1   BOARD_EL1; SCTLR_EL1 zonder A en M (tamago zet ze zelf weer
//                aan); stack aan het einde van de eigen RAM-declaratie; rt0.
//
// Registers voor de haken: x9 = x0 bij binnenkomst en x10 = boot-EL leven tot
// de scratch geschreven is — BOARD_EARLY laat die twee met rust (x10 bestaat
// daar nog niet, x9 wel). BOARD_EL2 en BOARD_EL1 mogen alles klobberen; de
// drop leest zijn entry pas ná BOARD_EL2 in x3.
//
// EL2/EL3-sysregs via WORD-encodings (Go's assembler kent ze niet bij naam):
// MSR = 0xd5180000 | op1<<16 | CRn<<12 | CRm<<8 | op2<<5 | Rt.

// BOOT_ENTRY: de naam van de ingang. Default `cpuinit` (wat tamago's linker
// verwacht); een board met een loader ervóór (uefi: de firmware roept
// cpuinit als AAPCS-functie aan, en die verhuist het image eerst) geeft de
// boot een eigen naam en springt er zelf heen.
#ifndef BOOT_ENTRY
#define BOOT_ENTRY cpuinit
#endif
#ifndef BOARD_EARLY
#define BOARD_EARLY
#endif
#ifndef BOARD_EL2
#define BOARD_EL2
#endif
#ifndef BOARD_EL1
#define BOARD_EL1
#endif

TEXT BOOT_ENTRY(SB),NOSPLIT|NOFRAME,$0
	MOVD	R0, R9		// x0 bij binnenkomst: DTB of param-blok
	BOARD_EARLY
	MRS	CurrentEL, R10
	LSR	$2, R10, R10
	AND	$0b11, R10, R10
	CMP	$2, R10
	BNE	notel2
	B	·cpuinitEL2(SB)
notel2:
	CMP	$3, R10
	BEQ	el3
	B	·cpuinitEL1(SB)

el3:
	MOVD	$0, R0
	ORR	$1<<10, R0	// RW: lagere levels AArch64
	ORR	$1<<8, R0	// HCE: HVC toegestaan (de kooi-uitgang is een HVC)
	ORR	$1<<5, R0	// RES1
	ORR	$1<<4, R0	// RES1
	ORR	$1<<0, R0	// NS
	WORD	$0xd51e1100	// msr scr_el3, x0
	MOVD	$0x3c9, R0	// EL2h, DAIF gemaskeerd
	WORD	$0xd51e4000	// msr spsr_el3, x0
	MOVD	$·cpuinitEL2(SB), R0
	WORD	$0xd51e4020	// msr elr_el3, x0
	ISB	$15
	ERET

TEXT ·cpuinitEL2(SB),NOSPLIT|NOFRAME,$0
#ifdef BOOT_SCRATCH
	MOVD	$BOOT_SCRATCH, R1
	MOVD	R10, (R1)	// +0: boot-EL
	MOVD	R9, 8(R1)	// +8: x0 bij binnenkomst
#endif
#ifdef TRAP_VEC
	MOVD	$TRAP_VEC, R0
	WORD	$0xd51cc000	// msr vbar_el2, x0
#endif
	// HCR_EL2 = RW alleen: EL1 draait AArch64, stage-2 uit, niets getrapt.
	// De app-cores krijgen hun RW|TSC|VM|FMO van de trampoline (el2.s).
	MOVD	$1<<31, R0
	WORD	$0xd51c1100	// msr hcr_el2, x0
	BOARD_EL2
	MOVD	$·cpuinitEL1(SB), R3
	DROP_TO_EL1(3)

TEXT ·cpuinitEL1(SB),NOSPLIT|NOFRAME,$0
	BOARD_EL1
	// SCTLR_EL1: alignment-check en MMU uit (tamago zet ze zelf weer aan).
	// Van EL2 komend staat dit al zo (drop.h), bij een EL1-aflevering niet.
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
