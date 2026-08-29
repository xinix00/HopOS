// bootstub.s — de voorkant van het bootobject: twee stukjes code die vóór
// HopOS draaien en die, anders dan al het andere in dit image, NIET op hun
// linkadres staan.
//
// Waarom ze bestaan. Zodra wij zélf het bootobject zijn (kmutil configure-boot
// --raw --entry-point 2048) laadt iBoot dit bestand op een adres van zijn
// keuze — gemeten 29-08 stond m1n1 de ene boot op 0x100_2bb8_000 en de andere
// op 0x100_3a90_000 — en springt naar bestandsoffset 0x800. Ons image is
// gelinkt op 0x101_0001_0000 en is niet verplaatsbaar, dus er moet iets
// tussen dat de rest naar zijn plek kopieert. Precies twee dingen landen
// buiten hun linkadres, en dat zijn deze:
//
//	offset 0x000  stubReset  — waar de SECUNDAIRE cores uit reset landen.
//	              iBoot zet RVBAR op het begin van het bootobject en
//	              vergrendelt het (lock=1 op alle tien de cores, gemeten
//	              29-08), dus dit adres is niet onderhandelbaar. m1n1 doet
//	              hetzelfde: bij hem staat daar zijn vectortabel, en zijn
//	              start.S controleert zelfs of RVBAR == _vectors_start.
//	offset 0x800  stubEntry — waar de firmware de BOOT-core aflevert, met
//	              x0 = boot_args. 0x800 omdat daar bij m1n1 zijn 16 × 0x80
//	              grote vectortabel eindigt; die 2048 staat in het
//	              installatiecommando en is dus vast.
//
// mkkernel -apple kopieert deze twee symbolen naar die offsets in het platte
// image en legt op offset 0x100 een parameterblok neer (doel, grootte,
// entry). Alles wat de stubs nodig hebben lezen ze daaruit, PC-relatief via
// hun eigen basis — géén enkele absolute constante, want een 64-bit immediate
// wordt door de Go-assembler soms een load uit een literal pool, en die pool
// blijft achter op zijn linkadres.
//
// De stubs draaien op EL2 met de MMU uit. Dat is geen aanname maar de
// vaststelling van m1n1: zijn mmu_init begint met "staat SCTLR_M al aan? dan
// klaar", en die tak wordt nooit genomen. Met de MMU uit is geheugen
// Device-nGnRnE: elke toegang gaat naar DRAM, exclusives (LDAXR/STLXR) zijn
// CONSTRAINED UNPREDICTABLE. Vandaar de MPIDR-brievenbus hieronder in plaats
// van een atomaire claim.

//go:build tamago && arm64

#include "textflag.h"

// Het scratch-blok, gerekend vanaf het DOELadres — pariteit met apple.go.
// Eén offset, de rest hangt eraan: hoe minder getallen op twee plekken staan,
// hoe minder er uit de pas kan lopen.
#define SCRATCH_OFF 0xE000
#define PARK_PC     0x30
#define PARK_ARG    0x38
#define PARK_FOR    0x40
#define STUB_SRC    0x48
#define STUB_X0     0x50

// Het parameterblok van mkkernel, op image-offset 0x100 (dus net achter
// stubReset, die daar ruim onder blijft).
#define P_DST   0x108
#define P_SIZE  0x110
#define P_ENTRY 0x118

// stubReset — de landingsplaats van een core die uit reset komt (RVBAR).
//
// Hij doet niets aan het silicium: m1n1's tabel geeft de M4-cores geen enkele
// chicken bit mee (chickens.c: init = NULL voor Donan E én P), dus een verse
// core is bruikbaar zoals hij is. Wat hij wél doet is wachten op werk.
//
// De brievenbus is op MPIDR geadresseerd: HOP schrijft argument en entry, en
// dan als laatste vóór wie het bedoeld is. Zo kunnen alle geparkeerde cores op
// hetzelfde adres wachten zonder elkaar het werk af te pakken — nodig, want
// zonder MMU is er geen bruikbare atomaire operatie om het anders te doen.
TEXT ·stubReset(SB),NOSPLIT|NOFRAME,$0
	WORD	$0x1000000a		// adr x10, . — wij staan op image-offset 0
	MOVD	P_DST(R10), R11		// waar het image straks draait
	ADD	$SCRATCH_OFF, R11, R11
	MRS	MPIDR_EL1, R2
	AND	$0xFFFF, R2, R2		// aff1:aff0 = cluster:core — uniek op deze SoC's,
					// en het enige dat HOP zelf kan afleiden

resetloop:
	MOVD	PARK_FOR(R11), R14
	CMP	R2, R14
	BNE	resetwait
	MOVD	PARK_PC(R11), R15
	CBNZ	R15, resetgo
resetwait:
	WFE
	B	resetloop

resetgo:
	MOVD	PARK_ARG(R11), R0
	MOVD	ZR, PARK_FOR(R11)	// ontvangstbewijs: HOP mag de volgende roepen
	WORD	$0xd5033f9f		// dsb sy
	JMP	(R15)

// stubEntry — de boot-core, met x0 van de firmware (boot_args als iBoot ons
// startte, m1n1's FDT als de proxy dat deed).
//
// Verplaatst het image naar zijn linkadres en springt erheen. Staat het er al
// (de loader kan het meteen goed neerzetten), dan blijft er alleen het
// vastleggen van waar we vandaan kwamen over — dat adres is wat RVBAR moet
// zijn, en dus het antwoord op "zijn de cores van ons".
TEXT ·stubEntry(SB),NOSPLIT|NOFRAME,$0
	WORD	$0x1000000a		// adr x10, . — wij staan op image-offset 0x800
	SUB	$0x800, R10, R10	// → de basis van het image
	MOVD	R0, R9			// x0 van de firmware; overleeft alles hieronder

	MOVD	P_DST(R10), R11
	MOVD	P_SIZE(R10), R12
	MOVD	P_ENTRY(R10), R13
	CBZ	R11, stubhang		// geen parameterblok: niets zinnigs te doen
	CBZ	R13, stubhang
	CMP	R11, R10
	BEQ	stubjump		// staat al goed

	// Overlap zou ons onder onze eigen voeten vandaan kopiëren. Naar BENEDEN
	// verplaatsen mag wel (voorwaarts kopiëren loopt dan voor zichzelf uit),
	// naar boven binnen het image niet.
	SUB	R10, R11, R1		// doel - bron; wikkelt om als het doel lager ligt
	CMP	R12, R1
	BLO	stubhang

	// Cacheregelgrootte uit CTR_EL0 (DminLine = log2 van het aantal woorden).
	WORD	$0xd53b0030		// mrs x16, ctr_el0
	UBFX	$16, R16, $4, R16
	MOVD	$4, R17
	LSL	R16, R17, R17

	// De bron schoonvegen. De firmware schreef ons met caches AAN; wij lezen
	// zo dadelijk met de MMU uit, rechtstreeks uit DRAM. Wat nog in een cache
	// hangt zouden we anders missen.
	MOVD	R10, R14
	ADD	R12, R10, R15
cleansrc:
	WORD	$0xd50b7c2e		// dc civac, x14
	ADD	R17, R14, R14
	CMP	R15, R14
	BLO	cleansrc

	// En het doel. Daar kunnen regels van een vorig leven liggen; een dirty
	// regel die later uitgeschreven wordt zou dwars door onze verse bytes heen
	// gaan.
	MOVD	R11, R14
	ADD	R12, R11, R15
cleandst:
	WORD	$0xd50b7c2e		// dc civac, x14
	ADD	R17, R14, R14
	CMP	R15, R14
	BLO	cleandst
	WORD	$0xd5033f9f		// dsb sy

	// Kopiëren, 16 bytes per slag. Ongecachet is elke toegang een rit naar
	// DRAM, dus het paarsgewijze LDP/STP scheelt de helft; mkkernel rondt de
	// grootte op 64 af zodat de staart geen apart geval is.
	MOVD	R10, R14
	MOVD	R11, R15
	ADD	R12, R10, R16
copy:
	LDP	(R14), (R1, R2)
	STP	(R1, R2), (R15)
	ADD	$16, R14, R14
	ADD	$16, R15, R15
	CMP	R16, R14
	BLO	copy

	WORD	$0xd5033f9f		// dsb sy
	WORD	$0xd508751f		// ic iallu — we hebben net code neergelegd
	WORD	$0xd5033f9f		// dsb sy
	ISB	$15

stubjump:
	// Waar de firmware ons neerzette, en wat er in x0 stond. Allebei pas hier
	// opschrijven: het scratch-blok ligt IN het image en is een regel geleden
	// nog overschreven door de kopie.
	ADD	$SCRATCH_OFF, R11, R14
	MOVD	R10, STUB_SRC(R14)
	MOVD	R9, STUB_X0(R14)
	WORD	$0xd5033f9f		// dsb sy
	MOVD	R9, R0
	JMP	(R13)

stubhang:
	WFE
	B	stubhang
