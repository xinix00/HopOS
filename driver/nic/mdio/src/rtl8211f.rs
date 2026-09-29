//! De Realtek RTL8211F: de gigabit-PHY van de Radxa Zero 3E, en een van de
//! meestgebruikte RGMII-PHY's die er zijn.
//!
//! WAAROM DIT NODIG IS, gemeten op ijzer 06-08: met alleen de MAC-kant goed
//! (RGMII-modus in de GRF, klokken, pinmux) kwam er een gigabit-link tot
//! stand en werkten 1124 ontvangen frames foutloos, maar geen enkel
//! VERZONDEN frame bereikte iets. Klassiek beeld van een RGMII-link waarvan
//! alleen de TX-kant zijn klokvertraging mist: de switch aan de overkant
//! bemonstert de data op het verkeerde moment en gooit alles weg als
//! CRC-fout, terwijl de RX-richting (waar de PHY zijn delay wél had) prima
//! loopt.
//!
//! De MAC kan die delay niet voor je regelen. `phy-mode = "rgmii-id"`
//! betekent letterlijk: de PHY doet de delays, de MAC zet ze op nul. Doet de
//! PHY het dan niet, dan doet niemand het.
//!
//! REFERENTIE (opgehaald 06-08): Linux `drivers/net/phy/realtek.c`,
//! `rtl8211f_config_init`, en de PHY-id-tabel waarin `0x001cc916` letterlijk
//! "RTL8211F Gigabit Ethernet" heet. Dat is precies het id dat onze
//! MDIO-scan las.
//!
//! LET OP: de Linux-driver zegt er zelf bij dat de bestaande stand uit
//! "pin-strapping RXD1 or the bootloader" komt. Er is dus géén betrouwbare
//! default: beide bits worden expliciet gezet, ook als ze goed lijken.

use crate::{Mdio, Result};

/// PHY-id van de RTL8211F: register 2.
pub const ID1: u16 = 0x001C;
/// PHY-id van de RTL8211F: register 3.
pub const ID2: u16 = 0xC916;

/// Realtek zet zijn extra registers achter een pagina-selector op 0x1F.
/// Pagina 0 is de standaard clause-22-ruimte; vergeet je terug te schakelen,
/// dan leest de rest van de wereld (BMCR, BMSR, autonegotiatie) rommel.
pub const PAGE_SELECT: u8 = 0x1F;
/// De pagina met de twee delay-registers.
pub const PAGE_DELAY: u16 = 0xD08;
/// De standaardpagina.
pub const PAGE_STD: u16 = 0x0000;
/// Het TX-delay-register op pagina 0xD08.
pub const REG_TX_DELAY: u8 = 0x11;
/// De TX-delay-bit.
pub const BIT_TX_DELAY: u16 = 1 << 8;
/// Het RX-delay-register op pagina 0xD08.
pub const REG_RX_DELAY: u8 = 0x15;
/// De RX-delay-bit.
pub const BIT_RX_DELAY: u16 = 1 << 3;

/// Is een gescand id deze chip?
#[must_use]
pub const fn is_rtl8211f(id1: u16, id2: u16) -> bool {
    id1 == ID1 && id2 == ID2
}

/// Zet de RGMII-klokvertragingen in de PHY.
///
/// De vier gevallen uit `rtl8211f_config_init`: "rgmii" = beide uit,
/// "rgmii-id" = beide aan (de Radxa Zero 3E), "rgmii-rxid" = alleen RX,
/// "rgmii-txid" = alleen TX. Read-modify-write per register, want er staan
/// meer bits in die we niet kennen. En de pagina gaat altijd terug naar 0,
/// ook na een busfout onderweg: een PHY die op pagina 0xD08 blijft staan
/// beantwoordt geen enkele normale clause-22-vraag meer en lijkt in een scan
/// verdwenen.
pub fn configure<M: Mdio + ?Sized>(m: &mut M, phy: u8, tx: bool, rx: bool) -> Result {
    m.write(phy, PAGE_SELECT, PAGE_DELAY)?;
    let bit = |on: bool, b: u16| if on { (0, b) } else { (b, 0) };
    let (tc, ts) = bit(tx, BIT_TX_DELAY);
    let (rc, rs) = bit(rx, BIT_RX_DELAY);
    let r = m
        .modify(phy, REG_TX_DELAY, tc, ts)
        .and_then(|()| m.modify(phy, REG_RX_DELAY, rc, rs));
    let back = m.write(phy, PAGE_SELECT, PAGE_STD);
    r.and(back)
}

/// Leest de twee delay-bits terug als `(tx, rx)`. Voor het meetinstrument:
/// de stand vóór onze schrijfactie zegt wat pin-strapping of de bootloader
/// achterliet, en dát is wat de Linux-driver alleen als debug-regel logt.
pub fn delays<M: Mdio + ?Sized>(m: &mut M, phy: u8) -> Result<(bool, bool)> {
    m.write(phy, PAGE_SELECT, PAGE_DELAY)?;
    let r = m.read(phy, REG_TX_DELAY).and_then(|t| {
        let r = m.read(phy, REG_RX_DELAY)?;
        Ok((t & BIT_TX_DELAY != 0, r & BIT_RX_DELAY != 0))
    });
    let back = m.write(phy, PAGE_SELECT, PAGE_STD);
    let v = r?;
    back.map(|()| v)
}
