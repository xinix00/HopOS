package licheerv

// CV181x clock-blok (0x03002000) — C906 big-core klok, voor terugklokken
// van de 24/7 printserver-node (registers uit clk-cv181x.c):
//
//	clk_c906_0: gate REG_CLK_EN_4(0x010) bit13
//	  div-branch 0: 0x130, veld [19:16] (width 4), src-mux bits [10:8]
//	  div-branch 1: 0x134, veld [19:16]
//	  branch-select: REG_CLK_SEL_0(0x020) bit23 — GEÏNVERTEERD:
//	    bit=1 → branch 0, bit=0 → branch 1 (cv181x_clk_get_clk_sel)
//	  bypass naar osc(25MHz): REG_CLK_BYP_1(0x034) bit6
//	  divider-write: veld vervangen + BIT(3) zetten (update-strobe)
//
// De CLINT-timebase is de vaste 25MHz osc — terugklokken raakt de
// tijdrekening dus NIET (time.Sleep etc. blijven kloppen).

import (
	"fmt"
)

const (
	clkBase = 0x03002000

	regClkSel0     = clkBase + 0x020
	regClkByp1     = clkBase + 0x034
	regDivC906_0_0 = clkBase + 0x130
	regDivC906_0_1 = clkBase + 0x134

	divShift  = 16
	divMask   = 0xf << divShift
	divStrobe = 1 << 3
)

// coreDivReg geeft het actieve divider-register van de big core.
func coreDivReg() uint64 {
	if read32(regClkSel0)>>23&1 == 1 {
		return regDivC906_0_0 // sel-bit geïnverteerd: 1 → branch 0
	}
	return regDivC906_0_1
}

// CoreClockRegs dumpt de ruwe klok-registers (voor de probe; op hardware
// invullen welke PLL de parent is vóór we absolute MHz claimen).
func CoreClockRegs() string {
	return fmt.Sprintf("sel0=%08x byp1=%08x div0=%08x div1=%08x actief=%08x",
		read32(regClkSel0), read32(regClkByp1),
		read32(regDivC906_0_0), read32(regDivC906_0_1),
		read32(coreDivReg()))
}

// CoreDiv leest de huidige divider van de actieve branch.
func CoreDiv() uint32 {
	return read32(coreDivReg()) & divMask >> divShift
}

// SetCoreDiv zet de divider van de actieve branch (1..15). Groter =
// langzamer = koeler; bij div op de 1GHz-parent: 2 → ~500MHz, 4 → ~250MHz.
// Diverders zijn glitch-vrij; de PLL zelf blijft onaangeroerd.
func SetCoreDiv(div uint32) {
	if div < 1 || div > 15 {
		return
	}
	reg := coreDivReg()
	write32(reg, read32(reg)&^uint32(divMask)|div<<divShift|divStrobe)
}

// BenchCore meet de relatieve corefrequentie: spin-iteraties per 10ms
// mtime (25MHz vast). Vóór/na SetCoreDiv aanroepen geeft de echte ratio.
func BenchCore() uint64 {
	end := Mtime() + 250000 // 10ms bij 25MHz
	n := uint64(0)
	for Mtime() < end {
		n++
	}
	return n
}
