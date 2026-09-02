// sysreg.h — de EL1-systeemregisters die de EL2-code (el2.s, smp.s, switch.s)
// van een app leest en schrijft, als macro's met één schakelaar: VHE.
//
// WAAROM: Apple silicon heeft geen FEAT_E2H0 — HCR_EL2.E2H staat vast op 1
// (gemeten M4, 28-08). Met E2H=1 zijn de gewone EL1-encoderingen op EL2
// OMGELEID naar EL2's eigen registers: `mrs x, sctlr_el1` leest dan
// SCTLR_EL2, en de context-switch zou EL2-staat bewaren in plaats van die van
// de app. De echte EL1-registers heten dan `_EL12` (op1=5 i.p.v. 0). Op nVHE-
// silicium (QEMU, Pi, RK3566) bestaan die encoderingen niet — dus twee
// varianten uit één bron, gekozen bij het bouwen: `-asmflags=all=-D=VHE`
// (image/apple-m4.sh). Zonder de define is elke macro byte-gelijk aan de
// oude literal (de nVHE-gate bewijst dat).
//
// Encodering: MRS = 0xd5300000 | op0'<<19 | op1<<16 | CRn<<12 | CRm<<8 |
// op2<<5 | Rt met op0'=1 voor op0=3; MSR = idem met 0xd5100000. _EL12 = de
// EL1-encodering + 0x50000 (op1 0→5). Geverifieerd met aarch64-elf-as.
//
// Niet omgeleid (dus geen macro nodig): TPIDR_EL0/EL1, TPIDRRO_EL0, PAR_EL1,
// CSSELR_EL1, SP_EL0, SP_EL1 (die is al een EL2-encodering) en alle *_EL2.
//
// Eén lay-outverschil dat géén encodering is maar een bitpatroon: CPTR_EL2
// (E2H=0: 0x33FF = RES1 zonder TFP; E2H=1: CPACR-vorm, FPEN=0b11).
// CNTHCTL_EL2 verschilt óók (E2H=0: bits 0/1; E2H=1: bits 10/11), maar de
// drop (drop.h) zet beide lay-outs tegelijk en heeft er geen schakelaar voor.

#ifdef VHE
#define EL1OP1 0x50000
#define CPTR_EL2_NOTRAP 0x300000  /* CPACR-vorm: FPEN=0b11, verder RES0 */
#else
#define EL1OP1 0
#define CPTR_EL2_NOTRAP 0x33FF    /* RES1-bits, TFP=0 */
#endif

#define MRS_SCTLR_EL1(rt)      WORD $(0xd5381000|EL1OP1|rt)
#define MSR_SCTLR_EL1(rt)      WORD $(0xd5181000|EL1OP1|rt)
#define MRS_TCR_EL1(rt)        WORD $(0xd5382040|EL1OP1|rt)
#define MSR_TCR_EL1(rt)        WORD $(0xd5182040|EL1OP1|rt)
#define MRS_TTBR0_EL1(rt)      WORD $(0xd5382000|EL1OP1|rt)
#define MSR_TTBR0_EL1(rt)      WORD $(0xd5182000|EL1OP1|rt)
#define MRS_TTBR1_EL1(rt)      WORD $(0xd5382020|EL1OP1|rt)
#define MSR_TTBR1_EL1(rt)      WORD $(0xd5182020|EL1OP1|rt)
#define MRS_MAIR_EL1(rt)       WORD $(0xd538a200|EL1OP1|rt)
#define MSR_MAIR_EL1(rt)       WORD $(0xd518a200|EL1OP1|rt)
#define MRS_AMAIR_EL1(rt)      WORD $(0xd538a300|EL1OP1|rt)
#define MSR_AMAIR_EL1(rt)      WORD $(0xd518a300|EL1OP1|rt)
#define MRS_VBAR_EL1(rt)       WORD $(0xd538c000|EL1OP1|rt)
#define MSR_VBAR_EL1(rt)       WORD $(0xd518c000|EL1OP1|rt)
#define MRS_CONTEXTIDR_EL1(rt) WORD $(0xd538d020|EL1OP1|rt)
#define MSR_CONTEXTIDR_EL1(rt) WORD $(0xd518d020|EL1OP1|rt)
#define MRS_CPACR_EL1(rt)      WORD $(0xd5381040|EL1OP1|rt)
#define MSR_CPACR_EL1(rt)      WORD $(0xd5181040|EL1OP1|rt)
#define MRS_CNTKCTL_EL1(rt)    WORD $(0xd538e100|EL1OP1|rt)
#define MSR_CNTKCTL_EL1(rt)    WORD $(0xd518e100|EL1OP1|rt)
#define MRS_ELR_EL1(rt)        WORD $(0xd5384020|EL1OP1|rt)
#define MSR_ELR_EL1(rt)        WORD $(0xd5184020|EL1OP1|rt)
#define MRS_SPSR_EL1(rt)       WORD $(0xd5384000|EL1OP1|rt)
#define MSR_SPSR_EL1(rt)       WORD $(0xd5184000|EL1OP1|rt)
#define MRS_ESR_EL1(rt)        WORD $(0xd5385200|EL1OP1|rt)
#define MSR_ESR_EL1(rt)        WORD $(0xd5185200|EL1OP1|rt)
#define MRS_FAR_EL1(rt)        WORD $(0xd5386000|EL1OP1|rt)
#define MSR_FAR_EL1(rt)        WORD $(0xd5186000|EL1OP1|rt)
