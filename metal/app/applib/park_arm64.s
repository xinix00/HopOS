// hvcExit: coöperatief stoppen doet de app niet meer met PSCI CPU_OFF (op de
// Pi 5-stockfirmware is dat een one-way door — de core komt nooit terug), maar
// met een HVC. Die trapt naar de EL2-vectoren van HopOS (VBAR_EL2 → de app-
// core-vectoren): index 8, ESR.EC = 0x16 (HVC64) → HopOS parkeert de core op
// EL2 in zijn WFE-lus, klaar om herstart te worden. De app zette StatusExited
// al vóór deze call, dus HopOS rapporteert géén fault. Keert nooit terug.

//go:build tamago && arm64

#include "textflag.h"

TEXT ·hvcExit(SB),NOSPLIT,$0
	WORD	$0xd4000002	// hvc #0
	RET			// onbereikbaar (HopOS parkeert de core)
