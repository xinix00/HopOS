//! De SoC-glue van het ethernet: de klokgates en de interne 100M-ePHY.
//!
//! Het init-recept van de ePHY is 1:1 dat van de vendor-U-Boot
//! (u-boot-2021.10/drivers/net/phy/cvitek.c, `cv182xa_ephy_init`) inclusief
//! de stappen uit board.c (shutdown-release, dig_rst_n, PHY_ID), zoals de
//! Go-kern het had (board/licheerv/hop/ephy.go). Wij hebben geen U-Boot
//! onder ons. Niet herschikken zonder bord erbij: het zijn analoge
//! kalibratiestappen in een vaste volgorde, geen registers met een betekenis
//! die we kennen.
//!
//! Dat het recept aankomt is op ijzer gemeten (probe 30-07): daarna antwoordt
//! MDIO-adres 0 met id 0043:5649, en dat id schrijft deze init zelf in de PHY.

use dev::Pa;

/// De klokgates: `clk_500m_eth0` en `clk_axi4_eth0` (na de FSBL open,
/// gemeten).
const CLK_EN0: Pa = Pa(0x0300_2000);
const CLK_500M_ETH0: u32 = 1 << 25;
const CLK_AXI4_ETH0: u32 = 1 << 26;

const EPHY: Pa = Pa(0x0300_9000);
const CTL: u64 = 0x800;
const APB_SEL: u64 = 0x804;
const PAGE: u64 = 0x07c;

const EFUSE_VALID: Pa = Pa(0x0305_0120);
const EFUSE_TUNE20: Pa = Pa(0x0305_1020);
const EFUSE_TUNE24: Pa = Pa(0x0305_1024);
const EFUSE_TX_ECHO_RC: u32 = 1 << 8;
const EFUSE_TX_ITUNE: u32 = 1 << 9;
const EFUSE_TX_RX_TERM: u32 = 1 << 11;

/// Staan beide ethernet-klokgates open?
pub(crate) fn clocks_on() -> bool {
    dev::read32(CLK_EN0) & (CLK_500M_ETH0 | CLK_AXI4_ETH0) == CLK_500M_ETH0 | CLK_AXI4_ETH0
}

/// Opent de ethernet-klokgates.
pub(crate) fn clocks_enable() {
    dev::write32(
        CLK_EN0,
        dev::read32(CLK_EN0) | CLK_500M_ETH0 | CLK_AXI4_ETH0,
    );
}

fn w(off: u64, v: u32) {
    dev::write32(EPHY.add(off), v);
}

fn r(off: u64) -> u32 {
    dev::read32(EPHY.add(off))
}

fn page(p: u32) {
    w(PAGE, p << 8);
}

fn table(p: u32, at: u64, vals: &[u32]) {
    page(p);
    for (i, &v) in vals.iter().enumerate() {
        w(at + 4 * i as u64, v);
    }
}

/// De volledige power-on plus analoge kalibratie van de interne PHY; de
/// MDIO-controle gaat daarna terug naar de MAC. `wait_us` is de wacht van
/// het board.
pub(crate) fn init(wait_us: impl Fn(u64)) {
    w(APB_SEL, 0x0001);
    w(CTL, 0x0900);
    wait_us(2_000);
    w(CTL, 0x0904);
    wait_us(10_000);
    page(0x05);
    w(0x40, 0x0c00);
    w(0x40, 0x0c7e);
    wait_us(1_000);
    w(CTL, 0x0906);

    // TX-tuning: de efuse als die geldig is, anders de vendor-defaults.
    page(0x05);
    let valid = dev::read32(EFUSE_VALID);
    let t24 = dev::read32(EFUSE_TUNE24);
    let t20 = dev::read32(EFUSE_TUNE20);
    if valid & EFUSE_TX_ITUNE != 0 {
        let v = (t24 >> 24) & 0xff | ((t24 >> 16) & 0xff) << 8;
        w(0x64, r(0x64) & !0xffff | v);
    } else {
        w(0x64, 0x5a5a);
    }
    if valid & EFUSE_TX_ECHO_RC != 0 {
        w(0x54, r(0x54) & !0xff00 | ((t24 >> 8) & 0xff) << 8);
    } else {
        w(0x54, 0x0000);
    }
    if valid & EFUSE_TX_RX_TERM != 0 {
        let v = ((t20 >> 28) & 0xf) << 4 | ((t20 >> 24) & 0xf) << 8;
        w(0x58, r(0x58) & !0xff0 | v);
    } else {
        w(0x58, 0x0bb0);
    }
    // 100BaseT rise/fall.
    w(0x5c, 0x0c10);
    w(0x68, 0x0003);
    w(0x54, 0x0000);
    // MLT3-vormtabellen: positieve fase (page 16), negatieve fase (page 17).
    table(0x10, 0x68, &[0x1000, 0x3020, 0x5040, 0x7060]);
    table(0x10, 0x58, &[0x1708, 0x3827, 0x5748, 0x7867]);
    table(
        0x11,
        0x40,
        &[
            0x9080, 0xb0a0, 0xd0c0, 0xf0e0, 0x9788, 0xb8a7, 0xd7c8, 0xf8e7,
        ],
    );
    // TX_Rterm aan en de RX-vcm.
    page(0x05);
    w(0x40, r(0x40) | 0x0001);
    w(0x4c, r(0x4c) | 0x0820);
    // Link-pulsvorm (page 10) en TP_IDLE (page 11).
    table(
        0x0a,
        0x40,
        &[
            0x3e00, 0x7864, 0x6470, 0x5f62, 0x5a5a, 0x5458, 0xb23a, 0x94a0, 0x9092, 0x8a8e, 0x8688,
            0x8484, 0x0082,
        ],
    );
    table(
        0x0b,
        0x40,
        &[
            0x5252, 0x5252, 0x4b52, 0x3d47, 0xaa99, 0x989e, 0x9395, 0x9091, 0x8e8f, 0x8d8e, 0x8c8c,
            0x8b8b, 0x008a,
        ],
    );
    // 10BaseT-datavormen (pages 13..16).
    table(
        0x0d,
        0x40,
        &[0x1e0a, 0x3862, 0x1e62, 0x2a08, 0x244c, 0x1a44, 0x061c],
    );
    table(
        0x0e,
        0x40,
        &[0x2d30, 0x3470, 0x0648, 0x261c, 0x3160, 0x2d5e],
    );
    table(
        0x0f,
        0x40,
        &[0x2922, 0x366e, 0x0752, 0x2556, 0x2348, 0x0c30],
    );
    table(
        0x10,
        0x40,
        &[0x1e08, 0x3868, 0x1462, 0x1a0e, 0x305e, 0x2f62],
    );
    // LED: LNK/SPD/DPX naar het LED-pad (page 1).
    page(0x01);
    w(0x68, r(0x68) & !0x0f00);
    // PHY_ID zetten (vendor board.c): hierdoor vindt de scan 0043:5649.
    page(0x00);
    w(0x08, 0x0043);
    w(0x0c, 0x5649);
    // AGC-swing (page 19) en de LPF/HPF-filters van de CV181x (page 18).
    page(0x13);
    w(0x58, 0x0012);
    w(0x5c, 0x6848);
    page(0x12);
    w(0x48, 0x0808);
    w(0x4c, 0x0808);
    w(0x50, 0x32f8);
    w(0x54, 0xf8dc);
    // Terug naar page 0, autoneg starten, full duplex adverteren.
    page(0x00);
    w(CTL, 0x090e);
    w(0x00, r(0x00) | 0x100);
    w(APB_SEL, 0x0000);
}
