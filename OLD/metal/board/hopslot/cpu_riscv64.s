//go:build riscv64

#include "textflag.h"

// func rdtime() uint64 — de TIME CSR. Op de C906 is dit de enige tijdbron: de
// c900-CLINT heeft géén mtime-register (gemeten 30-07, elke read is een
// bus-fout), en een gekooide app krijgt er toch geen venster op.
TEXT ·rdtime(SB),NOSPLIT|NOFRAME,$0-8
	RDTIME	X5
	MOV	X5, ret+0(FP)
	RET

// func mhartid() uint64 — de Go-assembler kent alleen CSR-namen, en mhartid
// staat er niet bij; dus het rauwe woord (csrr x5, mhartid = csrrs x5, mhartid, x0).
TEXT ·mhartid(SB),NOSPLIT|NOFRAME,$0-8
	WORD	$0xf14022f3	// csrr x5, mhartid
	MOV	X5, ret+0(FP)
	RET
