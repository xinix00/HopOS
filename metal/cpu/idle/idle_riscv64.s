// De coöperatieve yield op een gedeeld hart — de RISC-V-tegenhanger van de
// hvcYield in idle_arm64.s, en met dezelfde rolverdeling.
//
// `ecall` uit supervisor-modus trapt naar HOP's M-mode-switcher (cpu/mmode):
// die bewaart onze GPR- en sysreg-staat, geeft de mede-bewoner van dit hart zijn
// beurt en hervat ons hier. Alle integer-registers overleven de wissel — de
// switcher bewaart x1..x31 — dus de tellerstand vóór de yield mag gewoon in een
// register blijven staan.
//
// Er is géén argument dat "dit is een yield" zegt, anders dan de `hvc #1` op ARM:
// op bare metal bestaat er geen tweede ecall-gebruiker (tamago heeft geen
// syscalls), dus mcause 9 ÍS de yield. Zou dat ooit veranderen, dan hoort er een
// nummer in a7 en een check in de switcher.
//
// FP bewaren we ZELF, hier op de S-mode-stack, en niet in de switcher — exact de
// afweging van de ARM-kant. De yield is een gewone functie-aanroep, dus alleen de
// callee-saved fs0..fs11 (+ fcsr) moeten de wissel overleven; de buur die
// tussendoor draait klobbert ze. De caller-saved FP-registers houden wél residu
// van de yielder vast dat de buur kan zien — dezelfde eigenschap als op ARM
// (cpu/el2/switch.s bewaart daar óók geen FP), en de reden dat een slot dat
// FP-geheimen heeft een eigen hart hoort te krijgen.

//go:build tamago && riscv64

#include "textflag.h"

// func ecallYield() uint64 — retourneert de verstreken rdtime-ticks: de
// wall-tijd waarin de buur draaide, oftewel de tijd waarin wíj niets deden.
//
// LET OP DE +8 IN ELKE OFFSET. Een frame-declaratie ($112-8) laat de assembler
// een proloog genereren die het RETOURADRES op 0(SP) legt:
//
//	MOV  X1, -120(SP)   ← proloog
//	ADDI $-120, SP, SP
//	MOV  X1, (SP)       ← ra staat hier
//
// Wie hier zelf op 0(SP) schrijft, overschrijft dus zijn eigen returnadres, en
// de epiloog springt naar wat daar per ongeluk staat. GEMETEN 31-07 (Dereks
// review, uit de disassembly): F8 landde op 0(SP), en de log liet exact de
// gevolgen zien — ra = 0xc2abe06dfff79ff3 (de bits van een float64) en een
// mcause-12 op datzelfde adres met bit 0 gewist door jalr. Beide bewoners
// kregen hetzelfde adres omdat ze bij de yield dezelfde F8-waarde opsloegen.
//
// Dat het alléén op een gedeeld hart gebeurde is geen toeval en wees drie boots
// lang de verkeerde kant op: zonder buurman wordt deze functie nooit aangeroepen.
TEXT ·ecallYield(SB),NOSPLIT,$112-8
	MOVD	F8, 8(SP)		// fs0
	MOVD	F9, 16(SP)		// fs1
	MOVD	F18, 24(SP)		// fs2
	MOVD	F19, 32(SP)		// fs3
	MOVD	F20, 40(SP)		// fs4
	MOVD	F21, 48(SP)		// fs5
	MOVD	F22, 56(SP)		// fs6
	MOVD	F23, 64(SP)		// fs7
	MOVD	F24, 72(SP)		// fs8
	MOVD	F25, 80(SP)		// fs9
	MOVD	F26, 88(SP)		// fs10
	MOVD	F27, 96(SP)		// fs11
	WORD	$0x003022f3		// csrr t0, fcsr
	MOV	T0, 104(SP)

	WORD	$0xc0102373		// csrr t1, time
	MOV	$0, X17			// a7 = 0: dit is een YIELD, geen exit. Expliciet
	// nullen is niet netjesheid maar noodzaak — a7 is caller-saved, dus wat de
	// aanroeper er liet staan zou anders de switcher een exit laten lezen en deze
	// bewoner dood melden.
	WORD	$0x00000073		// ecall — naar cpu/mmode
	WORD	$0xc0102573		// csrr a0, time
	SUB	T1, A0, A0		// a0 = na − vóór

	MOV	104(SP), T0
	WORD	$0x00329073		// csrw fcsr, t0
	MOVD	8(SP), F8
	MOVD	16(SP), F9
	MOVD	24(SP), F18
	MOVD	32(SP), F19
	MOVD	40(SP), F20
	MOVD	48(SP), F21
	MOVD	56(SP), F22
	MOVD	64(SP), F23
	MOVD	72(SP), F24
	MOVD	80(SP), F25
	MOVD	88(SP), F26
	MOVD	96(SP), F27
	MOV	A0, ret+0(FP)
	RET

// func exitTrap() — "ik ben klaar". Zelfde trap, ander nummer in a7: de switcher
// meldt deze bewoner dood en roteert weg, zodat het hart voor de buren doorloopt
// en HOP niets hoeft te resetten. Tegenhanger van hvc #0 in applib/park_arm64.s.
// Keert nooit terug.
TEXT ·exitTrap(SB),NOSPLIT,$0
	MOV	$1, X17			// a7 = 1: exit
	WORD	$0x00000073		// ecall
	RET				// onbereikbaar
