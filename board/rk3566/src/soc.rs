//! De SoC-glue van de RK3566 onder de drivers: CRU (klokken, resets), GRF
//! (RGMII-modus), de pinmux van GMAC1, de PHY-reset op GPIO3, en de klokken
//! en de reset van het TRNG. De watchdog, de temperatuursensor en de
//! USB-klokken hebben elk hun eigen module (`watchdog`, `tsadc`, `usb`); zij
//! lenen hier [`CRU`], [`GRF`] en [`hiword`].
//!
//! Dat het zoveel is heeft één oorzaak, en die is gemeten: U-Boot raakt op
//! dit bord het ethernet niet aan ("No ethernet found"), dus élke laag
//! onder de MAC (pinnen, IO-mux-set, GRF-modus, klokgates, klokdeler,
//! PHY-reset) is van ons. Op ijzer bewezen 06-08 (gigabit-link met
//! DHCP-lease) in de Go-kern (`OLD/metal/board/rk3566`); dit is die code.
//!
//! REFERENTIE (opgehaald 05-08, nagerekend): Linux v6.13
//! `drivers/clk/rockchip/clk-rk3568.c` en `clk.h` (CRU),
//! `drivers/net/ethernet/stmicro/stmmac/dwmac-rk.c` (`rk3568_set_to_rgmii`,
//! `rk3568_set_gmac_speed`), `drivers/pinctrl/pinctrl-rockchip.c`,
//! `drivers/gpio/gpio-rockchip.c`, en
//! `rk3566-radxa-zero-3e.dts` met `rk356x-base.dtsi` voor de boardwaarden.
//!
//! Rockchip-registers zijn "hiword-masked": de bovenste 16 bits zijn een
//! write-enable-masker voor de onderste 16. Geen read-modify-write dus, en
//! geen race met wie er verder in schrijft.

use dev::Pa;

/// De CRU (clock and reset unit).
pub const CRU: Pa = Pa(0xFDD2_0000);
/// De SYS-GRF (syscon@fdc60000): iomux van bank 1..4 en de GMAC-glue.
pub const GRF: Pa = Pa(0xFDC6_0000);
/// De PMU-GRF: iomux van bank 0.
pub const PMU_GRF: Pa = Pa(0xFDC2_0000);
/// GPIO-bank 3 (rk356x-base.dtsi).
pub const GPIO3: Pa = Pa(0xFE76_0000);

/// Een schrijfactie voor een hiword-masked veld: waarde in de onderste 16
/// bits, maskerbits erboven.
#[must_use]
pub const fn hiword(val: u32, mask: u32, shift: u32) -> u32 {
    (val << shift) | (mask << (shift + 16))
}

/// Busy-wait op de architectuurklok. Alleen bij boot, in de NIC-probe.
fn delay_ms(ms: u64) {
    delay_us(ms.saturating_mul(1000));
}

/// Busy-wait van `us` microseconden op de architectuurklok: de korte
/// wachten van de SoC-glue (resets, analoge voorkanten), alleen bij boot.
pub(crate) fn delay_us(us: u64) {
    dev::delay(cpu::idle::now, us.saturating_mul(1000));
}

// --- CRU: de GMAC1-klokken ------------------------------------------------

/// `RK3568_CLKSEL_CON(33)`: alle vier de muxen van GMAC1.
const CLKSEL33: u64 = 0x100 + 33 * 4;
/// `RK3568_CLKGATE_CON(17)`: de gates van GMAC1 (actief-laag: 1 = uit).
const CLKGATE17: u64 = 0x300 + 17 * 4;
/// `RK3568_SOFTRST_CON(14)`: SRST_A_GMAC1 = 236 = bank 14, bit 12.
const SOFTRST14: u64 = 0x400 + 14 * 4;
const SRST_A_GMAC1: u32 = 12;

/// aclk (de AXI-master van de DMA), pclk (de registers), 2top (de
/// 125 MHz-bron) en refout (naar de PHY-kant).
const GMAC1_GATES: [u32; 4] = [3, 4, 5, 10];

// CLKSEL_CON(33): [1:0] RX_TX (0 = rgmii_speed), [2] SCLK_GMAC1 (0 =
// clk_mac1_2top), [5:4] RGMII_SPEED, [9:8] CLK_MAC1_2TOP (0 = cpll_125m).
// Wat de DTS van dit bord voorschrijft: `assigned-clock-parents =
// <&cru SCLK_GMAC1_RGMII_SPEED>, <&cru CLK_MAC1_2TOP>`: samen de 125 MHz die
// de MDIO-deler (CSR 100-150M) en de gigabit-RGMII-klok verwachten.
const SEL_RXTX_SHIFT: u32 = 0;
const SEL_SRC_SHIFT: u32 = 2;
const SEL_RGMII_SPEED_SHIFT: u32 = 4;
const SEL_2TOP_SHIFT: u32 = 8;

/// De gates-schrijfactie: alle vier open (0 = aan).
#[must_use]
pub fn gmac_gates_word() -> u32 {
    GMAC1_GATES.iter().fold(0, |g, &b| g | hiword(0, 1, b))
}

/// De bronkeuze: RX_TX van rgmii_speed, SCLK_GMAC1 van 2top, 2top van
/// cpll_125m.
#[must_use]
pub fn gmac_source_word() -> u32 {
    hiword(0, 0x3, SEL_RXTX_SHIFT) | hiword(0, 0x1, SEL_SRC_SHIFT) | hiword(0, 0x3, SEL_2TOP_SHIFT)
}

/// RGMII_SPEED per linksnelheid. `rk3568_set_gmac_speed` vraagt 125, 25 en
/// 2,5 MHz; met een 125 MHz-bron zijn dat de muxstanden /1 (0), /5 (3) en
/// /50 (2).
#[must_use]
pub fn gmac_speed_word(mbps: u32) -> u32 {
    let sel = match mbps {
        100 => 3,
        10 => 2,
        _ => 0,
    };
    hiword(sel, 0x3, SEL_RGMII_SPEED_SHIFT)
}

/// Opent de klokgates van GMAC1 en zet de bronkeuze zoals de DTS hem
/// voorschrijft. Idempotent. PCLK stond al open (de probe las VERSION 0x3051
/// op 05-08); ACLK is de vraag die pas de DMA beantwoordt.
pub fn gmac_clock_on() {
    dev::write32(CRU.add(CLKGATE17), gmac_gates_word());
    dev::write32(CRU.add(CLKSEL33), gmac_source_word());
    dev::mb();
}

/// Zet de RGMII-klok op de onderhandelde snelheid. Zonder dit klokt de MAC
/// bij 100 Mbit nog op gigabit-tempo de lijn uit: de link staat, maar geen
/// frame landt.
pub fn gmac_set_speed(mbps: u32) {
    dev::write32(CRU.add(CLKSEL33), gmac_speed_word(mbps));
    dev::mb();
}

/// Pulst de AXI-reset van GMAC1. GEMETEN NOODZAAK 05-08: met alle klokken
/// open, de pinmux gezet en de GRF op RGMII bleef de DMA-softreset hangen
/// (`bus mode 0x00000001`): de APB-kant leefde, de AXI-kant stond in reset.
/// De DTS zegt het ook (`resets = <&cru SRST_A_GMAC1>`); Linux pulst hem in
/// `stmmac_probe`, U-Boot niet, want die doet hier geen ethernet.
pub fn gmac_axi_reset() {
    dev::write32(CRU.add(SOFTRST14), hiword(1, 1, SRST_A_GMAC1));
    dev::mb();
    delay_ms(1);
    dev::write32(CRU.add(SOFTRST14), hiword(0, 1, SRST_A_GMAC1));
    dev::mb();
    delay_ms(1);
}

/// De CRU-registers terug, voor het meetinstrument: welke gates open
/// staan, welke muxstand erin zit en of de AXI-reset los is. Een gate die
/// bleef staan is anders niet te onderscheiden van een driver die verkeerd
/// programmeert (dat kostte de eerste trede-3a-boot).
#[must_use]
pub fn gmac_clocks() -> (u32, u32, u32) {
    (
        dev::read32(CRU.add(CLKSEL33)),
        dev::read32(CRU.add(CLKGATE17)),
        dev::read32(CRU.add(SOFTRST14)),
    )
}

// --- CRU: de klok van de cores (alleen gelezen; de knop is SCMI, `clock`) ---

/// `RK3568_PLL_CON(0)` en verder: de APLL (`pll_rk3328` in clk-rk3568.c,
/// de velden van `RK3036_PLLCON` in clk-pll.c).
const APLL_CON0: u64 = 0x0000;
const APLL_CON1: u64 = 0x0004;
const APLL_CON2: u64 = 0x0008;
/// `RK3568_MODE_CON0`: [1:0] de modus van de APLL (0 xin24m, 1 de PLL).
const MODE_CON0: u64 = 0x00C0;
/// `RK3568_CLKSEL_CON(0)`: [4:0] de deler van core 0, bit 6 de bron (0 de
/// APLL, 1 de GPLL, `rk3568_cpuclk_data`), bit 7 de APLL rechtstreeks
/// (U-Boot `CLK_CORE_PRE_SEL_APLL`).
const CLKSEL0: u64 = 0x100;

/// De rate van een PLL van dit type bij een kristal van 24 MHz, zoals
/// `rockchip_rk3036_pll_recalc_rate`; `None` bij een deler nul.
#[must_use]
pub fn pll_hz(con0: u32, con1: u32, con2: u32) -> Option<u64> {
    let fbdiv = u64::from(con0 & 0xfff);
    let post1 = u64::from((con0 >> 12) & 0x7);
    let refdiv = u64::from(con1 & 0x3f);
    let post2 = u64::from((con1 >> 6) & 0x7);
    let dsmpd = (con1 >> 12) & 1;
    let xin: u64 = 24_000_000;
    let mut vco = (xin * fbdiv).checked_div(refdiv)?;
    if dsmpd == 0 {
        vco += ((xin * u64::from(con2 & 0xff_ffff)) / refdiv) >> 24;
    }
    vco.checked_div(post1)?.checked_div(post2)
}

/// Hangt core 0 met dit `CLKSEL_CON(0)` aan de APLL? Bit 7 kiest hem
/// rechtstreeks, anders kiest bit 6 tussen APLL en GPLL. GEMETEN 03-10: na
/// U-Boot staan beide uit; na een SCMI-rate zet de TF-A 0x80c0 (1800) of
/// 0x00c0 (816), en de APLL staat dan precies op de gevraagde rate.
#[must_use]
pub fn on_apll(sel: u32) -> bool {
    sel & (1 << 7) != 0 || sel & (1 << 6) == 0
}

/// De klok van core 0 zoals de CRU hem leest: een tweede getuige naast
/// wat de TF-A over SCMI meldt (`clock`), die de APLL zelf verzet. `None`
/// als de core niet aan de APLL hangt of een deler nul leest.
#[must_use]
pub fn core_hz() -> Option<u64> {
    let r = |off| dev::read32(CRU.add(off));
    let sel = r(CLKSEL0);
    if !on_apll(sel) {
        return None;
    }
    let apll = if r(MODE_CON0) & 0x3 == 1 {
        pll_hz(r(APLL_CON0), r(APLL_CON1), r(APLL_CON2))?
    } else {
        24_000_000
    };
    Some(apll / u64::from((sel & 0x1f) + 1))
}

// --- GRF: RGMII-modus -----------------------------------------------------

/// `RK3568_GRF_GMAC1_CON0`: rx-delay [14:8], tx-delay [6:0].
const GRF_GMAC1_CON0: u64 = 0x0388;
/// `RK3568_GRF_GMAC1_CON1`: de PHY-interface en de delay-lijnen.
const GRF_GMAC1_CON1: u64 = 0x038C;
/// CON0: beide delays nul. Dit bord heeft `phy-mode = "rgmii-id"` (de PHY
/// doet de delays), en dwmac-rk roept dan `set_to_rgmii(0, 0)` aan.
pub const GRF_RGMII_DELAYS_ZERO: u32 = 0x7F7F_0000;
/// CON1: PHY_INTF_SEL_RGMII (bit 4 zetten, 5 en 6 wissen) plus beide
/// delay-lijnen aan (bit 1 = rx, bit 0 = tx). Die zet `rk3568_set_to_rgmii`
/// óók bij delay 0; afwijken van het recept is een gok.
pub const GRF_RGMII_MODE: u32 = 0x0073_0013;

/// Zet GMAC1 in RGMII-modus met nul-delays (rgmii-id).
pub fn gmac_set_rgmii() {
    dev::write32(GRF.add(GRF_GMAC1_CON0), GRF_RGMII_DELAYS_ZERO);
    dev::write32(GRF.add(GRF_GMAC1_CON1), GRF_RGMII_MODE);
    dev::mb();
}

// --- pinmux: de gmac1m1-groep ---------------------------------------------

/// `GRF_IOFUNC_SEL0`: bit 8 kiest de GMAC1-pinset (1 = M1). De RK3566 heeft
/// twee fysieke pinsets voor GMAC1 en een schakelaar ertussen; alleen de
/// pinnen muxen zonder die schakelaar levert een MAC waarvan de MDIO nooit
/// antwoordt.
const GRF_IOFUNC_SEL0: u64 = 0x0300;
const GMAC1_MUX_M1: u32 = (1 << 8) | (1 << 24);
/// Alle gmac1m1-pinnen gebruiken functie 3.
const FN_GMAC: u8 = 3;
/// De PHY-reset blijft GPIO.
const FN_GPIO: u8 = 0;

/// Eén pinmux-instelling: bank, bankbit (RK_Pxy = poort × 8 + nr), functie.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Pin {
    /// De GPIO-bank, 0..=4.
    pub bank: u8,
    /// Het bit in de bank, 0..32.
    pub bit: u8,
    /// De functie.
    pub func: u8,
}

const fn pin(bank: u8, bit: u8, func: u8) -> Pin {
    Pin { bank, bit, func }
}

/// De volledige gmac1m1-pinlijst uit rk3568-pinctrl.dtsi, in de groepen van
/// de DTS. RK_PA0..PA7 = 0..7, PB = 8..15, PC = 16..23, PD = 24..31. DIT
/// BORD GEBRUIKT M1 (rk3566-radxa-zero-3e.dts), niet M0.
pub const GMAC1_M1_PINS: [Pin; 16] = [
    // gmac1m1_miim: mdc, mdio.
    pin(4, 8 + 6, FN_GMAC),
    pin(4, 8 + 7, FN_GMAC),
    // gmac1m1_tx_bus2: txd0, txd1, txen.
    pin(4, 4, FN_GMAC),
    pin(4, 5, FN_GMAC),
    pin(4, 6, FN_GMAC),
    // gmac1m1_rx_bus2: rxd0, rxd1, rxdv.
    pin(4, 7, FN_GMAC),
    pin(4, 8, FN_GMAC),
    pin(4, 8 + 1, FN_GMAC),
    // gmac1m1_rgmii_clk: rxclk, txclk.
    pin(4, 3, FN_GMAC),
    pin(4, 0, FN_GMAC),
    // gmac1m1_rgmii_bus: rxd2, rxd3, en txd2, txd3 in BANK 3, niet 4.
    pin(4, 1, FN_GMAC),
    pin(4, 2, FN_GMAC),
    pin(3, 24 + 6, FN_GMAC),
    pin(3, 24 + 7, FN_GMAC),
    // gmac1m1_clkinout: de RGMII-referentieklok (clock_in_out = "input").
    pin(4, 16 + 1, FN_GMAC),
    // De PHY-reset (gmac1_rstn: <3 RK_PC0 RK_FUNC_GPIO>): gebruikte U-Boot
    // die pin voor iets anders, dan trekt de reset anders niets laag.
    pin(3, 16, FN_GPIO),
];

/// Het GRF-register en de bitshift van één pin: vier bits per pin, vier
/// pinnen per register, twee registers per groep van 8, 0x20 per bank. Bank
/// 0 hangt aan de PMU-GRF, 1..4 aan de SYS-GRF vanaf offset 0.
#[must_use]
pub fn iomux_reg(bank: u8, bit: u8) -> (Pa, u32) {
    let base = if bank == 0 {
        PMU_GRF
    } else {
        GRF.add(u64::from(bank - 1) * 0x20)
    };
    let off = u64::from(bit / 8) * 8 + u64::from((bit % 8) / 4) * 4;
    (base.add(off), u32::from(bit % 4) * 4)
}

/// Zet alle GMAC1-pinnen op hun ethernet-functie en schakelt de M1-pinset
/// in. Vóór de GRF-modus en vóór de eerste MDIO-transactie.
pub fn gmac_pinmux() {
    dev::write32(GRF.add(GRF_IOFUNC_SEL0), GMAC1_MUX_M1);
    for p in GMAC1_M1_PINS {
        let (reg, shift) = iomux_reg(p.bank, p.bit);
        dev::write32(reg, (0xF << (shift + 16)) | (u32::from(p.func) << shift));
    }
    dev::mb();
}

/// De huidige functie van één pin, voor het meetinstrument.
#[must_use]
pub fn mux_of(bank: u8, bit: u8) -> u32 {
    let (reg, shift) = iomux_reg(bank, bit);
    (dev::read32(reg) >> shift) & 0xF
}

// --- GPIO3 PC0: de PHY-reset ----------------------------------------------

/// `version_id` van de v2-GPIO-controller; op v1 gereserveerd.
const GPIO_VERSION_ID: u64 = 0x78;
/// GPIO_TYPE_V2 en varianten delen de bovenste byte.
const GPIO_TYPE_V2_TOP: u32 = 0x01;
/// v2: DR hoog op +0x04, DDR hoog op +0x0C (hiword-masked).
const GPIO_V2_DR_HI: u64 = 0x04;
const GPIO_V2_DDR_HI: u64 = 0x0C;
/// v1 als terugval: DR +0x00, DDR +0x04, gewone read-modify-write.
const GPIO_V1_DR: u64 = 0x00;
const GPIO_V1_DDR: u64 = 0x04;
/// RK_PC0 = bankbit 16.
const PHY_RESET_PIN: u32 = 16;

/// Zet of wist één pin: op v2 in de hoge helft met maskerbit, op v1 met
/// read-modify-write. De generatie komt uit het silicium, niet uit een
/// aanname: wie hier de v1-indeling gebruikt op een v2-bank schrijft de
/// richting in het int_en-register en trekt de reset nooit laag.
fn gpio_set(v2: bool, hi_off: u64, v1_off: u64, pin: u32, on: bool) {
    if v2 {
        let b = 1u32 << (pin % 16);
        let v = if on { b | (b << 16) } else { b << 16 };
        dev::write32(GPIO3.add(hi_off), v);
        return;
    }
    let r = GPIO3.add(v1_off);
    let v = dev::read32(r);
    dev::write32(r, if on { v | (1 << pin) } else { v & !(1 << pin) });
}

/// Houdt de PHY-reset 20 ms laag en laat hem daarna 50 ms los: de tijden
/// uit de DTS (`reset-assert-us = 20000`, `reset-deassert-us = 50000`).
/// Zonder dit is er op een koude boot geen PHY op de MDIO-bus, en omdat dit
/// bord `clock_in_out = "input"` heeft, komt ook de RGMII-referentieklok
/// van de PHY: een PHY in reset levert geen klok.
pub fn gmac_phy_reset() {
    let v2 = dev::read32(GPIO3.add(GPIO_VERSION_ID)) >> 24 == GPIO_TYPE_V2_TOP;
    gpio_set(v2, GPIO_V2_DDR_HI, GPIO_V1_DDR, PHY_RESET_PIN, true); // uitgang
    gpio_set(v2, GPIO_V2_DR_HI, GPIO_V1_DR, PHY_RESET_PIN, false); // actief-laag
    dev::mb();
    delay_ms(20);
    gpio_set(v2, GPIO_V2_DR_HI, GPIO_V1_DR, PHY_RESET_PIN, true);
    dev::mb();
    delay_ms(50);
}

// --- het TRNG: klokken en reset --------------------------------------------

/// `CLKGATE_CON(9)`: bit 10 = hclk, bit 11 = de kern van het TRNG
/// (clk-rk3568.c `HCLK_TRNG_NS`, `CLK_TRNG_NS`). Beide `CLK_IGNORE_UNUSED`,
/// dus vermoedelijk al open na de bootketen; "vermoedelijk" is precies
/// waarom we ze toch zetten.
const CLKGATE9: u64 = 0x300 + 9 * 4;
const GATE_HCLK_TRNG: u32 = 10;
const GATE_CLK_TRNG: u32 = 11;
/// `SOFTRST_CON(6)`: SRST_TRNG_NS = 109 = bank 6, bit 13 (rk3568-cru.h).
const SOFTRST6: u64 = 0x400 + 6 * 4;
const SRST_TRNG: u32 = 13;

/// Opent de klokken van het TRNG en pulst zijn reset (Go `trngInit`; Linux
/// neemt 2 us, wij 5). VÓÓR de eerste aanraking van het blok: een ongeklokt
/// Rockchip-blok geeft geen abort maar houdt de bus vast (Go, 06-08: een
/// boot die stierf vóór zijn banner).
pub fn trng_clock_on() {
    dev::write32(
        CRU.add(CLKGATE9),
        hiword(0, 1, GATE_HCLK_TRNG) | hiword(0, 1, GATE_CLK_TRNG),
    );
    dev::mb();
    dev::write32(CRU.add(SOFTRST6), hiword(1, 1, SRST_TRNG));
    dev::mb();
    delay_us(5);
    dev::write32(CRU.add(SOFTRST6), hiword(0, 1, SRST_TRNG));
    dev::mb();
}
