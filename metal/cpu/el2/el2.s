// EL2-trampoline voor stage-2-isolatie (fase 4.2). HOP geeft PSCI CPU_ON
// niet de app-entry maar dít symbool, met het FYSIEKE adres van de
// control-page van het slot in x0 (ctx). De app-image draait daardoor nooit
// op EL2: hier wordt de door HOP gebouwde stage-2-tabel (metal/kern/stage2)
// geactiveerd en dan pas naar de app-entry op EL1 gedropt. Wat de app op EL1
// ook mapt — de IPA→PA-vertaling laat alleen zijn eigen partitie door; een
// greep buiten de kooi trapt naar de EL2-vectoren die de core via PSCI
// CPU_OFF uitzetten.
//
// Volledig data-gedreven: álle adressen (vectoren, stage-2-tabel, entry,
// VMID) komen van de control-page die HOP bij Start vulde — geen #defines,
// dus board-neutraal óók onder een per-board PA-plan. De offsets (layout.
// Ctrl*) staan hieronder als literals; layout.go benoemt die koppeling.
//
// EL2-sysregs via WORD-encodings (Go-assembler kent ze niet bij naam):
// MSR = 0xd5180000 | op1<<16 | CRn<<12 | CRm<<8 | op2<<5 | Rt.

//go:build tamago && arm64

#include "textflag.h"

TEXT s2tramp(SB),NOSPLIT|NOFRAME,$0
	// x0 = fysieke control-page van dit slot (PSCI ctx, door HOP gezet).
	// EL2-vectoren eerst: elke exception hierna (m.n. een stage-2-fault
	// vanuit EL1) rapporteert en CPU_OFF't. Zie stage2.InitVectors.
	MOVD	0x50(R0), R1	// layout.CtrlVecPA
	WORD	$0xd51cc001	// msr vbar_el2, x1

	MOVD	0x38(R0), R2	// layout.CtrlS2Table → stage-2 L1 (fysiek)
	MOVD	0x30(R0), R3	// layout.CtrlEntry   → app-entry (IPA)
	MOVD	0xB8(R0), R6	// layout.CtrlSlot    → VMID

	// TPIDR_EL2 = fysieke parkeer-mailbox van deze core: de parkeerlus
	// (ParkCodePA) vindt 'm daar terug zonder MPIDR. Overleeft de EL1-app
	// (TPIDR_EL2 is EL2-only). Idempotent bij herdispatch.
	MOVD	0xC8(R0), R7	// layout.CtrlMboxPA
	WORD	$0xd51cd047	// msr tpidr_el2, x7

	// SP_EL2 = de sched-scratch van deze core (mailbox + layout.SchedScratch):
	// de vector-thunks en de EL2-switch (switch.s) parkeren daar registers
	// vóórdat ze ook maar iets anders aanraken. Idempotent bij herdispatch.
	ADD	$16, R7, R8
	MOVD	R8, RSP

	// VTCR_EL2: 4KB-granule, 32-bit IPA, PS = min(PARange, 44-bit). De pool
	// ligt op servers vér boven de oude 40-bit/1TB-aanname (Altra: het
	// bulk-DRAM huist in dezelfde hoge regionen als de 16TB-UART — gemeten
	// 15-07: met PS=40 stierf elke loader op een address-size-fault bij zijn
	// eerste instructie uit een hoge partitie). Klemmen op het silicium is
	// verplicht: PS bóven PARange is constrained unpredictable (Pi's
	// A72/A76 melden 40-bit). 44-bit dekt 16TB — ruim boven elk DRAM-plan.
	WORD	$0xd5380705	// mrs x5, id_aa64mmfr0_el1
	AND	$0xF, R5
	CMP	$4, R5
	BLT	vtcrps		// PARange < 44-bit: het silicium-maximum
	MOVD	$4, R5		// anders klemmen op 44-bit (16TB)
vtcrps:
	MOVD	$0x80003560, R4	// VTCR zonder PS-veld
	ORR	R5<<16, R4, R4
	WORD	$0xd51c2144	// msr vtcr_el2, x4

	// VTTBR_EL2 = L1-tabel | VMID(slot)<<48; oude vertalingen vegen.
	LSL	$48, R6, R5
	ORR	R2, R5, R5
	WORD	$0xd51c2105	// msr vttbr_el2, x5
	WORD	$0xd50c87df	// tlbi vmalls12e1
	DSB	$15

	// HCR_EL2: RW(31, EL1=AArch64) | TSC(19, trap SMC) | VM(0, stage-2 aan
	// voor EL1&0). TSC maakt "een app praat nooit met de firmware" hard: er
	// bestáát geen legitieme app-SMC (zelfs SMP-bring-up loopt via HOP,
	// CtrlSMPReq; exit is een HVC) — een SMC uit de kooi is dus per definitie
	// een ontsnappingspoging en landt als EC=0x17 op de EL2-vectoren, net als
	// een stage-2-fault. GEEN TWE: de coöperatieve core-deling (switch.s)
	// wisselt op een EXPLICIETE HVC-yield van de idle-governor, niet op een
	// getrapte WFE — dat laatste is op QEMU-TCG een no-op en dus onbewijsbaar,
	// en een WFE-trap-heisenbug op ijzer. WFE blijft puur power (dedicated
	// core slaapt lokaal). Geen IMO(4): de hard-kill loopt niet via een IRQ
	// maar via stage-2-intrekking (HOP nult de map + TLBI → deze core faultt
	// synchroon naar de vectoren).
	MOVD	$1<<31, R4
	ORR	$1<<19, R4, R4
	ORR	$1, R4, R4
	WORD	$0xd51c1104	// msr hcr_el2, x4

	// Timers vrij voor EL1 (zelfde als cpuinit-EL2-pad).
	WORD	$0xd53ce104	// mrs x4, cnthctl_el2
	ORR	$0b11, R4, R4
	WORD	$0xd51ce104	// msr cnthctl_el2, x4
	MOVD	$0, R4
	WORD	$0xd51ce064	// msr cntvoff_el2, x4

	// EL1-staat NIET erven — de silicium-les (Pi 5, 2026-07-10): bij een
	// warme CPU_ON (core was eerder van een ándere app en deed CPU_OFF)
	// initieert TF-A alleen EL2; EL1 is dan wat de vorige huurder achterliet
	// — MMU áán, oude TTBR/VBAR — en de allereerste EL1-fetch na de ERET zou
	// door stale tabellen vertalen. QEMU verhulde dit (volledige vCPU-reset
	// bij CPU_ON). Dus expliciet, zoals cpuinit dat voor de primary doet:
	// SCTLR_EL1 = 0x30d00800 (RES1-bits; M/C/I/A/WXN uit) en CPTR_EL2 =
	// 0x33FF (TFP=0 — anders trapt tamago's eerste FP-instructie naar EL2).
	MOVD	$0x30d00800, R4
	MSR	R4, SCTLR_EL1
	MOVD	$0x33FF, R4
	WORD	$0xd51c1144	// msr cptr_el2, x4

	// I-cache van déze core leeg vóór de drop. Elke app linkt op hetzelfde
	// canonieke adres, dus bij een WARME herdispatch (park → mailbox → hier)
	// houdt de PIPT-I$ nog de INSTRUCTIES VAN DE VORIGE HUURDER voor exact
	// deze partitie-PA's vast — HOP's DC CIVAC bij het plaatsen raakt de
	// I-zijde nooit, en alleen een koude PSCI CPU_ON krijgt TF-A's
	// cache-hygiëne. Zonder deze regel executeert de nieuwe app de code van
	// de oude (Altra-vondst 15-07: eerste huurder leeft, élke herdispatch
	// dood bij boot; QEMU-TCG verhult dit — geen I$-model). Slot = core,
	// dus lokaal (IALLU) is compleet.
	WORD	$0xd508751f	// ic iallu
	DSB	$15
	ISB	$15

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
