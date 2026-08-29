// Systeemregisters die de probe rapporteert. Alleen lezen; WORD-encodering
// omdat de Go-assembler niet elk ID-register bij naam kent.

//go:build tamago && arm64

#include "textflag.h"

TEXT ·ReadTCR(SB),NOSPLIT,$0-8
	WORD	$0xd5382040	// mrs x0, tcr_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·ReadSCTLR(SB),NOSPLIT,$0-8
	WORD	$0xd5381000	// mrs x0, sctlr_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·ReadMMFR0(SB),NOSPLIT,$0-8
	WORD	$0xd5380700	// mrs x0, id_aa64mmfr0_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·ReadMMFR1(SB),NOSPLIT,$0-8
	WORD	$0xd5380720	// mrs x0, id_aa64mmfr1_el1 (VH = bits 11:8)
	MOVD	R0, ret+0(FP)
	RET

TEXT ·ReadMMFR4(SB),NOSPLIT,$0-8
	WORD	$0xd5380780	// mrs x0, id_aa64mmfr4_el1 (E2H0 = bits 27:24; ongeïmplementeerd leest 0)
	MOVD	R0, ret+0(FP)
	RET

TEXT ·ReadCNTKCTL(SB),NOSPLIT,$0-8
	WORD	$0xd538e100	// mrs x0, cntkctl_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·CNTFRQ(SB),NOSPLIT,$0-8
	WORD	$0xd53be000	// mrs x0, cntfrq_el0
	MOVD	R0, ret+0(FP)
	RET

// CNTPCT: de fysieke systeemteller. Na de hop woont HOP op een andere core dan
// waar de firmware ons startte, en een teller die daar stilstaat laat élke
// time.Sleep eeuwig duren — dus die is meetbaar gemaakt.
TEXT ·CNTPCT(SB),NOSPLIT,$0-8
	WORD	$0xd53be020	// mrs x0, cntpct_el0
	MOVD	R0, ret+0(FP)
	RET

TEXT ·ReadSPRRConfig(SB),NOSPLIT,$0-8
	WORD	$0xd53ef100	// mrs x0, s3_6_c15_c1_0 (SYS_IMP_APL_SPRR_CONFIG_EL1)
	MOVD	R0, ret+0(FP)
	RET

TEXT ·tlbiAll(SB),NOSPLIT,$0-0
	WORD	$0xd5033f9f	// dsb sy
	WORD	$0xd508871f	// tlbi vmalle1
	WORD	$0xd508831f	// tlbi vmalle1is — ook de andere cores; ze delen
				// deze tabellen zodra ze app-werk krijgen
	WORD	$0xd5033f9f	// dsb sy
	WORD	$0xd5033fdf	// isb
	RET

// wfeBurst doet n keer WFE en geeft de verstreken counter-ticks terug: de
// meting die zegt of WFE op dit silicium écht slaapt of alleen een
// klaarstaand event consumeert.
TEXT ·wfeBurst(SB),NOSPLIT,$0-16
	MOVD	n+0(FP), R2
	WORD	$0xd53be040	// mrs x0, cntvct_el0
loopw:
	WFE
	SUB	$1, R2, R2
	CBNZ	R2, loopw
	WORD	$0xd53be041	// mrs x1, cntvct_el0
	SUB	R0, R1, R0
	MOVD	R0, ret+8(FP)
	RET

// wfiTimer zet de fysieke EL1-timer op `ticks` vooruit, doet één WFI en zet de
// timer weer uit; het resultaat is de werkelijk verstreken tijd in ticks. Dit
// is het alternatief voor de WFE-event-stream op een teller waar WFE nauwelijks
// slaapt: een echte deadline i.p.v. een gedeelde event-flank. De interrupt komt
// niet binnen (DAIF gemaskeerd, en niemand routeert de AIC) maar wekt WFI wél —
// dat is precies wat de architectuur belooft voor WFI-wake-up events.
TEXT ·wfiTimer(SB),NOSPLIT,$0-16
	MOVD	ticks+0(FP), R2
	WORD	$0xd53be040	// mrs x0, cntvct_el0
	WORD	$0xd51be202	// msr cntp_tval_el0, x2
	MOVD	$1, R3
	WORD	$0xd51be223	// msr cntp_ctl_el0, x3 (ENABLE, IMASK=0)
	ISB	$15
	WFI
	MOVD	$0, R3
	WORD	$0xd51be223	// msr cntp_ctl_el0, x3 (uit — anders blijft hij pending)
	ISB	$15
	WORD	$0xd53be041	// mrs x1, cntvct_el0
	SUB	R0, R1, R0
	MOVD	R0, ret+8(FP)
	RET

// ESR/FAR van EL1: waarom een toegang stukliep, en op welk adres. De
// EL2-vectortabel in cpuinit.s meldt dit al voor EL2-excepties; dit is
// dezelfde meting voor de wereld waar HopOS zelf in draait (pariteit met de
// EL2-encoderingen daar: alleen op1 verschilt, 0 i.p.v. 4).
TEXT ·ReadESR(SB),NOSPLIT,$0-8
	WORD	$0xd5385200	// mrs x0, esr_el1
	MOVD	R0, ret+0(FP)
	RET

TEXT ·ReadFAR(SB),NOSPLIT,$0-8
	WORD	$0xd5386000	// mrs x0, far_el1
	MOVD	R0, ret+0(FP)
	RET

// timerFires: zet de fysieke timer op `ticks` en POLLT of hij afgaat (ISTATUS),
// zonder WFI. Dat scheidt twee vragen die anders op één hang uitkomen: loopt de
// timer op deze core, of loopt hij wel maar wekt hij de core niet? Geeft het
// aantal verstreken ticks, of 0 als hij binnen het budget niet afging.
TEXT ·timerFires(SB),NOSPLIT,$0-16
	MOVD	ticks+0(FP), R2
	WORD	$0xd53be041	// mrs x1, cntvct_el0 — startstand
	WORD	$0xd51be202	// msr cntp_tval_el0, x2
	MOVD	$1, R3
	WORD	$0xd51be223	// msr cntp_ctl_el0, x3 (ENABLE, IMASK=0)
	ISB	$15
	LSL	$4, R2, R4	// budget: ruim boven de deadline
tfloop:
	WORD	$0xd53be220	// mrs x0, cntp_ctl_el0
	AND	$4, R0, R5	// ISTATUS
	CBNZ	R5, tfdone
	SUB	$1, R4
	CBNZ	R4, tfloop
	MOVD	$0, R0
	B	tfout
tfdone:
	WORD	$0xd538c100	// mrs x0, isr_el1 — staat de FIQ/IRQ pending AAN DEZE
				// core? Los van DAIF, dus veilig te lezen: dit is
				// precies de vraag die WFI anders met een eeuwige
				// slaap beantwoordt.
	ORR	$1<<32, R0, R0	// merk "de timer ging af" apart van de ISR-bits
tfout:
	MOVD	$0, R3
	WORD	$0xd51be223	// msr cntp_ctl_el0, x3 (uit)
	ISB	$15
	MOVD	R0, ret+8(FP)
	RET

// TTBR0_EL1: welke tabellen de hardware op DEZE core echt gebruikt. Niet
// dezelfde vraag als "welke tabellen hebben wij gevuld" — en op een core die
// wij pas ná de firmware bewoonden is dat verschil precies wat je wilt zien.
TEXT ·ReadTTBR0(SB),NOSPLIT,$0-8
	WORD	$0xd5382000	// mrs x0, ttbr0_el1
	MOVD	R0, ret+0(FP)
	RET
