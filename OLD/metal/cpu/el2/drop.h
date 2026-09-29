// drop.h — de ENE drop van EL2 naar EL1: DROP_TO_EL1(entry).
//
// Acht plekken in de boom deden dit elk op hun eigen manier, en ze waren
// uit elkaar gegroeid op precies de punten waar het pijn deed
// (docs/LAATSTE_PLAN.md):
//
//   - SCTLR_EL1 op een bekende waarde vóór de ERET stond alleen in apple (de
//     flip-fix van 02-09: EL1 erfde de MMU van de vórige kern) en in de
//     trampoline (de Pi 5-fix van 10-07: een warme CPU_ON erfde EL1 van de
//     vorige huurder — TF-A initieert alleen EL2, en de allereerste
//     EL1-fetch na de ERET vertaalt dan door stale tabellen; QEMU verhult
//     dat met een volledige vCPU-reset). De andere zes erfden wat er stond.
//   - CNTHCTL_EL2 in beide lay-outs (E2H 0 én 1) stond alleen in apple.
//   - CPTR_EL2 zonder FP-trap stond alleen in de trampoline (anders trapt
//     tamago's eerste FP-instructie naar EL2).
//
// Elke fix was één plek gefixt en zeven niet. Vanaf nu staat de reeks hier,
// en gaat elke ingang — de trampoline (el2.s), de SMP-secundaire (smp.s) en
// elke board-boot (boot.h) — door dezelfde macro.
//
// De reeks, in deze volgorde:
//
//   SCTLR_EL1 = 0x30d00800   RES1-bits; M/C/I/A/WXN uit — NOOIT erven
//   CPTR_EL2  = geen FP-trap  (nVHE 0x33FF; VHE de CPACR-vorm, FPEN=0b11)
//   CNTHCTL   = EL1-toegang tot teller en timer, in BEIDE lay-outs
//   CNTVOFF   = 0
//   SPSR_EL2  = EL1h, DAIF gemaskeerd
//   ELR_EL2   = entry
//   ISB; ERET
//
// Wat de macro NIET doet, met opzet:
//
//   - HCR_EL2: die is van de aanroeper. De boot zet RW, de trampoline
//     RW|TSC|VM|FMO — dat is het enige echte verschil tussen de ingangen, en
//     het hoort zichtbaar te blijven bij wie het zet.
//   - I-cache-hygiëne (hygiene.h): alleen wie in vers geschreven code springt
//     heeft die nodig; die aanroeper zet I_HYGIENE vlak vóór deze macro.
//
// Contract voor de aanroeper:
//
//   - `entry` is een REGISTERNUMMER (0–30), net als de rt van sysreg.h.
//   - x0–x7 blijven onaangeroerd en overleven de ERET: zo geeft smp.s de
//     M-context aan zijn EL1-stub door. Klad is x16 (IP0 — het
//     intra-procedure-call-register dat de ABI precies hiervoor bedoelt).
//   - sysreg.h moet vóór dit bestand geïncludeerd zijn (de VHE-schakelaar,
//     MSR_SCTLR_EL1 en CPTR_EL2_NOTRAP). Go's asm-preprocessor lost #include
//     op vanuit de package-map van het .s-bestand, niet vanuit het
//     includerende .h — daarom includeert dit bestand niets zelf.
//
// CNTHCTL_EL2 in beide lay-outs: bits 0/1 zijn onder E2H=0 EL1PCTEN|EL1PCEN,
// bits 10/11 zijn dat onder E2H=1 (EL1PCTEN|EL1PTEN). De andere twee zijn in
// elke lay-out onschuldig (E2H=0: RES0; E2H=1: EL0-toegang). Twee ORR's,
// want 0xC03 is geen geldige bitmask-immediate en zou de assembler naar
// REGTMP laten grijpen.
//
// Encoderingen (Go's assembler kent de EL2-registers niet bij naam), Rt=16:
// MSR = 0xd5180000 | op1<<16 | CRn<<12 | CRm<<8 | op2<<5 | Rt.
#define DROP_TO_EL1(entry) \
	MOVD $0x30d00800, R16; \
	MSR_SCTLR_EL1(16); \
	MOVD $CPTR_EL2_NOTRAP, R16; \
	WORD $0xd51c1150; \
	WORD $0xd53ce110; \
	ORR $0b11, R16, R16; \
	ORR $0b11<<10, R16, R16; \
	WORD $0xd51ce110; \
	MOVD $0, R16; \
	WORD $0xd51ce070; \
	MOVD $0x3c5, R16; \
	WORD $0xd51c4010; \
	WORD $(0xd51c4020|entry); \
	ISB $15; \
	ERET
