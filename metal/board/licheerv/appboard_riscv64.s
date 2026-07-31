// PC-lezer: het eigen adres bepaalt of we HOP zijn of een app-slot (zie
// CoreID in appboard.go). AUIPC met offset 0 geeft precies dat, zonder CSR.

#include "textflag.h"

// func pc() uint64
TEXT ·pc(SB),NOSPLIT|NOFRAME,$0-8
	MOV	$0, X5
	AUIPC	$0, X5
	MOV	X5, ret+0(FP)
	RET
