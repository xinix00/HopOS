package mdio

// De Realtek RTL8211F: de gigabit-PHY van de Radxa Zero 3E, en een van de
// meestgebruikte RGMII-PHY's die er zijn. Hij staat hier en niet in een
// board-pakket omdat een PHY-eigenaardigheid bij de PHY hoort — het volgende
// bord met dezelfde chip krijgt hem gratis.
//
// WAAROM DIT NODIG IS, gemeten op ijzer 06-08: met alleen de MAC-kant goed
// (RGMII-modus in de GRF, klokken, pinmux) kwam er een gigabit-link tot stand en
// werkten 1124 ontvangen frames foutloos — maar geen enkel VERZONDEN frame
// bereikte iets. Klassiek beeld van een RGMII-link waarvan alleen de TX-kant
// zijn klokvertraging mist: de switch aan de overkant bemonstert de data op het
// verkeerde moment en gooit alles weg als CRC-fout, terwijl de RX-richting
// (waar de PHY zijn delay wél had) prima loopt.
//
// De MAC kan die delay niet voor je regelen. `phy-mode = "rgmii-id"` betekent
// letterlijk: de PHY doet de delays, de MAC zet ze op nul. Doet de PHY het dan
// niet, dan doet niemand het.
//
// REFERENTIE (opgehaald 06-08): Linux drivers/net/phy/realtek.c —
// `rtl8211f_config_init`, en de PHY-id-tabel waarin `0x001cc916` letterlijk
// "RTL8211F Gigabit Ethernet" heet. Dat is precies het id dat onze MDIO-scan las.
//
// LET OP: de driver zegt er zelf bij dat de bestaande stand uit "pin-strapping
// RXD1 or the bootloader" komt. Er is dus géén betrouwbare default — je moet
// beide bits expliciet zetten, ook als je denkt dat ze goed staan.
const (
	// PHY-id van de RTL8211F: register 2 en 3 van de clause-22-ruimte.
	RTL8211FID1 = 0x001C
	RTL8211FID2 = 0xC916

	// Realtek zet zijn extra registers achter een pagina-selector op reg 0x1F.
	// Pagina 0 is de standaard clause-22-ruimte; vergeet je terug te schakelen,
	// dan leest de rest van de wereld (BMCR, BMSR, autoneg) rommel.
	rtlPageSelect = 0x1F
	rtlPageDelay  = 0xD08
	rtlPageStd    = 0x0000

	rtlRegTXDelay = 0x11
	rtlBitTXDelay = 1 << 8

	rtlRegRXDelay = 0x15
	rtlBitRXDelay = 1 << 3
)

// IsRTL8211F zegt of een gescande PHY-id deze chip is.
func IsRTL8211F(id1, id2 uint16) bool {
	return id1 == RTL8211FID1 && id2 == RTL8211FID2
}

// ConfigureRTL8211F zet de RGMII-klokvertragingen in de PHY.
//
// Voor `phy-mode = "rgmii-id"` (wat de Radxa Zero 3E gebruikt) zijn beide true.
// De varianten: "rgmii" = beide false, "rgmii-rxid" = alleen rx, "rgmii-txid" =
// alleen tx — precies de vier gevallen uit rtl8211f_config_init.
//
// Read-modify-write per register, want er staan meer bits in die we niet kennen
// en dus niet mogen weggooien. En de pagina gaat gegarandeerd terug naar 0, ook
// als er onderweg iets misgaat — een PHY die op pagina 0xd08 blijft staan
// beantwoordt geen enkele normale clause-22-vraag meer.
func ConfigureRTL8211F(m MDIO, phy int, txDelay, rxDelay bool) {
	m.MDIOWrite(phy, rtlPageSelect, rtlPageDelay)
	defer m.MDIOWrite(phy, rtlPageSelect, rtlPageStd)

	setBit(m, phy, rtlRegTXDelay, rtlBitTXDelay, txDelay)
	setBit(m, phy, rtlRegRXDelay, rtlBitRXDelay, rxDelay)
}

// RTL8211FDelays leest de twee delay-bits terug. Voor het meetinstrument: de
// stand vóór onze schrijfactie zegt wat pin-strapping of de bootloader had
// achtergelaten, en dát is de informatie die de Linux-driver alleen als
// debug-regel logt.
func RTL8211FDelays(m MDIO, phy int) (tx, rx bool) {
	m.MDIOWrite(phy, rtlPageSelect, rtlPageDelay)
	tx = m.MDIORead(phy, rtlRegTXDelay)&rtlBitTXDelay != 0
	rx = m.MDIORead(phy, rtlRegRXDelay)&rtlBitRXDelay != 0
	m.MDIOWrite(phy, rtlPageSelect, rtlPageStd)
	return
}

func setBit(m MDIO, phy, reg int, bit uint16, on bool) {
	v := m.MDIORead(phy, reg)
	if on {
		v |= bit
	} else {
		v &^= bit
	}
	m.MDIOWrite(phy, reg, v)
}
