//go:build gui

// Package dwc3 zet een Synopsys DesignWare USB3-core in hostmodus, zodat de
// xHCI-registers eronder betekenen wat ze horen te betekenen.
//
// WAAROM DIT NODIG IS. Een DWC3-core is niet alleen een xHCI: het is één blok
// dat óók een device-controller kan zijn. Welke van de twee hij is, staat in
// GCTL.PRTCAPDIR, en op een OTG-poort (de USB-C van de Radxa) is de defaultstand
// niet host. De xHCI-registers zijn dan aanwezig maar dood — je leest een nette
// CAPLENGTH en er gebeurt vervolgens niets. Dat is precies het soort stilte dat
// je op ijzer een avond kost, dus dit blok zet de modus expliciet.
//
// Het venster: 0x0000-0x7FFF zijn de xHCI-registers, 0xC100+ de globale
// registers van de core. Eén basisadres dus, twee registerwerelden.
//
// REFERENTIE: drivers/usb/dwc3/core.c (dwc3_core_soft_reset + dwc3_core_init)
// en de DWC_usb3 Programming Guide. De volgorde hieronder is die van de
// referentie; waar wij iets overslaan staat waarom.
package dwc3

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// Globale registers, absoluut binnen het corevenster.
const (
	gCtl         = 0xC110
	gSts         = 0xC118
	gSNPSID      = 0xC120
	gUSB2PHYCfg0 = 0xC200
	gUSB3PipeCtl = 0xC2C0
)

// GCTL-bits (DWC_usb3 §6.1.1.5).
const (
	ctlDisClkGating  = 1 << 0
	ctlDisScramble   = 1 << 3
	ctlScaleDownMask = 0x3 << 4
	ctlCoreSoftReset = 1 << 11
	ctlPrtCapMask    = 0x3 << 12
	ctlPrtCapHost    = 1 << 12
)

// GUSB2PHYCFG- en GUSB3PIPECTL-bits.
const (
	u2SusPHY       = 1 << 6
	u2EnblSlpM     = 1 << 8
	u2TrdTimMask   = 0xF << 10
	u2TrdTim16Bit  = 5 << 10
	u2TrdTim8Bit   = 9 << 10
	u2PhyIf16Bit   = 1 << 3
	u2PhySoftReset = 1 << 31

	u3SusPHY       = 1 << 17
	u3PhySoftReset = 1 << 31
)

// Core is één DWC3-blok.
type Core struct {
	Base uintptr
	Name string
}

// ID leest GSNPSID: de bovenste 16 bits zijn de familie (0x5533 = DWC_usb3,
// 0x3331 = DWC_usb31, 0x3332 = DWC_usb32), de onderste de revisie. Nul of
// all-ones betekent dat het venster niets terugpraat.
func (c *Core) ID() uint32 { return dev.Read32(c.Base + gSNPSID) }

// HostMode voert de volledige bring-up uit: soft reset van core en PHY's, dan
// de core in hostmodus met de suspend-bits uit.
//
// De suspend-bits (SUSPHY) zijn geen detail. Staan ze aan, dan mag de PHY zich
// slapend leggen zodra er even niets gebeurt — en zonder interruptpad om hem te
// wekken (wij pollen) blijft hij dat. Linux zet ze pas aan nadat het hele
// apparaat draait; wij hebben ze niet nodig en laten ze uit.
func (c *Core) HostMode() error {
	id := c.ID()
	switch id >> 16 {
	case 0x5533, 0x3331, 0x3332:
	default:
		return fmt.Errorf("dwc3 %s: GSNPSID %#08x op %#x — geen DWC3-core (0 = niet geklokt, 0xFF..= dode bus)",
			c.Name, id, c.Base)
	}

	// Core in reset vóórdat de PHY's eruit gaan: een PHY-reset onder een
	// lopende core is undefined (core.c, dwc3_core_soft_reset).
	c.mod(gCtl, 0, ctlCoreSoftReset)
	c.mod(gUSB3PipeCtl, 0, u3PhySoftReset)
	c.mod(gUSB2PHYCfg0, 0, u2PhySoftReset)
	time.Sleep(100 * time.Millisecond)
	c.mod(gUSB3PipeCtl, u3PhySoftReset, 0)
	c.mod(gUSB2PHYCfg0, u2PhySoftReset, 0)
	time.Sleep(100 * time.Millisecond)
	c.mod(gCtl, ctlCoreSoftReset, 0)

	// PHY's wakker en op volle snelheid. De UTMI-breedte lezen we uit het
	// register zelf: die is door de integrator vastgedraad en de turnaround-tijd
	// hoort erbij (8-bit = 9, 16-bit = 5 — core.c dwc3_phy_setup).
	v := dev.Read32(c.Base + gUSB2PHYCfg0)
	v &^= u2SusPHY | u2EnblSlpM | u2TrdTimMask
	if v&u2PhyIf16Bit != 0 {
		v |= u2TrdTim16Bit
	} else {
		v |= u2TrdTim8Bit
	}
	dev.Write32(c.Base+gUSB2PHYCfg0, v)
	c.mod(gUSB3PipeCtl, u3SusPHY, 0)

	// De core zelf: geen scaledown (dat is een simulatiestand), scrambling aan,
	// klokpoorten aan, en de poort als HOST.
	g := dev.Read32(c.Base + gCtl)
	g &^= ctlScaleDownMask | ctlDisScramble | ctlDisClkGating | ctlPrtCapMask
	g |= ctlPrtCapHost
	dev.Write32(c.Base+gCtl, g)
	dev.MB()

	// De modewissel heeft even nodig voor de poortlogica hem volgt.
	time.Sleep(25 * time.Millisecond)
	return nil
}

// Regs geeft de globale registers voor één diagnoseregel.
func (c *Core) Regs() (id, ctl, u2, u3, sts uint32) {
	return dev.Read32(c.Base + gSNPSID), dev.Read32(c.Base + gCtl),
		dev.Read32(c.Base + gUSB2PHYCfg0), dev.Read32(c.Base + gUSB3PipeCtl),
		dev.Read32(c.Base + gSts)
}

func (c *Core) mod(off uintptr, clear, set uint32) {
	dev.Write32(c.Base+off, dev.Read32(c.Base+off)&^clear|set)
	dev.MB()
}
