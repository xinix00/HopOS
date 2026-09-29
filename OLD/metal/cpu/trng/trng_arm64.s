// CPU-instructie-entropie en feature-detectie voor package trng. Geen mnemonic
// in de Go-assembler voor RNDR/systeemregisters → als WORD gecodeerd:
//   mrs R0, RNDR            = 0xd53b2400  (S3_3_C2_C4_0)
//   mrs R0, ID_AA64ISAR0_EL1 = 0xd5380600 (RNDR-feature in [63:60])
//   mrs R0, ID_AA64PFR0_EL1  = 0xd5380400 (EL3-veld in [15:12])

//go:build tamago && arm64

#include "textflag.h"

// rndr64() (v uint64, ok bool) — leest RNDR. RNDR zet PSTATE.NZCV: Z=1 ⇒ geen
// entropie (v=0). Zie Arm ARM, FEAT_RNG.
TEXT ·rndr64(SB),NOSPLIT,$0-9
	WORD	$0xd53b2400	// mrs R0, RNDR
	BEQ	rndrfail	// Z=1 → mislukt
	MOVD	R0, v+0(FP)
	MOVD	$1, R1
	MOVB	R1, ok+8(FP)
	RET
rndrfail:
	MOVD	$0, R0
	MOVD	R0, v+0(FP)
	MOVB	R0, ok+8(FP)
	RET

// rndrSupported() bool — ID_AA64ISAR0_EL1.RNDR ([63:60]) != 0.
TEXT ·rndrSupported(SB),NOSPLIT,$0-1
	WORD	$0xd5380600	// mrs R0, ID_AA64ISAR0_EL1
	LSR	$60, R0, R0
	CBZ	R0, isarno
	MOVD	$1, R0
	MOVB	R0, ret+0(FP)
	RET
isarno:
	MOVD	$0, R0
	MOVB	R0, ret+0(FP)
	RET

// el3Present() bool — ID_AA64PFR0_EL1.EL3 ([15:12]) != 0.
TEXT ·el3Present(SB),NOSPLIT,$0-1
	WORD	$0xd5380400	// mrs R0, ID_AA64PFR0_EL1
	LSR	$12, R0, R0
	AND	$0xf, R0, R0
	CBZ	R0, el3no
	MOVD	$1, R0
	MOVB	R0, ret+0(FP)
	RET
el3no:
	MOVD	$0, R0
	MOVB	R0, ret+0(FP)
	RET
