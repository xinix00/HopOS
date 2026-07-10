//go:build tamago

#include "textflag.h"

// MB: DMB SY — volledige geheugenbarrière tussen cores.
TEXT ·MB(SB),NOSPLIT,$0
	WORD	$0xd5033fbf	// dmb sy
	RET

// SEV: wek geparkeerde cores (WFE) — HOP dispatcht een core door zijn mailbox
// te schrijven en dan SEV te doen. DSB vooraf zodat de mailbox-write al
// zichtbaar is als de core ontwaakt.
TEXT ·SEV(SB),NOSPLIT,$0
	WORD	$0xd5033f9f	// dsb sy
	WORD	$0xd503209f	// sev
	RET

// CleanInv: DC CIVAC (clean+invalidate naar PoC) per cache-regel over
// [addr, addr+size), afgesloten met DSB SY. By-VA-maintenance broadcast door
// het hele inner-shareable domein, dus dit veegt de regels uit álle caches
// (elke core, elk level t/m de DSU-L3) — precies wat nodig is wanneer HOP
// (ongecached) en een app-core (cacheable) dezelfde fysieke regels raken.
// Regelgrootte uit CTR_EL0.DminLine (A53/A72/A76: 64B), niet hardgecodeerd.
// Op QEMU/TCG is dit een no-op (TCG modelleert geen caches) — het bewijs is
// het board; de Pi-probe (park-levensteken=0) toonde deze bug-klasse al aan.
TEXT ·CleanInv(SB),NOSPLIT,$0-16
	MOVD	addr+0(FP), R0
	MOVD	size+8(FP), R1
	CBZ	R1, done
	ADD	R0, R1, R1	// R1 = einde (exclusief)
	WORD	$0xd53b0022	// mrs x2, ctr_el0
	UBFX	$16, R2, $4, R2	// DminLine: log2(regel/4B)
	MOVD	$4, R3
	LSL	R2, R3, R3	// R3 = regelgrootte in bytes
	SUB	$1, R3, R4
	BIC	R4, R0, R0	// start op regelgrens
loop:
	WORD	$0xd50b7e20	// dc civac, x0
	ADD	R3, R0, R0
	CMP	R1, R0
	BLO	loop
	WORD	$0xd5033f9f	// dsb sy
done:
	RET
