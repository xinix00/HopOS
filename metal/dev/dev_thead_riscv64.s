//go:build tamago

// CPU-PROFIEL IN DE BESTANDSNAAM. Dit is niet ISA-generiek RISC-V maar T-Head
// C906: de cache-instructies hieronder (th.dcache.*, th.sync.*) zijn
// XuanTie-uitbreidingen, geen standaard — de basis-ISA heeft geen cache-onderhoud
// (Zicbom kwam later en deze kern heeft het niet).
//
// Waarom de seam hier een BESTAND is en niet een import van cpu/thead, zoals
// kern/cage het doet: dev importeert per laag-regel niets (tools/importcheck), en
// assembly kan geen Go-constante lezen. Dus draagt de bestandsnaam het profiel, en
// is een tweede CPU een tweede bestand achter dezelfde build-tag.

#include "textflag.h"

// De riscv64-helft van de dev-primitieven. Let op: de cache-ops zijn de
// XuanTie-vendorextensie (T-Head C906/C910, pre-Zicbom) — encodings 1:1 uit de
// vendor-kernel (linux_5.10/arch/riscv/mm/cacheflush.c). Een generieke RISC-V
// core zou hier Zicbom (cbo.flush) gebruiken; zodra HopOS een tweede RISC-V
// board krijgt hoort dit dus achter een feature-check of per board.

// MB: volledige geheugenbarrière tussen harts.
TEXT ·MB(SB),NOSPLIT,$0
	FENCE
	RET

// SEV: op RISC-V bestaat geen SEV/WFE-event. Wekken doet het board (reset-blok
// of msip-IPI); wat hier telt is dat de mailbox-write zichtbaar is vóórdat de
// aanroeper wekt — dus de barrière.
TEXT ·SEV(SB),NOSPLIT,$0
	FENCE
	RET

// CleanInv veegt [addr, addr+size) uit de caches: th.dcache.cipa per regel
// (clean+invalidate by physical address) + th.sync.is — de vendor-vorm
// (linux_5.10/arch/riscv/mm/cacheflush.c gebruikt exact deze encodings).
//
// De PA-variant, en dat mag omdat er maar één soort aanroeper is: HOP, in de
// laag die hij zelf bezit, zonder translatie — daar ís een adres al fysiek. Een
// APP komt hier niet: zijn ABI-regio's zijn device gemapt (kern/slots slotMap),
// dus daar valt niets te onderhouden, en zijn Push/Pull zijn no-ops
// (share_riscv64_app.go) — precies zoals op ARM.
//
// Dat onderscheid is er niet voor de netheid: gaf een app hier een LINKadres aan
// een PA-op, dan veegde hij de cacheline van een héél ander slot. Met meerdere
// bewoners is dat geen slordigheid meer maar het weggooien van iemand anders zijn
// data.
TEXT ·CleanInv(SB),NOSPLIT,$0-16
	MOV	addr+0(FP), A0
	MOV	size+8(FP), A1
	BEQZ	A1, done
	ADD	A0, A1, A1	// A1 = einde (exclusief)
	MOV	$63, T0
	NOT	T0, T0
	AND	T0, A0, A0	// start op de 64B-regelgrens
loop:
	WORD	$0x02b5000b	// th.dcache.cipa a0
	ADD	$64, A0
	BLT	A0, A1, loop
	WORD	$0x01b0000b	// th.sync.is
done:
	RET
