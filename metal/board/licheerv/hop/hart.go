// Package hop is de HOP-helft van het LicheeRV Nano-board: het deel van
// board.Board dat alleen de HOP-kern nodig heeft. Waar de ARM-boards hier
// PSCI en EL2-trampolines neerzetten, staat op dit board het SoC-reset-blok —
// dat is op RISC-V zowel de slot-start als de kill, en tegelijk het enige dat
// de kooi van het vorige slot weer wist (gemeten op de SG2002, 30-07).
//
// Registerrecept 1:1 uit de vendor-FSBL (plat/cv181x/platform.c, reset_c906l):
//
//	0x03003024 bit6 laag    → reset assert (stopt het hart, ook uit een tight loop)
//	0x020B0004 bit13 hoog   → boot-vector-override aan (SEC_SYS +0x04)
//	0x020B0020/24           → boot-vector lo/hi
//	0x03003024 bit6 hoog    → deassert = hart start op de vector
package hop

import (
	"fmt"

	"hop-os/metal/board"
	"hop-os/metal/board/licheerv"
)

const (
	c906lReset  = 0x03003024 // SoC-reset-blok, bit 6 = het C906L-hart
	secSysCtrl  = 0x020B0004 // SEC_SYS: bit 13 = boot-vector-override
	secSysVecLo = 0x020B0020
	secSysVecHi = 0x020B0024

	// hartC906L is het app-hart van dit board: de 700MHz "little" core. De
	// FSBL start óns image op de big core, dus dat is HOP's hart en deze is
	// het slot. (README-aanbeveling van de bring-up: rollen zo houden, want
	// dan is het proven vendor-reset-recept letterlijk ons mechanisme.)
	hartC906L = 1
	resetBit  = 1 << 6
)

// AppHarts geeft de harts die als app-slot te gebruiken zijn — op dit board
// precies één. Geen PSCI-telling om op terug te vallen: topologie is
// board-kennis.
func (machine) AppHarts() []int { return []int{hartC906L} }

// BootMode: HOP draait in M-mode (3). Minder kan niet — alleen M-mode
// programmeert PMP en reset harts, en zonder dat is er geen kooi.
func (machine) BootMode() int { return 3 }

// HartOn zet het hart op entry en laat hem lopen. Op een lópend hart is dit
// kill + verse start: de reset-assert stopt hem waar hij ook is, en wist zijn
// PMP-locks — precies wat een slot-restart nodig heeft.
func (machine) HartOn(hart int, entry uint64) error {
	if hart != hartC906L {
		return fmt.Errorf("licheerv: hart %d bestaat niet (alleen %d)", hart, hartC906L)
	}
	licheerv.Write32(c906lReset, licheerv.Read32(c906lReset)&^resetBit)
	licheerv.Write32(secSysCtrl, licheerv.Read32(secSysCtrl)|1<<13)
	licheerv.Write32(secSysVecLo, uint32(entry))
	licheerv.Write32(secSysVecHi, uint32(entry>>32))
	licheerv.Write32(c906lReset, licheerv.Read32(c906lReset)|resetBit)
	return nil
}

// HartOff houdt het hart in reset: kill zonder herstart (het slot is dan leeg
// en zijn locks zijn weg).
func (machine) HartOff(hart int) error {
	if hart != hartC906L {
		return fmt.Errorf("licheerv: hart %d bestaat niet (alleen %d)", hart, hartC906L)
	}
	licheerv.Write32(c906lReset, licheerv.Read32(c906lReset)&^resetBit)
	return nil
}

// HartState leest de powertoestand uit het reset-blok. Geen ON_PENDING op dit
// board: de deassert is meteen effectief.
func (machine) HartState(hart int) board.PowerState {
	if hart != hartC906L || licheerv.Read32(c906lReset)&resetBit == 0 {
		return board.PowerOff
	}
	return board.PowerOn
}
