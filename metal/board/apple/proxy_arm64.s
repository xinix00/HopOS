// proxyBoot — de sprong van de mini-proxy (proxy.go): MMU en caches uit, en dan
// het geladen image binnen op zijn stub-entry, met x0 zoals de firmware hem gaf.
//
// Dit is dezelfde overgang die cpuinit.s maakt (dáár mrs/msr sctlr_el2 met de
// M/C/I-bits eruit), maar dan de andere kant op: wij geven de machine terug in
// de staat waarin iBoot een bootobject aflevert. Dat mag hier zonder truc met
// een sprongvenster, omdat onze wereld vlak is — VA == PA voor dit image, dus
// de PC blijft na het uitzetten van de vertaling op precies dezelfde
// instructie staan.
//
// Het cache-onderhoud over het geladen image staat in proxy.go (dev.CleanInv);
// hier volgt alleen nog de instructie-cache, want vanaf de sprong wordt er code
// gelezen die zojuist als DATA is geschreven.

//go:build tamago && arm64

#include "textflag.h"

// func proxyBoot(entry, x0 uint64)
TEXT ·proxyBoot(SB),NOSPLIT|NOFRAME,$0-16
	MOVD	entry+0(FP), R9
	MOVD	x0+8(FP), R10

	WORD	$0xd5033f9f		// dsb sy
	WORD	$0xd508751f		// ic iallu
	WORD	$0xd5033fdf		// isb

	// MMU en caches uit — pariteit met cpuinit.s regel 87-92.
	WORD	$0xd53c1005		// mrs x5, sctlr_el2
	BIC	$1<<0, R5		// M — vertaling
	BIC	$1<<2, R5		// C — data-cache
	BIC	$1<<12, R5		// I — instructie-cache
	WORD	$0xd51c1005		// msr sctlr_el2, x5
	WORD	$0xd5033fdf		// isb
	WORD	$0xd5033f9f		// dsb sy

	// x0 = boot_args, precies zoals de firmware ons afleverde; de rest van de
	// argumentregisters op nul, zoals iBoot dat doet.
	MOVD	R10, R0
	MOVD	$0, R1
	MOVD	$0, R2
	MOVD	$0, R3
	JMP	(R9)
