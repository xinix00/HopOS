// hvcRevoke: de HOP-core (core 0) draait op EL1, maar de hard-kill vereist
// TLBI ALLE1IS — een EL2-instructie. Deze stub doet HVC #0; de revoke-vector
// die stage2.InitVectors op layout.RevokeVecBase legde (en waar cpuinit
// VBAR_EL2 van core 0 heen wees) doet de TLBI en ERET't terug naar de
// instructie ná de HVC. De handler raakt geen general-purpose registers
// (TLBI/DSB/ISB gebruiken xzr), dus er valt niets te bewaren over de HVC heen.

//go:build tamago && arm64

#include "textflag.h"

TEXT ·hvcRevoke(SB),NOSPLIT,$0
	WORD	$0xd4000002	// hvc #0
	RET
