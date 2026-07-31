// T-Head C906 cache maintenance ops — encodings uit de vendor-kernel
// (linux_5.10/arch/riscv/mm/cacheflush.c). De Go-assembler kent de
// XuanTie-extensies niet, vandaar WORD.

#include "textflag.h"

// func dcacheCPA(start, end uintptr) — clean by PA, per 64B line + sync.is
TEXT ·dcacheCPA(SB),NOSPLIT|NOFRAME,$0-16
	MOV	start+0(FP), A0
	MOV	end+8(FP), A1
cpa_loop:
	WORD	$0x0295000b	// th.dcache.cpa a0
	ADD	$64, A0
	BLT	A0, A1, cpa_loop
	WORD	$0x01b0000b	// th.sync.is
	RET

// func dcacheCIPA(start, end uintptr) — clean+invalidate by PA + sync.is
TEXT ·dcacheCIPA(SB),NOSPLIT|NOFRAME,$0-16
	MOV	start+0(FP), A0
	MOV	end+8(FP), A1
cipa_loop:
	WORD	$0x02b5000b	// th.dcache.cipa a0
	ADD	$64, A0
	BLT	A0, A1, cipa_loop
	WORD	$0x01b0000b	// th.sync.is
	RET

// func Fence()
TEXT ·Fence(SB),NOSPLIT|NOFRAME,$0
	FENCE
	RET
