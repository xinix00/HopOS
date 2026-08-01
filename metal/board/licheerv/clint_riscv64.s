// CSR-lezers voor de CLINT-probe. De Go-assembler kent geen csrr-mnemonic,
// dus WORD — zelfde vorm als time_riscv64.s (csrrs rd, csr, x0).

#include "textflag.h"

// func mhartid() uint64 — welke hart draait dit
TEXT ·mhartid(SB),NOSPLIT|NOFRAME,$0-8
	WORD	$0xf1402573	// csrr a0, mhartid
	MOV	A0, ret+0(FP)
	RET

// func mip() uint64 — pending machine interrupts (bit 3 = MSIP, bit 7 = MTIP)
TEXT ·mip(SB),NOSPLIT|NOFRAME,$0-8
	WORD	$0x34402573	// csrr a0, mip
	MOV	A0, ret+0(FP)
	RET

// func mie() uint64 — enabled machine interrupts. De probe kijkt hiernaar
// vóórdat hij een timer laat vuren: staat MTIE/MSIE aan, dan zou het vuren een
// trap worden in plaats van alleen een pending-bit.
TEXT ·mie(SB),NOSPLIT|NOFRAME,$0-8
	WORD	$0x30402573	// csrr a0, mie
	MOV	A0, ret+0(FP)
	RET

// func mstatus() uint64 — de probe toetst dat MIE (bit 3) uit staat vóór hij
// ooit een mie-bit zet: in M-mode wordt een interrupt alleen GENOMEN als
// mstatus.MIE=1, en HopOS heeft geen interrupt-handler die dat overleeft. Uit
// betekent dat een pending+enabled wekker een wfi laat terugkeren zonder ooit
// een trap te worden — precies de eigenschap waar de slaap op leunt.
TEXT ·mstatus(SB),NOSPLIT|NOFRAME,$0-8
	WORD	$0x30002573	// csrr a0, mstatus
	MOV	A0, ret+0(FP)
	RET

// func wfiMTIE() — de slaap-primitief van het EIGEN hart: MTIE aan, wfi, MTIE
// uit. Identiek aan wat de switcher in zijn park doet (cpu/mmode/switch.s),
// maar dan voor de bewoner die al in M-mode staat: HOP zelf. De aanroeper heeft
// mtimecmp al gearmd; wfi kijkt naar mip&mie en mstatus.MIE=0 (getoetst door de
// probe) garandeert dat het wekken nooit een genomen interrupt wordt.
TEXT ·wfiMTIE(SB),NOSPLIT|NOFRAME,$0
	MOV	$0x80, X5	// MTIE (bit 7)
	WORD	$0x3042a073	// csrs mie, x5
	WORD	$0x10500073	// wfi
	WORD	$0x3042b073	// csrc mie, x5
	RET
