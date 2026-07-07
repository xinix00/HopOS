//go:build tamago

#include "textflag.h"

// MB: DMB SY — volledige geheugenbarrière tussen cores.
TEXT ·MB(SB),NOSPLIT,$0
	WORD	$0xd5033fbf	// dmb sy
	RET
