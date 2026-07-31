// rdtime — TIME CSR (0xC01), de read-only schaduw van mtime.
//
// Dit is het tijdpad voor de app-hart in het kooi-model: tamago's runtime
// schrijft nergens mtimecmp (alleen mtime-reads voor nanotime), dus met
// rdtime heeft een app GEEN CLINT-MMIO-venster nodig — en daarmee is het
// CLINT-DoS-kanaal (mtimecmp/msip van beide harts op één 4K-page) dicht.

#include "textflag.h"

// func rdtime() uint64
TEXT ·rdtime(SB),NOSPLIT|NOFRAME,$0-8
	WORD	$0xc0102573	// csrr a0, time
	MOV	A0, ret+0(FP)
	RET
