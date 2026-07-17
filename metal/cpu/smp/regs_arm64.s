// EL1-registerreads voor de SMP-handoff (smp.go/node.go): de secundaire core
// — app-SMP of node — erft de ÁCTIEVE adreswereld {ttbr0, tcr, mair, vbar}
// van zijn dispatchende primaire, gelezen van de levende registers i.p.v.
// aangenomen. Voor apps zijn dat tamago's InitMMU-waarden (identiek aan de
// oude hardcoded stub-constanten); voor de node kan het mmu48's 48-bit-wereld
// zijn (extendVA — de Altra-UART en SBSA-watchdog wonen op 16TB, onvertaalbaar
// in de 39-bit-default: fault → watchdog-reset, gemeten 17-07 via de kabel).
// MRS-encodings als WORD, zoals overal (de Go-assembler kent ze deels niet).

//go:build tamago && arm64

#include "textflag.h"

TEXT ·readTTBR0(SB),NOSPLIT,$0-8
	WORD	$0xd5382000	// mrs x0, ttbr0_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·readTCR(SB),NOSPLIT,$0-8
	WORD	$0xd5382040	// mrs x0, tcr_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·readMAIR(SB),NOSPLIT,$0-8
	WORD	$0xd538a200	// mrs x0, mair_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·readVBAR(SB),NOSPLIT,$0-8
	WORD	$0xd538c000	// mrs x0, vbar_el1
	MOVD	R0, ret+0(FP)
	RET
