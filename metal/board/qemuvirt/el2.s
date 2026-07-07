// EL2-trampoline voor stage-2-isolatie (fase 4.2). HOP geeft PSCI CPU_ON
// niet de app-entry maar dít symbool, met de slot-index in x0 (ctx). De
// app-image draait daardoor nooit op EL2: hier wordt de door HOP gebouwde
// stage-2-tabel (metal/stage2) geactiveerd en dan pas naar de app-entry op
// EL1 gedropt. Wat de app op EL1 ook mapt — de IPA→PA-vertaling laat alleen
// zijn eigen slot door; een greep buiten de kooi trapt naar de EL2-vectoren
// (VBAR_EL2 → Stage2Base) die de core via PSCI CPU_OFF uitzetten.
//
// EL2-sysregs via WORD-encodings (Go-assembler kent ze niet bij naam):
// MSR = 0xd5180000 | op1<<16 | CRn<<12 | CRm<<8 | op2<<5 | Rt.

//go:build tamago && arm64

#include "textflag.h"

#define CTRL_BASE    0xB0000000
#define STAGE2_BASE  0xB2000000

TEXT s2tramp(SB),NOSPLIT|NOFRAME,$0
	// EL2-vectoren: elke exception hier (m.n. stage-2-fault vanuit EL1)
	// parkeert/CPU_OFF't de core. Zie stage2.InitVectors.
	MOVD	$STAGE2_BASE, R1
	WORD	$0xd51cc001	// msr vbar_el2, x1

	// Ctrl-page van dit slot: L1-tabel + app-entry, door HOP klaargezet.
	MOVD	$CTRL_BASE, R1
	ADD	R0<<12, R1, R1
	MOVD	0x38(R1), R2	// layout.CtrlS2Table
	MOVD	0x30(R1), R3	// layout.CtrlEntry

	// VTCR_EL2: RES1(31) | SH0=inner | ORGN0/IRGN0=WB | SL0=level1 | T0SZ=32
	// → 4KB-granule, 32-bit IPA-ruimte.
	MOVD	$0x80003560, R4
	WORD	$0xd51c2144	// msr vtcr_el2, x4

	// VTTBR_EL2 = L1-tabel | VMID(slot)<<48; oude vertalingen vegen.
	LSL	$48, R0, R5
	ORR	R2, R5, R5
	WORD	$0xd51c2105	// msr vttbr_el2, x5
	WORD	$0xd50c87df	// tlbi vmalls12e1
	DSB	$15

	// HCR_EL2: RW (EL1=AArch64) | VM (stage-2 aan voor EL1&0) | IMO
	// (fysieke IRQ's — de hard-kill-SGI — naar EL2; de app kan ze op EL1
	// niet maskeren en ziet onder IMO alleen de virtuele ICC-registers).
	MOVD	$1<<31, R4
	ORR	$1<<4, R4, R4
	ORR	$1, R4, R4
	WORD	$0xd51c1104	// msr hcr_el2, x4

	// Fysieke GIC-interface van deze core open voor de kill-SGI: PMR laat
	// alles door, groep 1 aan. Op EL2 zijn dit de fysieke registers.
	MOVD	$0xff, R4
	WORD	$0xd5184604	// msr icc_pmr_el1, x4
	MOVD	$1, R4
	WORD	$0xd518cce4	// msr icc_igrpen1_el1, x4

	// Timers vrij voor EL1 (zelfde als cpuinit-EL2-pad).
	WORD	$0xd53ce104	// mrs x4, cnthctl_el2
	ORR	$0b11, R4, R4
	WORD	$0xd51ce104	// msr cnthctl_el2, x4
	MOVD	$0, R4
	WORD	$0xd51ce064	// msr cntvoff_el2, x4

	// Drop naar EL1 op de app-entry (EL1h, DAIF gemaskeerd).
	MOVD	$0, R4
	ORR	$0b1111<<6, R4
	ORR	$0b0101<<0, R4
	WORD	$0xd51c4004	// msr spsr_el2, x4
	WORD	$0xd51c4023	// msr elr_el2, x3
	ISB	$15
	ERET

// S2TrampPC geeft het fysieke adres van de trampoline (HOP-image is
// identity-geladen: symbooladres = fysiek adres) voor PSCI CPU_ON.
TEXT ·S2TrampPC(SB),NOSPLIT,$0-8
	MOVD	$s2tramp(SB), R0
	MOVD	R0, ret+0(FP)
	RET
