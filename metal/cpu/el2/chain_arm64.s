// ChainloadEL2: de kern-flip-sprong (docs/kern-flip.md). De nieuwe kern moet
// op EL2 binnenkomen met de MMU uit én een schone cache, en hoe we daar komen
// hangt af van waar we NU zijn:
//
//   - EL1 (QEMU virt, later de Pi's): HVC #2 naar de handler die InitVectors
//     op de revoke-vectoren legde (slot 0x400, synchrone exception uit een
//     lager EL). LET OP: dat pad veegt de caches nog NIET — op QEMU (TCG,
//     geen cachemodel) is dat onzichtbaar, maar het is een bekend gat voor de
//     eerste EL1-board-flip op ijzer.
//   - EL2 mét VHE (de M4: E2H staat er vast op 1): een HVC zou hier op het
//     same-level-slot (0x200) landen, waar het board zijn fault-dumper heeft.
//     Op EL2 is er ook niemand nodig — we doen zelf wat m1n1 vóór élke
//     payload doet (mmu_disable, src/memory.c): SCTLR lezen en I/C/M eruit
//     maskeren (lezen-en-maskeren, geen gegokte vaste waarde), en daarná de
//     datacache vegen. m1n1 veegt álles by set/way (dcsw_op_all); wij vegen
//     by VA over precies HOP's eigen RAM-venster, en dat is op dit board
//     bewijsbaar hetzelfde: al het andere geheugen is device-gemapt
//     (board/apple MapDRAM), dus buiten dit venster kán geen dirty line
//     bestaan. Ná het vegen wordt er niets cacheable meer geschreven (C
//     staat al uit), dus er blijft niets achter. Eerst DAIF dicht: een
//     timer-FIQ tussen "MMU uit" en de sprong zou in Go-code landen die er
//     niet meer is. Omdat HOP plat gemapt is loopt de fetch na "M uit"
//     gewoon door op hetzelfde fysieke adres (dezelfde dans als cpuinit's
//     nohop-pad).
//
// Keert nooit terug.

//go:build tamago && arm64

#include "textflag.h"
#include "hygiene.h"

TEXT ·chainload(SB),NOSPLIT,$0-40
	MOVD	entry+0(FP), R16
	MOVD	x0arg+8(FP), R0
	MOVD	ramStart+16(FP), R4
	MOVD	ramEnd+24(FP), R5
	MOVD	vbar+32(FP), R7
	WORD	$0xd5384242	// mrs x2, currentel
	CMP	$0x8, R2
	BNE	hvc		// EL1: via de handler
	WORD	$0xd53c1103	// mrs x3, hcr_el2
	TBZ	$34, R3, hvc	// EL2 zonder E2H: de sctlr_el1-redirect klopt dan niet
	WORD	$0xd5034fdf	// msr daifset, #0xf
	// Vectoren van déze core naar de kale fault-dumper (el2fault-tabel op
	// TrapVecPA, zelfde bytes in oude én nieuwe kern): een vroege fault in
	// de landing meldt dan ESR/ELR/FAR in plaats van door de oude
	// tamago-vectoren het lijk van deze kern in te vallen. Onder E2H=1 is de
	// vbar_el1-encodering de redirect naar VBAR_EL2.
	WORD	$0xd518c007	// msr vbar_el1, x7
	WORD	$0xd5033fdf	// isb
	// m1n1's mmu_disable: SCTLR lezen, I/C/M maskeren, terugschrijven.
	// Onder E2H=1 is de sctlr_el1-encodering op EL2 de redirect naar
	// SCTLR_EL2 — dit zet dus ónze MMU en caches uit.
	WORD	$0xd5381002	// mrs x2, sctlr_el1
	BIC	$1, R2, R2	// M
	BIC	$4, R2, R2	// C
	BIC	$0x1000, R2, R2	// I
	WORD	$0xd5181002	// msr sctlr_el1, x2
	WORD	$0xd5033fdf	// isb
	// De veeg (m1n1's dcsw_op_all, in de by-VA-vorm): dc civac over het eigen
	// RAM-venster, regelgrootte uit CTR_EL0 — zelfde bron als dev.CleanInv.
	WORD	$0xd53b0022	// mrs x2, ctr_el0
	UBFX	$16, R2, $4, R2
	MOVD	$4, R6
	LSL	R2, R6, R6	// R6 = cacheregel in bytes
	SUB	$1, R6, R2
	BIC	R2, R4, R4	// start op regelgrens
sweep:
	WORD	$0xd50b7e24	// dc civac, x4
	ADD	R6, R4, R4
	CMP	R5, R4
	BLO	sweep
	WORD	$0xd5033f9f	// dsb sy
	// En de INSTRUCTIEcache — de stap die de eerste vier ijzer-flips de kop
	// kostte (01-09): het venster was net nog een app-partitie waar CODE
	// draaide. Zelfde blok als de app-drop en de SMP-secundaire (hygiene.h,
	// mét de Altra-les van 15-07 erin) — drie ingangen, één implementatie.
	I_HYGIENE
	JMP	(R16)
hvc:
	MOVD	R16, R2		// de handler verwacht entry in x0, firmware-x0 in x1
	MOVD	R0, R1
	MOVD	R2, R0
	WORD	$0xd4000042	// hvc #2
	B	-1(PC)		// onbereikbaar — de handler keert niet terug
