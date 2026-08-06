// EL2-capabele CPU-init voor de RK3566-probe — kloon van board/qemuvirt/
// cpuinit.s (zie daar voor het volledige verhaal), met twee verschillen:
//
//   - de scratch-adressen liggen bínnen het eigen RAM-venster (zie rk3566.go:
//     laag DRAM op dit bord kan TrustZone-gefirewalld zijn, en een scratch die
//     faultt sterft vóór de eerste UART-byte);
//   - VBAR_EL2 van core 0 → REVOKE_VEC, waar stage2.InitVectors na de boot de
//     HVC-revoke-handler in plugt (de hard-kill van een kooi). Moet byte-gelijk
//     zijn aan revokeVecPA in plan.go; die pariteit wordt bij SetupPlan
//     gecheckt.
//
// U-Boot's `booti` levert af op EL2 met x0 = DTB — dezelfde conventie als
// QEMU-virt en de Pi-armstub, dus dezelfde vorm.

//go:build linkcpuinit

#include "textflag.h"

#define BOOT_SCRATCH 0x0220F000	// = rk3566.BootScratch (pariteit: rk3566.go)
#define DTB_PTR      0x0220F008	// = rk3566.DTBPtr
#define REVOKE_VEC   0x062F0000	// = revokeVecPA (pariteit: plan.go, gecheckt)

TEXT cpuinit(SB),NOSPLIT|NOFRAME,$0
	MOVD	R0, R9		// x0 = DTB-pointer bij firmware-boot; bewaren vóór clobber
	MRS	CurrentEL, R0
	LSR	$2, R0, R0
	AND	$0b11, R0, R0

	CMP	$2, R0
	BEQ	el2
	CMP	$3, R0
	BEQ	el3
	// EL1-boot: scratch blijft 0 ⇒ de main meldt BootEL < 2 (de probe draait
	// door — meten mag; de agent-main weigert straks, HopOS eist EL2).
	B	·cpuinitEL1(SB)

el2:
	// boot-EL + DTB-pointer naar de scratch (MMU is uit, dit is DRAM).
	MOVD	$BOOT_SCRATCH, R1
	MOVD	R0, (R1)
	MOVD	$DTB_PTR, R1
	MOVD	R9, (R1)

	// VBAR_EL2 van de HOP-core → de revoke-vectoren (stage2.InitVectors vult ze
	// na boot; de hard-kill-HVC uit stage2.Revoke landt daar en doet TLBI
	// ALLE1IS). Alleen core 0 komt op dit el2-pad; app-cores entreren op EL1 via
	// de trampoline en zetten hun eigen VBAR_EL2 op Stage2Base.
	MOVD	$REVOKE_VEC, R0
	WORD	$0xd51cc000	// msr vbar_el2, x0

	// HCR_EL2: RW(31)=1 — EL1 draait AArch64. Stage-2 (VM-bit) blijft uit;
	// de app-core-variant zet hier VTTBR_EL2 + VM.
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
	// EL3-pad, één-op-één uit tamago's init.s (volledigheid; TF-A levert EL2).
	MOVD	$BOOT_SCRATCH, R1
	MOVD	R0, (R1)
	MOVD	$DTB_PTR, R1
	MOVD	R9, (R1)
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
