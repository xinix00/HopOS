package rk3566

import (
	"time"

	"github.com/xinix00/HopOS/metal/dev"
)

// De CRU-kant van het GMAC1: klokgates, de bronkeuze en — het stukje dat pas na
// autonegotiatie bekend is — de deler die de RGMII-klok op de linksnelheid zet.
//
// REFERENTIE (opgehaald 05-08): Linux v6.13 drivers/clk/rockchip/clk-rk3568.c
// (de GMAC1-tak van rk3568_clk_branches) met de offsets uit
// drivers/clk/rockchip/clk.h, plus rk3568_set_gmac_speed in dwmac-rk.c voor
// welke snelheid welke deler krijgt.
//
// De RK3566 deelt deze CRU met de RK3568; het GMAC1 hangt volledig in
// CLKSEL_CON(33) en CLKGATE_CON(17), wat het gelukkig één register per vraag
// maakt.
const (
	cruCLKSEL33  = 0x100 + 33*4 // RK3568_CLKSEL_CON(33) = 0x184
	cruCLKGATE17 = 0x300 + 17*4 // RK3568_CLKGATE_CON(17) = 0x344

	// CLKGATE_CON(17): Rockchip-gates zijn ACTIEF-LAAG (1 = klok uit) en
	// hiword-masked, net als de GRF.
	// aclk = de AXI-master van de DMA, pclk = de APB-kant voor de registers
	// (die stond al open, anders had de probe geen VERSION gelezen), 2top = de
	// 125MHz-bron en refout de klok die naar de PHY-kant gaat.
	gateACLKGMAC1     = 3  // aclk_gmac1
	gatePCLKGMAC1     = 4  // pclk_gmac1
	gateCLKMAC12TOP   = 5  // clk_mac1_2top
	gateCLKMAC1RefOut = 10 // clk_mac1_refout

	// CLKSEL_CON(33), alle vier de muxen van dit GMAC:
	//	[1:0] SCLK_GMAC1_RX_TX     0 = rgmii_speed, 1 = rmii_speed, 2 = xpcs
	//	[2]   SCLK_GMAC1           0 = clk_mac1_2top, 1 = gmac1_clkin
	//	[3]   SCLK_GMAC1_RMII_SPEED
	//	[5:4] SCLK_GMAC1_RGMII_SPEED  {clk, clk, clk/50, clk/5}
	//	[9:8] CLK_MAC1_2TOP        0 = cpll_125m, 1 = cpll_50m, 2 = cpll_25m, 3 = ppll
	selRXTXShift       = 0
	selGMAC1SrcShift   = 2
	selRGMIISpeedShift = 4
	selMAC12TopShift   = 8

	// De waarden die de DTS van dít bord voorschrijft:
	//	assigned-clock-parents = <&cru SCLK_GMAC1_RGMII_SPEED>, <&cru CLK_MAC1_2TOP>
	// dus RX_TX ← rgmii_speed (0) en SCLK_GMAC1 ← clk_mac1_2top (0), en 2top
	// staat op cpll_125m (0) — samen de 125MHz die de MDIO-deler (CSR 100-150M)
	// en de gigabit-RGMII-klok verwachten.
	selRXTXRGMII       = 0
	selGMAC1From2Top   = 0
	selMAC12TopCPLL125 = 0

	// RGMII_SPEED per linksnelheid. rk3568_set_gmac_speed vraagt 125MHz/25MHz/
	// 2,5MHz aan; met een 125MHz-bron zijn dat de delers /1, /5 en /50 — en die
	// zitten hier als muxstand, niet als deelfactor.
	rgmiiSpeed1000 = 0 // clk_gmac1 (125MHz)
	rgmiiSpeed100  = 3 // clk_gmac1_tx_div5 (25MHz)
	rgmiiSpeed10   = 2 // clk_gmac1_tx_div50 (2,5MHz)
)

// hiword bouwt een schrijfactie voor een hiword-masked veld: waarde in de
// onderste 16 bits, maskerbits erboven. Eén plek, want elke CRU- en
// GRF-schrijfactie in dit pakket heeft deze vorm.
func hiword(val, mask, shift uint32) uint32 {
	return val<<shift | mask<<(shift+16)
}

// GMACClockOn opent de klokgates die het GMAC1 nodig heeft en zet de bronkeuze
// zoals de DTS hem voorschrijft. Idempotent: gates zijn actief-laag en
// hiword-masked, dus een tweede aanroep is een no-op.
//
// PCLK stond al open (de probe las VERSION 0x3051 op 05-08); ACLK is de vraag
// die pas de DMA beantwoordt, en die openen we hier vóórdat de descriptors
// bestaan in plaats van erna te gokken.
func GMACClockOn() {
	var g uint32
	for _, b := range []uint32{gateACLKGMAC1, gatePCLKGMAC1, gateCLKMAC12TOP, gateCLKMAC1RefOut} {
		g |= hiword(0, 1, b) // 0 = klok AAN (actief-laag)
	}
	dev.Write32(CRUBase+cruCLKGATE17, g)

	dev.Write32(CRUBase+cruCLKSEL33,
		hiword(selRXTXRGMII, 0x3, selRXTXShift)|
			hiword(selGMAC1From2Top, 0x1, selGMAC1SrcShift)|
			hiword(selMAC12TopCPLL125, 0x3, selMAC12TopShift))
	dev.MB()
}

// GMACSetSpeed zet de RGMII-kloksnelheid na autonegotiatie. Zonder dit klokt de
// MAC bij 100Mbit nog op gigabit-tempo de lijn uit en komt er aan de overkant
// niets bruikbaars aan — de link staat dan wel, maar geen enkel frame landt.
func GMACSetSpeed(speed int) {
	sel := uint32(rgmiiSpeed1000)
	switch speed {
	case 100:
		sel = rgmiiSpeed100
	case 10:
		sel = rgmiiSpeed10
	}
	dev.Write32(CRUBase+cruCLKSEL33, hiword(sel, 0x3, selRGMIISpeedShift))
	dev.MB()
}

// De AXI-reset van het GMAC1. GEMETEN NOODZAAK 05-08: met alle klokken open,
// de pinmux gezet en de GRF op RGMII bleef de DMA-softreset van de MAC hangen —
// `bus mode 0x00000001`, de SFT_RESET-bit klaart nooit. Dat is precies het
// beeld van een blok waarvan de APB-kant leeft (VERSION las 0x3051 en al onze
// GRF-schrijfacties landden) maar de AXI-kant in reset staat: het registerbestand
// antwoordt, de DMA niet.
//
// De DTS zegt het ook, en dáár had ik het kunnen zien:
// `resets = <&cru SRST_A_GMAC1>; reset-names = "stmmaceth"` — Linux pulst die
// reset in stmmac_probe vóórdat het de DMA aanraakt. U-Boot niet, want die doet
// hier geen ethernet.
//
// Adres uit rk3568-cru.h (SRST_A_GMAC1 = 236) via de indeling van
// rockchip_register_softrst met ROCKCHIP_SOFTRST_HIWORD_MASK: bank = id/16 = 14,
// bit = id%16 = 12, register = SOFTRST_CON(0) + bank*4.
const (
	cruSOFTRST14 = 0x400 + 14*4 // RK3568_SOFTRST_CON(14) = 0x438
	srstAGMAC1   = 12
)

// GMACAXIReset pulst de AXI-reset van het GMAC1 en laat hem gedeassert achter.
// Aanroepen vóór de DMA-softreset van de driver.
func GMACAXIReset() {
	dev.Write32(CRUBase+cruSOFTRST14, hiword(1, 1, srstAGMAC1)) // assert
	dev.MB()
	time.Sleep(time.Millisecond)
	dev.Write32(CRUBase+cruSOFTRST14, hiword(0, 1, srstAGMAC1)) // deassert
	dev.MB()
	time.Sleep(time.Millisecond)
}

// GMACClocks leest de CRU-registers terug voor het meetinstrument: één regel die
// zegt welke gates open staan, welke muxstand er echt in het silicium zit en of
// de AXI-reset gedeassert is. Een gate of reset die bleef staan is anders niet te
// onderscheiden van een driver die verkeerd programmeert — dat kostte de eerste
// trede-3a-boot.
func GMACClocks() (clksel33, clkgate17, softrst14 uint32) {
	return dev.Read32(CRUBase + cruCLKSEL33),
		dev.Read32(CRUBase + cruCLKGATE17),
		dev.Read32(CRUBase + cruSOFTRST14)
}
