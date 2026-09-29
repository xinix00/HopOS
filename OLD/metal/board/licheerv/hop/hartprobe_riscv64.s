// De SELF-probe van de app-hart-wekker: draait op het app-hart ZELF, in
// M-mode, vers uit reset — want dat is de enige plek waar de vraag te
// beantwoorden is. mip is een per-hart CSR: of MTIP op hart 1 ooit pendt kan
// hart 0 principieel niet zien. De boot-probe op hart 0 (licheerv/clint.go)
// bewees zíjn keten, en de extrapolatie naar dit hart was de storm van 01-08:
// de eerste park-slaap op de C906L werd nooit gewekt ("core 1 never yielded").
// Dit bestand meet in plaats van aan te nemen.
//
// Contract met hartprobe.go (de HOP-kant):
//   - entry via HartOn(hart, appHartProbePC()) — reset-boot, geen argumenten,
//     geen stack; alles komt uit immediates en mhartid.
//   - resultaten landen in de boot-scratch-page (0x8FE00000, zie plan.go —
//     een init-check houdt beide in de pas):
//       +0   voortgang: nummer van de laatst GESLAAGDE stap (zie onder)
//       +8   mhartid (bevestigt op welk hart we echt draaien)
//       +16  rdtime-sample bij start (tijd-domein-vergelijking met hart 0)
//       +24  vuur-latentie van mtimecmp in tikken
//       +32  mip-snapshot (diagnose bij een gestrande stap)
//       +40  msip-uitslag: 1 = ok, 2 = never pended, 3 = stuck pending
//   - caches BLIJVEN UIT (reset-stand): elke store gaat rechtstreeks DRAM in,
//     dus HOP hoeft alleen zijn eigen cache te invalideren (dev.Pull).
//   - een hang vertelt evenveel als een uitslag: de voortgang blijft staan op
//     de laatste geslaagde stap en HOP's timeout + hart-reset ruimt op.
//
// De stappen (voortgangsnummers):
//   1  levend op het hart, mtvec geparkeerd
//   2  mtimecmp houdt een geschreven waarde vast
//   3  gearmd op now+1ms
//   4  MTIP is gaan staan (comparator vuurt → de draad naar dít hart bestaat)
//   5  MTIP zakt na ontwapenen
//   6  msip-test afgerond (uitslag op +40 — optioneel kanaal, blokkeert niet)
//   7  wfi met pending+enabled keerde terug — de keten is rond
//
// Alle wachtlussen zijn begrensd op rdtime-deadlines; alleen de wfi van stap 7
// kan per definitie niet zelf begrensd worden — dát is precies wat hij test.

//go:build tamago

#include "textflag.h"

// func appHartProbePC() uintptr — het fysieke adres van de probe (HOP's image
// is identity-geladen, dus het symbooladres ís het PA; zelfde argument als
// cpu/mmode.EntryPC).
TEXT ·appHartProbePC(SB),NOSPLIT,$0-8
	MOV	$hartprobe(SB), X5
	MOV	X5, ret+0(FP)
	RET

TEXT hartprobe(SB),NOSPLIT|NOFRAME,$0
	// Een onverwachte trap (misuitgelijnde fetch, illegale instructie) hoort
	// te parkeren, niet naar een reset-waarde van mtvec te springen.
	MOV	$probepark(SB), X6
	WORD	$0x30531073		// csrw mtvec, x6

	MOV	$0x8FE00000, X5		// mailbox = boot-scratch (plan.go)

	// Stap 1: levend, en op wélk hart.
	WORD	$0xf1402373		// csrr x6, mhartid
	MOV	X6, 8(X5)
	MOV	$1, X7
	MOV	X7, 0(X5)
	FENCE

	// CLINT-adressen van DIT hart — uit mhartid, niet hardgecodeerd, zodat
	// dezelfde probe elk toekomstig app-hart kan doormeten.
	MOV	$0x74004000, X10	// mtimecmp[hart] = basis + 8×hart
	SLL	$3, X6, X7
	ADD	X7, X10, X10
	MOV	$0x74000000, X11	// msip[hart] = basis + 4×hart
	SLL	$2, X6, X7
	ADD	X7, X11, X11

	// Tijd-sample voor de domein-vergelijking (loopt de teller van dit hart
	// in dezelfde wereld als die van hart 0?).
	WORD	$0xc01023f3		// csrr x7, time
	MOV	X7, 16(X5)

	// Stap 2: houdt mtimecmp een waarde vast? 32-bit toegangen — deze CLINT
	// weigert 64-bit MMIO (gemeten, licheerv.go).
	MOV	$-1, X7
	MOVW	X7, 0(X10)
	MOVW	X7, 4(X10)
	MOVWU	0(X10), X8
	MOVWU	4(X10), X9
	SLL	$32, X9, X9
	OR	X8, X9, X9
	MOV	$-1, X7
	BEQ	X7, X9, hold_ok
	MOV	X9, 32(X5)		// wat er wél terugkwam
	FENCE
	JMP	probepark(SB)		// voortgang blijft 1
hold_ok:
	MOV	$2, X7
	MOV	X7, 0(X5)
	FENCE

	// Stap 3: armen op now+1ms, in de spec-volgorde (lo→∞, hi, lo) zodat er
	// nooit een halve waarde in het verleden staat.
	WORD	$0xc01023f3		// csrr x7, time — de arm-tijd
	MOV	$25000, X9
	ADD	X7, X9, X9		// x9 = wektijd
	MOV	$-1, X13
	MOVW	X13, 0(X10)
	SRL	$32, X9, X13
	MOVW	X13, 4(X10)
	MOVW	X9, 0(X10)
	MOV	$3, X13
	MOV	X13, 0(X5)
	FENCE

	// Stap 4: vuurt de comparator — pendt MTIP (bit 7) op dít hart? Harde
	// deadline op arm-tijd+3ms; de wektijd ligt op +1ms.
	MOV	$75000, X12
	ADD	X7, X12, X12
fire_wait:
	WORD	$0x34402473		// csrr x8, mip
	MOV	$128, X13
	AND	X13, X8, X13
	BNEZ	X13, fired
	WORD	$0xc0102773		// csrr x14, time
	BLTU	X14, X12, fire_wait
	MOV	X8, 32(X5)		// nooit gevuurd: mip-snapshot voor de diagnose
	FENCE
	JMP	probepark(SB)		// voortgang blijft 3
fired:
	WORD	$0xc0102773		// csrr x14, time
	SUB	X7, X14, X14
	MOV	X14, 24(X5)		// vuur-latentie sinds het armen
	MOV	X8, 32(X5)
	MOV	$4, X13
	MOV	X13, 0(X5)
	FENCE

	// Stap 5: ontwapenen en MTIP moet weer zakken (1ms settle).
	MOV	$-1, X13
	MOVW	X13, 0(X10)
	MOVW	X13, 4(X10)
	WORD	$0xc0102773		// csrr x14, time
	MOV	$25000, X12
	ADD	X14, X12, X12
clear_wait:
	WORD	$0x34402473		// csrr x8, mip
	MOV	$128, X13
	AND	X13, X8, X13
	BEQZ	X13, cleared
	WORD	$0xc0102773		// csrr x14, time
	BLTU	X14, X12, clear_wait
	MOV	X8, 32(X5)
	FENCE
	JMP	probepark(SB)		// voortgang blijft 4
cleared:
	MOV	$5, X13
	MOV	X13, 0(X5)
	FENCE

	// Stap 6: msip — zelf zetten en wissen, mét settle-tijd per flank (de
	// les van de hart-0-probe: wie meteen leest meet zijn eigen ongeduld).
	// Optioneel kanaal: de uitslag gaat naar +40 en blokkeert de rest niet.
	MOV	$1, X13
	MOVW	X13, 0(X11)
	WORD	$0xc0102773		// csrr x14, time
	MOV	$25000, X12
	ADD	X14, X12, X12
msip_set:
	WORD	$0x34402473		// csrr x8, mip
	MOV	$8, X13			// MSIP (bit 3)
	AND	X13, X8, X13
	BNEZ	X13, msip_pended
	WORD	$0xc0102773		// csrr x14, time
	BLTU	X14, X12, msip_set
	MOV	$2, X13			// never pended
	MOV	X13, 40(X5)
	MOVW	ZERO, 0(X11)
	JMP	msip_done
msip_pended:
	MOVW	ZERO, 0(X11)
	WORD	$0xc0102773		// csrr x14, time
	MOV	$25000, X12
	ADD	X14, X12, X12
msip_clr:
	WORD	$0x34402473		// csrr x8, mip
	MOV	$8, X13
	AND	X13, X8, X13
	BEQZ	X13, msip_ok
	WORD	$0xc0102773		// csrr x14, time
	BLTU	X14, X12, msip_clr
	MOV	$3, X13			// stuck pending
	MOV	X13, 40(X5)
	JMP	msip_done
msip_ok:
	MOV	$1, X13
	MOV	X13, 40(X5)
msip_done:
	MOV	$6, X13
	MOV	X13, 0(X5)
	FENCE

	// Stap 7: de wfi zelf — pending éérst (wektijd = nu), dan MTIE aan, wfi,
	// MTIE uit. mstatus.MIE is 0 uit reset en niemand heeft hem aangeraakt,
	// dus de pending wekker kan alleen WEKKEN, nooit een trap worden.
	WORD	$0xc0102773		// csrr x14, time
	MOV	$-1, X13
	MOVW	X13, 0(X10)
	SRL	$32, X14, X13
	MOVW	X13, 4(X10)
	MOVW	X14, 0(X10)		// wektijd = nu → per direct pending
	MOV	$128, X13
	WORD	$0x3046a073		// csrs mie, x13
	WORD	$0x10500073		// wfi — DE test; een hang laat voortgang 6 staan
	WORD	$0x3046b073		// csrc mie, x13
	MOV	$-1, X13
	MOVW	X13, 0(X10)		// ontwapend achterlaten
	MOVW	X13, 4(X10)
	MOV	$7, X13
	MOV	X13, 0(X5)
	FENCE

	// Stap 8 (bonus, ná het oordeel van stap 7): is het DW-WDT-blok
	// (0x03010000) bereikbaar? Van deze SoC is gemeten dat afwezige MMIO een
	// bus-fout is (de CLINT-mtime-les), en HOP kan zo'n fout niet overleven —
	// dít hart wel: een trap springt naar probepark (mtvec) en +48 blijft dan
	// 0 = "niet bereikbaar". We schrijven niets dat de WDT wapent: alleen
	// TORR (de timeout-range, inert zolang CR.enable uit staat) en lezen hem
	// terug. 1 = leeft en houdt waarden vast; 2 = leeft maar readback wijkt af.
	MOV	$0x03010000, X10
	MOVWU	8(X10), X14		// WDT_CCVR — de aanraking die faalt, parkeert
	MOV	$0xFF, X13
	MOVW	X13, 4(X10)		// WDT_TORR = top/top_init 15 (inert: enable uit)
	MOVWU	4(X10), X14
	MOV	$0xFF, X13
	BEQ	X14, X13, wdt_ok
	MOV	$2, X13			// leeft, maar houdt de waarde niet vast
	MOV	X13, 48(X5)
	FENCE
	JMP	probepark(SB)
wdt_ok:
	MOV	$1, X13			// bereikbaar én beschrijfbaar
	MOV	X13, 48(X5)
	FENCE
	JMP	probepark(SB)

// probepark: klaar (of gestrand). Niet stil — de lus schrijft continu de
// waarde 1 naar het EIGEN mtimecmp-adres (0x74004000 + 8×mhartid). Dat is de
// DECODE-TEST: beide cores noemen zichzelf mhartid 0 en gebruiken dus
// hetzelfde adres. Leest HOP ondertussen een 1 op zíjn 0x74004000, dan is de
// CLINT GEDEELD en vechten twee slapers om één comparator — de verdenking
// achter de stille nodedood van 01-08 (boots 6/7: HOP's hart weg, app-hart
// draaide door). Leest hij alleen eigen waarden, dan is de decode core-lokaal.
// De waarde 1 is bewust: altijd in het verleden, dus als hij HOP's comparator
// overschrijft wekt dat per direct (spin, geen hang) — de veilige richting.
// Geen wfi hier: dít is de toestand waarin we juist níet weten of die terugkomt.
TEXT probepark(SB),NOSPLIT|NOFRAME,$0
	WORD	$0xf1402373		// csrr x6, mhartid
	MOV	$0x74004000, X10
	SLL	$3, X6, X7
	ADD	X7, X10, X10
	MOV	$1, X13
parkloop:
	MOVW	X13, 0(X10)
	JMP	parkloop
