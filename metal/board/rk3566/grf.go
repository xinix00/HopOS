package rk3566

import (
	"time"

	"github.com/xinix00/HopOS/metal/dev"
)

// De Rockchip-glue rond het GMAC: welke PHY-interface, welke klokdelays, en de
// PHY-reset. Dit is board/SoC-kennis en geen driverwerk — vandaar hier en niet
// in driver/nic/dwmac4, dat alleen de Synopsys-kant kent.
//
// REFERENTIE (opgehaald 05-08, narekend — niet uit het hoofd): Linux v6.13
// drivers/net/ethernet/stmicro/stmmac/dwmac-rk.c (rk3568_set_to_rgmii met
// RK3568_GRF_GMAC1_CON0/CON1 en de GRF_BIT/HIWORD_UPDATE-macro's),
// drivers/gpio/gpio-rockchip.c (de GPIO-v2-indeling), plus
// arch/arm64/boot/dts/rockchip/rk3566-radxa-zero-3e.dts en rk356x-base.dtsi voor
// de boardwaarden. De RK3566 gebruikt de rk3568-ops, en die tabel noemt zelf
// 0xfe010000 als gmac1 — precies het adres waar onze probe VERSION 0x3051 las.
const (
	// GRF (syscon@fdc60000) en de twee GMAC1-registers.
	grfGMAC1Con0 = 0x0388
	grfGMAC1Con1 = 0x038C

	// GRF-registers van Rockchip zijn "hiword-masked": de bovenste 16 bits zijn
	// een write-enable-masker voor de onderste 16. Alleen bits waarvan het
	// maskerbit staat veranderen — dus is een read-modify-write niet nodig, en
	// racet dit niet met wie er verder in deze GRF schrijft.
	//
	//	GRF_BIT(n)     = BIT(n) | BIT(n+16)          → zet bit n
	//	GRF_CLR_BIT(n) = BIT(n+16)                   → wis bit n
	//	HIWORD_UPDATE(v, mask, shift) = v<<shift | mask<<(shift+16)

	// CON0 = rx-delay [14:8] en tx-delay [6:0], beide met hun 0x7F-masker.
	// Dit bord heeft phy-mode = "rgmii-id" (de PHY doet de delays zelf), en
	// dwmac-rk roept dan set_to_rgmii(0, 0) aan — dus beide delays nul.
	//	HIWORD_UPDATE(0,0x7F,8) | HIWORD_UPDATE(0,0x7F,0)
	grfRGMIIDelaysZero = 0x7F7F0000

	// CON1 = PHY_INTF_SEL_RGMII (bit 4 zetten, 5 en 6 wissen) plus beide
	// delay-lijnen aan (bit 1 = rx, bit 0 = tx). Die twee zet rk3568_set_to_rgmii
	// óók bij delay 0 — vandaar hier ook; afwijken van het recept is geen
	// vereenvoudiging maar een gok.
	//	GRF_BIT(4)|GRF_CLR_BIT(5)|GRF_CLR_BIT(6)|GRF_BIT(1)|GRF_BIT(0)
	grfRGMIIMode = 0x00730013
)

// GMACSetRGMII zet het GMAC1-blok in RGMII-modus met nul-delays: precies de
// configuratie die de DTS van dít bord voorschrijft (rgmii-id).
func GMACSetRGMII() {
	dev.Write32(GRFBase+grfGMAC1Con0, grfRGMIIDelaysZero)
	dev.Write32(GRFBase+grfGMAC1Con1, grfRGMIIMode)
	dev.MB()
}

// GMACGlue leest de drie GRF-registers terug die de glue schrijft: de twee
// GMAC1-CON's en de M1-pinsetschakelaar. Alleen voor het meetinstrument — een
// hiword-masked schrijfactie die niet landt (verkeerd masker, verkeerd adres) is
// anders pas zichtbaar als een MAC die niets ontvangt.
func GMACGlue() (con0, con1, iofuncSel0 uint32) {
	return dev.Read32(GRFBase + grfGMAC1Con0),
		dev.Read32(GRFBase + grfGMAC1Con1),
		dev.Read32(GRFBase + grfIOFuncSel0)
}

// De ethernet-PHY hangt met zijn reset aan GPIO3 PC0, actief-laag:
//
//	rk3566-radxa-zero-3e.dts: reset-gpios = <&gpio3 RK_PC0 GPIO_ACTIVE_LOW>,
//	                          reset-assert-us = 20000, reset-deassert-us = 50000
//
// RK_PC0 = bankbit 16 (poort C = 2, dus 2*8+0).
//
// De RK356x-banken zijn de v2-GPIO-controller, en die is NIET zoals v1: het
// dataregister is gesplitst in een lage helft (bits 0..15) op +0x00 en een hoge
// (16..31) op +0x04, elk hiword-masked zoals de GRF. Bit 16 zit dus in de HOGE
// helft, en daar schrijf je bit 0 met maskerbit 16. Dat is niet cosmetisch: wie
// hier de v1-indeling gebruikt schrijft de DDR-richting in het int_en-register
// en trekt de reset nooit laag.
const (
	gpio3Base     = 0xFE760000 // gpio3 (rk356x-base.dtsi)
	gpioVersionID = 0x78       // v2-registertabel; op v1 gereserveerd
	gpioTypeV2    = 0x01000C2B // GPIO_TYPE_V2 en varianten delen de indeling

	// v2-indeling (gpio_regs_v2): DR op +0x00/+0x04, DDR op +0x08/+0x0C.
	gpioV2DRHi  = 0x04
	gpioV2DDRHi = 0x0C
	// v1-indeling (gpio_regs_v1), als terugval: DR +0x00, DDR +0x04, plain rmw.
	gpioV1DR  = 0x00
	gpioV1DDR = 0x04

	phyResetPin = 16 // RK_PC0
)

// gpioV2 vraagt het het silicium in plaats van het aan te nemen: version_id
// leest op een v2-bank de type-code en op een v1-bank iets anders. Zo staat er
// geen aanname over de generatie in de code die pas op het bord zichtbaar wordt.
func gpioV2() bool {
	id := dev.Read32(gpio3Base + gpioVersionID)
	return id>>24 == gpioTypeV2>>24
}

// gpioSetBit zet of wist één bit in een v2-registerpaar (hoge helft), of doet
// een read-modify-write op v1.
func gpioSetBit(v2 bool, hiOff, v1Off uintptr, pin uint, set bool) {
	if v2 {
		b := uint32(1) << (pin % 16)
		if set {
			dev.Write32(gpio3Base+hiOff, b|b<<16)
		} else {
			dev.Write32(gpio3Base+hiOff, b<<16)
		}
		return
	}
	v := dev.Read32(gpio3Base + v1Off)
	if set {
		v |= 1 << pin
	} else {
		v &^= 1 << pin
	}
	dev.Write32(gpio3Base+v1Off, v)
}

// GMACPHYReset houdt de PHY-reset 20ms laag en laat hem daarna 50ms los — de
// tijden uit de DTS. Zonder dit is er op een koude boot geen PHY op de MDIO-bus:
// U-Boot raakt het GMAC niet aan (hij meldt zelf "No ethernet found"), dus komt
// deze pin ongeïnitialiseerd bij ons aan.
//
// De pin moet uitgang zijn vóórdat de waarde iets betekent; de pinmux laten we
// staan, want GPIO is de reset-functie van deze pin (de DTS muxt hem niet om).
func GMACPHYReset() {
	v2 := gpioV2()
	gpioSetBit(v2, gpioV2DDRHi, gpioV1DDR, phyResetPin, true) // richting = uitgang
	gpioSetBit(v2, gpioV2DRHi, gpioV1DR, phyResetPin, false)  // assert (actief-laag)
	dev.MB()
	time.Sleep(20 * time.Millisecond)
	gpioSetBit(v2, gpioV2DRHi, gpioV1DR, phyResetPin, true) // release
	dev.MB()
	time.Sleep(50 * time.Millisecond)
}
