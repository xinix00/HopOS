package hop

import (
	"time"

	"github.com/xinix00/HopOS/metal/dev"
)

// De SoC-glue van het ethernet: klokgates en de interne 100M-ePHY. Dit is
// board-kennis en staat daarom hier, niet in driver/nic/dwmac — dat pakket is
// de Synopsys-IP en niets meer, precies zoals gem en genet.
//
// Het init-recept van de ePHY is 1:1 overgenomen uit de vendor-U-Boot-driver
// (u-boot-2021.10/drivers/net/phy/cvitek.c, cv182xa_ephy_init) inclusief de
// stappen die daar in board.c staan (shutdown-release, dig_rst_n, PHY_ID). Wij
// hebben geen U-Boot meer onder ons, dus doen we de hele keten zelf. Niet
// herschikken zonder bord erbij: het zijn analoge kalibratiestappen in een
// vaste volgorde, geen registers met een betekenis die we kennen.
//
// Dat het recept aankomt is op ijzer gemeten (probe 30-07): ná deze init
// antwoordt MDIO-adres 0 met id 0043:5649 — en dat id schrijft deze functie
// zélf in de PHY (page 0, 0x08/0x0c). Als de ePHY niet zou luisteren, zou daar
// 0xffff staan.
const (
	// Klokgates in het CV181x-klokblok: clk_500m_eth0 en clk_axi4_eth0 zijn
	// in Linux CLK_IS_CRITICAL en staan na de FSBL al open (gemeten) — dit is
	// de zekerheid voor het geval een andere FSBL ze dicht laat.
	clkEn0      = 0x03002000
	clk500mEth0 = 1 << 25
	clkAxi4Eth0 = 1 << 26

	ephyBase   = 0x03009000
	ephyCtl    = ephyBase + 0x800 // shutdown / dig_rst_n / ana_rst_n / an-start
	ephyAPBSel = ephyBase + 0x804 // 1 = registers via APB, 0 = MDIO via de MAC
	ephyPage   = ephyBase + 0x07c // page-select (page << 8)

	// Fabriekskalibratie voor de TX-analoog, met geldigheidsbits. Ontbreekt
	// die, dan zijn de vendor-defaults de terugval (zoals cvitek.c doet).
	efuseValid    = 0x03050120
	efuseTune20   = 0x03051020
	efuseTune24   = 0x03051024
	efuseTxEchoRC = 1 << 8
	efuseTxITune  = 1 << 9
	efuseTxRxTerm = 1 << 11
)

// ethClocksOn meldt of beide ethernet-klokgates open staan.
func ethClocksOn() bool {
	return dev.Read32(clkEn0)&(clk500mEth0|clkAxi4Eth0) == clk500mEth0|clkAxi4Eth0
}

// ethClocksEnable opent de ethernet-klokgates.
func ethClocksEnable() {
	dev.Write32(clkEn0, dev.Read32(clkEn0)|clk500mEth0|clkAxi4Eth0)
}

func ephyW(off uintptr, v uint32) { dev.Write32(ephyBase+off, v) }
func ephyR(off uintptr) uint32    { return dev.Read32(ephyBase + off) }
func ephyPageSel(p uint32)        { dev.Write32(ephyPage, p<<8) }

// ephyInit doet de volledige power-on plus analoge kalibratie van de interne
// PHY en geeft de MDIO-controle daarna terug aan de MAC.
func ephyInit() {
	dev.Write32(ephyAPBSel, 0x0001) // registers via APB

	// shutdown-release, dan dig_rst_n (vendor board.c).
	dev.Write32(ephyCtl, 0x0900)
	time.Sleep(2 * time.Millisecond)
	dev.Write32(ephyCtl, 0x0904)
	time.Sleep(10 * time.Millisecond)

	// Analoog aan: PD los, EN los, PLL laten locken, dan ana_rst_n.
	ephyPageSel(0x05)
	ephyW(0x40, 0x0c00)
	ephyW(0x40, 0x0c7e)
	time.Sleep(1 * time.Millisecond)
	dev.Write32(ephyCtl, 0x0906)

	// TX-tuning: efuse als die geldig is, anders de vendor-defaults.
	ephyPageSel(0x05)
	valid := dev.Read32(efuseValid)

	if valid&efuseTxITune != 0 {
		v := (dev.Read32(efuseTune24)>>24)&0xff | ((dev.Read32(efuseTune24)>>16)&0xff)<<8
		ephyW(0x64, ephyR(0x64)&^0xffff|v)
	} else {
		ephyW(0x64, 0x5a5a)
	}
	if valid&efuseTxEchoRC != 0 {
		ephyW(0x54, ephyR(0x54)&^0xff00|((dev.Read32(efuseTune24)>>8)&0xff)<<8)
	} else {
		ephyW(0x54, 0x0000)
	}
	if valid&efuseTxRxTerm != 0 {
		v := ((dev.Read32(efuseTune20)>>28)&0xf)<<4 | ((dev.Read32(efuseTune20)>>24)&0xf)<<8
		ephyW(0x58, ephyR(0x58)&^0xff0|v)
	} else {
		ephyW(0x58, 0x0bb0)
	}

	// 100BaseT rise/fall.
	ephyW(0x5c, 0x0c10)
	ephyW(0x68, 0x0003)
	ephyW(0x54, 0x0000)

	// MLT3-vormtabellen: positieve fase (page 16), negatieve fase (page 17).
	ephyPageSel(0x10)
	for i, v := range []uint32{0x1000, 0x3020, 0x5040, 0x7060} {
		ephyW(uintptr(0x68+4*i), v)
	}
	for i, v := range []uint32{0x1708, 0x3827, 0x5748, 0x7867} {
		ephyW(uintptr(0x58+4*i), v)
	}
	ephyPageSel(0x11)
	for i, v := range []uint32{
		0x9080, 0xb0a0, 0xd0c0, 0xf0e0, 0x9788, 0xb8a7, 0xd7c8, 0xf8e7,
	} {
		ephyW(uintptr(0x40+4*i), v)
	}

	// TX_Rterm aan + RX-vcm.
	ephyPageSel(0x05)
	ephyW(0x40, ephyR(0x40)|0x0001)
	ephyW(0x4c, ephyR(0x4c)|0x0820)

	// Link-pulsvorm (page 10) en TP_IDLE (page 11).
	ephyPageSel(0x0a)
	for i, v := range []uint32{
		0x3e00, 0x7864, 0x6470, 0x5f62, 0x5a5a, 0x5458, 0xb23a,
		0x94a0, 0x9092, 0x8a8e, 0x8688, 0x8484, 0x0082,
	} {
		ephyW(uintptr(0x40+4*i), v)
	}
	ephyPageSel(0x0b)
	for i, v := range []uint32{
		0x5252, 0x5252, 0x4b52, 0x3d47, 0xaa99, 0x989e, 0x9395,
		0x9091, 0x8e8f, 0x8d8e, 0x8c8c, 0x8b8b, 0x008a,
	} {
		ephyW(uintptr(0x40+4*i), v)
	}

	// 10BaseT-datavormen (pages 13..16).
	ephyPageSel(0x0d)
	for i, v := range []uint32{0x1e0a, 0x3862, 0x1e62, 0x2a08, 0x244c, 0x1a44, 0x061c} {
		ephyW(uintptr(0x40+4*i), v)
	}
	ephyPageSel(0x0e)
	for i, v := range []uint32{0x2d30, 0x3470, 0x0648, 0x261c, 0x3160, 0x2d5e} {
		ephyW(uintptr(0x40+4*i), v)
	}
	ephyPageSel(0x0f)
	for i, v := range []uint32{0x2922, 0x366e, 0x0752, 0x2556, 0x2348, 0x0c30} {
		ephyW(uintptr(0x40+4*i), v)
	}
	ephyPageSel(0x10)
	for i, v := range []uint32{0x1e08, 0x3868, 0x1462, 0x1a0e, 0x305e, 0x2f62} {
		ephyW(uintptr(0x40+4*i), v)
	}

	// LED: LNK/SPD/DPX naar het LED-pad (page 1).
	ephyPageSel(0x01)
	ephyW(0x68, ephyR(0x68)&^0x0f00)

	// PHY_ID zetten (vendor board.c) — hierdoor vindt mdio.Scan 0043:5649.
	ephyPageSel(0x00)
	ephyW(0x08, 0x0043)
	ephyW(0x0c, 0x5649)

	// AGC-swing (page 19) en de LPF/HPF-filters van de CV181x (page 18).
	ephyPageSel(0x13)
	ephyW(0x58, 0x0012)
	ephyW(0x5c, 0x6848)
	ephyPageSel(0x12)
	ephyW(0x48, 0x0808)
	ephyW(0x4c, 0x0808)
	ephyW(0x50, 0x32f8)
	ephyW(0x54, 0xf8dc)

	// Terug naar page 0, autoneg starten, full duplex adverteren.
	ephyPageSel(0x00)
	dev.Write32(ephyCtl, 0x090e)
	ephyW(0x00, ephyR(0x00)|0x100)

	dev.Write32(ephyAPBSel, 0x0000) // MDIO-controle terug naar de MAC
}
