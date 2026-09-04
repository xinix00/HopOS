//go:build tamago && arm64

#include "textflag.h"

// hvcDoorAck: HVC #5 — de switcher haalt de virtuele FIQ (HCR_EL2.VF) weg.
// Geen argumenten, geen resultaat; de switcher raakt geen app-register.
TEXT ·hvcDoorAck(SB),NOSPLIT,$0
	WORD	$0xd40000a2	// hvc #5
	RET
