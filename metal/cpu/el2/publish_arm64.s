//go:build tamago && arm64

#include "textflag.h"

// The caller's data-cache clean ends with DSB. Broadcast instruction-cache
// invalidation before any core is dispatched into the newly copied code.
TEXT ·PublishCode(SB),NOSPLIT,$0
	WORD	$0xd508711f	// ic ialluis
	DSB	$15
	ISB	$15
	RET
