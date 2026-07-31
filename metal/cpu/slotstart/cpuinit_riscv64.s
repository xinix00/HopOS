// CPU-init van het generieke app-board op RISC-V (bouw met -tags linkcpuinit,
// net als de arm64-helft in cpuinit_arm64.s). Dit is het eerste dat draait als
// de kooi-stub het hart aan de app geeft — vóór de Go-runtime, vóór welke
// Go-code dan ook.
//
// Waarom dit bestaat, en niet tamago's eigen versie: die schrijft mie en
// mstatus, en dat zijn M-mode-CSR's. Een app-slot draait in S-MODE (de
// kooi-stub mret't erheen zodra het hart supervisor-modus heeft), en daar is
// zo'n write een illegal instruction. GEMETEN op het bordje 31-07, en het is
// meteen de tweede instructie van het entry:
//
//	mcause 0x2 (illegal instruction)
//	mepc   0x881ac1ce → tamago/riscv64.Init+0x6
//	mtval  0x30429073 → csrw mie, t0
//
// De app stierf dus vóór zijn eerste logregel en vóór zijn eerste hartslag: HOP
// zag alleen status Booting en een slot dat nooit iets zei.
//
// Dit pad raakt daarom géén M-mode-CSR. Dat maakt het tegelijk mode-neutraal:
// sie en sstatus zijn vanuit M-mode óók schrijfbaar (sstatus is een venster op
// mstatus, dus FS landt op hetzelfde bit), dus hetzelfde image draait op een
// hart met én zonder supervisor-modus. HOP hoeft bij het plaatsen niets te
// weten en er is één artifact per architectuur.
//
// Wat de EL1-helft op ARM aan SCTLR doet, hoeft hier niet — maar niet omdat er
// geen MMU aan zou staan: de kooi-stub zet satp (Sv39) vóór hij ons binnenlaat,
// want de map-helft van de kooi is wat élk slot op hetzelfde linkadres legt. Wij
// hoeven er alleen niets AAN te doen: de tabel staat er al, de caches heeft de
// stub gezet, en dit pad raakt geen enkel vertaalregister.

//go:build linkcpuinit

#include "textflag.h"

TEXT cpuinit(SB),NOSPLIT|NOFRAME,$0
	// Interrupts uit — alleen de S-mode-kant. mie blijft met opzet onaangeroerd:
	// die is van HOP, en aanraken is precies de trap hierboven.
	MOV	$0, T0
	CSRRW	T0, SIE, ZERO

	// FPU aan: sstatus.FS = Initial (bit 13). De Go-runtime gebruikt
	// FP-registers, en met FS=Off is élke FP-instructie een illegal instruction.
	// Via sstatus i.p.v. mstatus: zelfde bit, wél toegestaan in S-mode.
	// csrrs x0, sstatus, t0 — de assembler kent SSTATUS niet als naam.
	MOV	$(1<<13), T0
	WORD	$0x1002a073

	// Stack aan het einde van de eigen RAM-declaratie (door HOP gepatcht).
	MOV	runtime∕goos·RamStart(SB), X2
	MOV	runtime∕goos·RamSize(SB), T1
	MOV	runtime∕goos·RamStackOffset(SB), T2
	ADD	T1, X2
	SUB	T2, X2

	JMP	_rt0_tamago_start(SB)
