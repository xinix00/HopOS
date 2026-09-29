//! Wat per `mac_version` verschilt in het mainline-pad (recept §15): een
//! handvol parameters in `rtl_hw_start_8125_common`, de EPHY-tabel, de
//! CLKREQ-bit, de PHY-config en de advertentie. Alles wat hier niet staat,
//! is voor de familie gelijk. Welke variant het is, beslist de chip-XID bij
//! de reset, niet het PCI-id.

/// Eén PHY-stap van een variant (zonder firmware-blob: mainline draait
/// zonder; het blob zijn PHY-errata-patches, geen voorwaarde voor link of
/// verkeer).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Step {
    /// PHY-OCP-register: `mask` wissen, `set` zetten.
    Ocp(u16, u16, u16),
    /// `r8168g_phy_param`: 0xa436 = parm, 0xa438 aanpassen.
    ParamG(u16, u16, u16),
    /// `rtl8125_phy_param`: 0xb87c = parm, 0xb87e aanpassen.
    Param8125(u16, u16, u16),
}

/// Eén variant.
#[derive(Debug)]
pub(crate) struct Variant {
    pub(crate) name: &'static str,
    /// De `rtl_ephy_init`-tabel vóór 8125_common: (reg, mask, bits).
    pub(crate) ephy: &'static [(u16, u16, u16)],
    /// W8(0xd8, &!0x02): alleen VER_70/80.
    pub(crate) rx_desc_fmt: bool,
    /// mac_ocp 0xe614 masker 0x0700 naar deze waarde.
    pub(crate) e614: u16,
    /// mac_ocp 0xe63e masker 0x0c30 naar deze waarde.
    pub(crate) e63e: u16,
    /// De tweede 0xea1c-aanpassing: dit masker naar nul.
    pub(crate) ea1c_second: u16,
    /// De mitigation-tabel wissen tot hier (0xa80 of 0xb00).
    pub(crate) mitig_end: u16,
    /// W16(0x7a, 0).
    pub(crate) int_cfg1: bool,
    /// CLKREQ uit via Config2 bit 7 (61-66) in plaats van INT_CFG0 bit 3.
    pub(crate) clkreq_cfg2: bool,
    /// De variant-eigen PHY-stappen, na `enable_gphy_10m`.
    pub(crate) phy: &'static [Step],
    /// `rtl8168g_config_eee_phy`: 0xa432 |= 0x0010.
    pub(crate) eee_phy: bool,
    /// `rtl8125bp_driver_start` (VER_66).
    pub(crate) dash: bool,
    /// De 0xa5d4-advertentie: 0x0180 (2.5G + 5G) of 0x0080 (2.5G).
    pub(crate) adv: u16,
}

pub(crate) static V8126A: Variant = Variant {
    name: "RTL8126A",
    ephy: &[],
    rx_desc_fmt: true,
    e614: 0x0400,
    e63e: 0x0020,
    ea1c_second: 0x0300,
    mitig_end: 0xa80,
    int_cfg1: true,
    clkreq_cfg2: false,
    phy: &[],
    eee_phy: false,
    dash: false,
    adv: 0x0180,
};

/// `rtl8125b_hw_phy_config` (recept §15f) zonder firmware. De tien
/// `ParamG(0x8044..=0x807a, stap 6)` staan uitgeschreven.
static PHY_8125B: [Step; 19] = [
    Step::Ocp(0xac46, 0x00f0, 0x0090),
    Step::Ocp(0xad30, 0x0003, 0x0001),
    Step::Param8125(0x80f5, 0xffff, 0x760e),
    Step::Param8125(0x8107, 0xffff, 0x360e),
    Step::Param8125(0x8551, 0xff00, 0x0800),
    Step::Ocp(0xbf00, 0xe000, 0xa000),
    Step::Ocp(0xbf46, 0x0f00, 0x0300),
    Step::ParamG(0x8044, 0xffff, 0x2417),
    Step::ParamG(0x804a, 0xffff, 0x2417),
    Step::ParamG(0x8050, 0xffff, 0x2417),
    Step::ParamG(0x8056, 0xffff, 0x2417),
    Step::ParamG(0x805c, 0xffff, 0x2417),
    Step::ParamG(0x8062, 0xffff, 0x2417),
    Step::ParamG(0x8068, 0xffff, 0x2417),
    Step::ParamG(0x806e, 0xffff, 0x2417),
    Step::ParamG(0x8074, 0xffff, 0x2417),
    Step::ParamG(0x807a, 0xffff, 0x2417),
    Step::Ocp(0xa4ca, 0, 0x0040),
    Step::Ocp(0xbf84, 0xe000, 0xa000),
];

/// `rtl8125cp_hw_phy_config` zonder firmware.
static PHY_8125CP: [Step; 9] = [
    Step::Ocp(0xad0e, 0x007f, 0x000b),
    Step::Ocp(0xad78, 0, 1 << 4),
    Step::Param8125(0x807f, 0xff00, 0x5300),
    Step::ParamG(0x81b8, 0xffff, 0x00b4),
    Step::ParamG(0x81ba, 0xffff, 0x00e4),
    Step::ParamG(0x81c5, 0xffff, 0x0104),
    Step::ParamG(0x81d0, 0xffff, 0x054d),
    Step::Ocp(0xa430, 0, 0x0003),
    Step::Ocp(0xa442, 0, 1 << 7),
];

/// `rtl8125bp_hw_phy_config` zonder firmware.
static PHY_8125BP: [Step; 4] = [
    Step::ParamG(0x8010, 0x0800, 0),
    Step::Param8125(0x8088, 0xff00, 0x9000),
    Step::Param8125(0x808f, 0xff00, 0x9000),
    Step::ParamG(0x8174, 0x2000, 0x1800),
];

pub(crate) static V8125B: Variant = Variant {
    name: "RTL8125B",
    ephy: &[
        (0x0b, 0xffff, 0xa908),
        (0x1e, 0xffff, 0x20eb),
        (0x4b, 0xffff, 0xa908),
        (0x5e, 0xffff, 0x20eb),
        (0x22, 0x0030, 0x0020),
        (0x62, 0x0030, 0x0020),
    ],
    rx_desc_fmt: false,
    e614: 0x0200,
    e63e: 0x0000,
    ea1c_second: 0x0004,
    mitig_end: 0xa80,
    int_cfg1: true,
    clkreq_cfg2: true,
    phy: &PHY_8125B,
    eee_phy: true,
    dash: false,
    adv: 0x0080,
};

pub(crate) static V8125D: Variant = Variant {
    name: "RTL8125D",
    ephy: &[],
    rx_desc_fmt: false,
    e614: 0x0300,
    e63e: 0x0020,
    ea1c_second: 0x0004,
    mitig_end: 0xb00,
    int_cfg1: false,
    clkreq_cfg2: true,
    phy: &[],
    eee_phy: true,
    dash: false,
    adv: 0x0080,
};

pub(crate) static V8125CP: Variant = Variant {
    name: "RTL8125CP",
    phy: &PHY_8125CP,
    ..V8125D_BASE
};

pub(crate) static V8125BP: Variant = Variant {
    name: "RTL8125BP",
    phy: &PHY_8125BP,
    dash: true,
    ..V8125D_BASE
};

/// De 8125D-waarden als bouwsteen voor CP en BP (die ze delen).
const V8125D_BASE: Variant = Variant {
    name: "RTL8125D",
    ephy: &[],
    rx_desc_fmt: false,
    e614: 0x0300,
    e63e: 0x0020,
    ea1c_second: 0x0004,
    mitig_end: 0xb00,
    int_cfg1: false,
    clkreq_cfg2: true,
    phy: &[],
    eee_phy: true,
    dash: false,
    adv: 0x0080,
};

/// De variant bij een XID (TxConfig[31:20] & 0xfcf; r8169
/// `rtl_chip_infos`). De oude 8125A (lange eigen PHY-lijst) zit op geen
/// Orion en staat er bewust niet in.
pub(crate) fn by_xid(xid: u32) -> Option<&'static Variant> {
    match xid {
        0x649 | 0x64a => Some(&V8126A),
        0x641 => Some(&V8125B),
        0x688..=0x68a => Some(&V8125D),
        0x708 => Some(&V8125CP),
        0x681 => Some(&V8125BP),
        _ => None,
    }
}
