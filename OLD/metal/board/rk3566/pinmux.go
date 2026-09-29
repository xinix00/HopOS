package rk3566

import "github.com/xinix00/HopOS/metal/v2/dev"

// Pinmux voor het GMAC1. U-Boot laat dit liggen (hij meldt zelf "No ethernet
// found", dus zijn ethernet-driver komt nooit aan de pinnen), dus moeten wij de
// vijftien pinnen zelf naar hun ethernet-functie zetten — anders praat een
// perfect geconfigureerde MAC tegen GPIO's.
//
// REFERENTIE (opgehaald 05-08): Linux v6.13
// drivers/pinctrl/pinctrl-rockchip.c (rk3568_pin_banks + de offset-berekening in
// rockchip_pinctrl_get_soc_data, plus rk3568_mux_route_data) en
// arch/arm64/boot/dts/rockchip/rk3568-pinctrl.dtsi voor de pinlijst zelf.
//
// DIT BORD GEBRUIKT DE M1-PINGROEP. rk3566-radxa-zero-3e.dts kiest
// gmac1m1_miim/tx_bus2/rx_bus2/rgmii_clk/rgmii_bus/clkinout — niet de m0-groep.
// Dat kost naast de vijftien pin-muxen óók één routebit (zie grfIOFuncSel0):
// de RK3568/66 heeft twee fysieke pinsets voor GMAC1 en een aparte
// selectieschakelaar ertussen. Alleen de pinnen muxen zonder die schakelaar
// levert een MAC waarvan de MDIO nooit antwoordt.
const (
	// SYS-GRF: iomux van bank 1..4, 0x20 per bank (vier groepen van 8 pinnen,
	// elk twee 32-bit registers omdat 4 bits per pin er maar vier in een
	// hiword-masked woord laten passen).
	grfIOMuxBank1 = 0x0000

	// GRF_IOFUNC_SEL0: bit 8 kiest de GMAC1-pinset (0 = M0, 1 = M1).
	// pinctrl-rockchip.c: RK_MUXROUTE_GRF(4, RK_PA7, 3, 0x0300,
	//                       WRITE_MASK_VAL(8, 8, 1)) /* GMAC1 IO mux M1 */
	grfIOFuncSel0 = 0x0300
	grfGMAC1MuxM1 = 1<<8 | 1<<24 // WRITE_MASK_VAL(8,8,1)
	pmuGRFBase    = 0xFDC20000   // iomux van bank 0 zit in de PMU-GRF
	muxFuncGMAC   = 3            // alle gmac1m1-pinnen gebruiken functie 3
	muxFuncGPIO   = 0
)

// pin is één pinmux-instelling: bank, bankbit (RK_Pxy = poort*8 + nr) en functie.
type pin struct {
	bank, bit, fn uint32
}

// gmac1M1Pins is de volledige gmac1m1-pinlijst uit rk3568-pinctrl.dtsi, in
// dezelfde groepen als de DTS ze noemt zodat een lezer ze naast elkaar kan
// leggen. RK_PA0..PA7 = 0..7, PB0..PB7 = 8..15, PC0..PC7 = 16..23, PD = 24..31.
var gmac1M1Pins = []pin{
	// gmac1m1_miim: mdc, mdio
	{4, 8 + 6, muxFuncGMAC}, // RK_PB6 gmac1_mdcm1
	{4, 8 + 7, muxFuncGMAC}, // RK_PB7 gmac1_mdiom1
	// gmac1m1_tx_bus2: txd0, txd1, txen
	{4, 4, muxFuncGMAC}, // RK_PA4 gmac1_txd0m1
	{4, 5, muxFuncGMAC}, // RK_PA5 gmac1_txd1m1
	{4, 6, muxFuncGMAC}, // RK_PA6 gmac1_txenm1
	// gmac1m1_rx_bus2: rxd0, rxd1, rxdv
	{4, 7, muxFuncGMAC},     // RK_PA7 gmac1_rxd0m1
	{4, 8 + 0, muxFuncGMAC}, // RK_PB0 gmac1_rxd1m1
	{4, 8 + 1, muxFuncGMAC}, // RK_PB1 gmac1_rxdvcrsm1
	// gmac1m1_rgmii_clk: rxclk, txclk
	{4, 3, muxFuncGMAC}, // RK_PA3 gmac1_rxclkm1
	{4, 0, muxFuncGMAC}, // RK_PA0 gmac1_txclkm1
	// gmac1m1_rgmii_bus: rxd2, rxd3, txd2, txd3 — let op: de twee txd's zitten
	// in bank 3, niet 4. Precies het soort detail waar een pinlijst uit het
	// hoofd stukloopt.
	{4, 1, muxFuncGMAC},      // RK_PA1 gmac1_rxd2m1
	{4, 2, muxFuncGMAC},      // RK_PA2 gmac1_rxd3m1
	{3, 24 + 6, muxFuncGMAC}, // RK_PD6 gmac1_txd2m1
	{3, 24 + 7, muxFuncGMAC}, // RK_PD7 gmac1_txd3m1
	// gmac1m1_clkinout: de RGMII-referentieklok (clock_in_out = "input")
	{4, 16 + 1, muxFuncGMAC}, // RK_PC1 gmac1_mclkinoutm1

	// En de PHY-reset blijft GPIO — de DTS muxt hem expliciet zo
	// (gmac1_rstn: <3 RK_PC0 RK_FUNC_GPIO>). Hem hier meenemen is geen
	// overbodigheid: als U-Boot die pin voor iets anders gebruikte, trekt onze
	// GMACPHYReset anders niets laag.
	{3, 16 + 0, muxFuncGPIO}, // RK_PC0 gmac1-reset
}

// iomuxReg geeft het GRF-registeradres en de bitshift voor één pin.
//
// Vier bits per pin, vier pinnen per register (de bovenste 16 bits zijn het
// write-masker), dus twee registers per groep van 8 en 0x20 per bank. Bank 0
// hangt aan de PMU-GRF, 1..4 aan de SYS-GRF vanaf offset 0.
func iomuxReg(bank, bit uint32) (addr uintptr, shift uint32) {
	base := uintptr(GRFBase) + grfIOMuxBank1 + uintptr(bank-1)*0x20
	if bank == 0 {
		base = pmuGRFBase
	}
	off := uintptr(bit/8)*8 + uintptr((bit%8)/4)*4
	return base + off, (bit % 4) * 4
}

// setMux zet één pin op een functie. Hiword-masked, dus geen read-modify-write
// en geen race met wie er verder in deze GRF schrijft.
func setMux(p pin) {
	addr, shift := iomuxReg(p.bank, p.bit)
	dev.Write32(addr, 0xF<<(shift+16)|p.fn<<shift)
}

// GMACPinmux zet alle GMAC1-pinnen op hun ethernet-functie en schakelt de
// M1-pinset in. Aanroepen vóór GMACSetRGMII en vóór de eerste MDIO-transactie.
func GMACPinmux() {
	dev.Write32(GRFBase+grfIOFuncSel0, grfGMAC1MuxM1)
	for _, p := range gmac1M1Pins {
		setMux(p)
	}
	dev.MB()
}

// MuxOf leest de huidige functie van één pin terug. Alleen voor het
// meetinstrument: hiermee zien we in één boot of U-Boot iets gemuxt had en of
// onze schrijfactie aankwam — een pinmux die stil niet landt is anders pas
// zichtbaar als een MAC die niets ontvangt.
func MuxOf(bank, bit uint32) uint32 {
	addr, shift := iomuxReg(bank, bit)
	return dev.Read32(addr) >> shift & 0xF
}

// GMACPins geeft de pinlijst aan het meetinstrument (bank, bit, verwachte
// functie), zodat de probe hem kan aflopen zonder de tabel te dupliceren.
func GMACPins() (banks, bits, fns []uint32) {
	for _, p := range gmac1M1Pins {
		banks = append(banks, p.bank)
		bits = append(bits, p.bit)
		fns = append(fns, p.fn)
	}
	return
}
