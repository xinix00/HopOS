//! Een Broadcom-STB PCIe-root-complex: de BCM2712-variant (Raspberry Pi 5:
//! pcie0/1/2, waarvan pcie2 naar de RP1 met de netwerkcontroller gaat en
//! pcie1 naar de FFC voor M.2/NVMe), en de BCM2711-variant (Pi 4: de VL805).
//!
//! GEMETEN 10-07 (probe5): de Pi 5-firmware traint GEEN enkele PCIe-link:
//! PHYLINKUP=0, DL_ACTIVE=0, alle reads door het venster geven 0xdeaddead.
//! De VideoCore laadt alleen de RP1-firmware (via I2C in RP1-SRAM, vóór er
//! PCIe bestaat); de link zelf is aan het OS, elke boot. Linux ook.
//!
//! Het recept is Linux' `drivers/pci/controller/pcie-brcmstb.c`
//! (raspberrypi/linux rpi-6.12.y, "brcm,bcm2712-pcie"). De
//! BCM2712-eigenaardigheden die daar verstopt zitten en zonder welke niets
//! werkt:
//!
//! 1. RESCAL: één gedeeld analoog kalibratieblok voor alle drie de
//!    controllers, één keer starten en pollen vóór de eerste bridge.
//! 2. Bridge-reset via de externe SW_INIT-resetcontroller (bank/bit), niet
//!    via RGR1_SW_INIT_1 van oudere chips.
//! 3. PERST# via PCIE_MISC_PCIE_CTRL bit 2 (PERSTB, invers: 1 = lossen).
//! 4. HARD_DEBUG op 0x4304 (oudere chips: 0x4204); SerDes uit IDDQ door
//!    bit 27 te wissen.
//! 5. De PLL van de PHY verwacht af fabriek een 100 MHz-refclk; de Pi 5
//!    heeft een 54 MHz-kristal. Zonder de MDIO-herprogrammering (blok
//!    0x1600) traint de link NOOIT: dat was het 0xdeaddead-raadsel.
//! 6. Elk inbound-window heeft óók een UBUS-remap-register met ACCESS_EN.
//!
//! De freeze-jacht van 13-07 (BCM2712 C1-erratum, OLD/docs/v1/archief)
//! liet twee instellingen achter die hier staan: MAX_BURST 128 B en
//! VDM-QoS aan.

#![cfg_attr(not(test), no_std)]
#![cfg_attr(
    test,
    allow(
        clippy::unwrap_used,
        clippy::expect_used,
        clippy::panic,
        clippy::indexing_slicing
    )
)]

use core::fmt;
use dev::{Pa, Reg};

#[cfg(test)]
mod tests;

/// De registeroffsets vanaf de controller-basis (`pcie_offsets_bcm7712` en
/// de PCIE_MISC-constanten van `pcie-brcmstb.c`).
pub mod off {
    /// Type-1-header: command.
    pub const CFG_COMMAND: u64 = 0x04;
    /// Type-1-header: primary/secondary/subordinate.
    pub const CFG_PRIMARY_BUS: u64 = 0x18;
    /// Type-1-header: bridge memory base/limit.
    pub const CFG_MEM_BASE: u64 = 0x20;
    /// PCIe-cap 0xac + 0x30: target link speed [3:0].
    pub const CFG_LNKCTL2: u64 = 0xdc;
    /// Endianness van BAR2.
    pub const CFG_VENDOR_SPEC1: u64 = 0x0188;
    /// Klassecode [23:0].
    pub const CFG_ID_VAL3: u64 = 0x043c;
    /// ASPM-support [11:10], max link speed [3:0].
    pub const CFG_LINK_CAP: u64 = 0x04dc;
    /// PM-klokperiode [7:0].
    pub const CFG_PHY_CTL15: u64 = 0x184c;
    /// Interne PHY-MDIO: adres.
    pub const MDIO_ADDR: u64 = 0x1100;
    /// Interne PHY-MDIO: schrijfdata.
    pub const MDIO_WR_DATA: u64 = 0x1104;
    /// RC_TL_VDM_CTL1: VendorID-match.
    pub const RC_TL_VDM_CTL1: u64 = 0x0a0c;
    /// RC_TL_VDM_CTL0: VDM_ENABLED|IGNORETAG|IGNOREVNDRID [18:16].
    pub const RC_TL_VDM_CTL0: u64 = 0x0a20;
    /// MISC_CTRL.
    pub const MISC_CTRL: u64 = 0x4008;
    /// Outbound window 0: PCIe-adres laag (+ 8 per window).
    pub const MISC_WIN0_LO: u64 = 0x400c;
    /// Outbound window 0: PCIe-adres hoog.
    pub const MISC_WIN0_HI: u64 = 0x4010;
    /// RC-BAR 1..3 (inbound): +8 per BAR, HI op +4.
    pub const MISC_RC_BAR1_LO: u64 = 0x402c;
    /// Config-retry-timeout.
    pub const MISC_CFG_RETRY_TMO: u64 = 0x405c;
    /// PCIE_CTRL: bit 2 = PERSTB.
    pub const MISC_PCIE_CTRL: u64 = 0x4064;
    /// PCIE_STATUS.
    pub const MISC_PCIE_STATUS: u64 = 0x4068;
    /// Outbound window 0: base/limit in MB.
    pub const MISC_WIN0_BASE_LIMIT: u64 = 0x4070;
    /// Outbound window 0: base-MB >> 12.
    pub const MISC_WIN0_BASE_HI: u64 = 0x4080;
    /// Outbound window 0: limit-MB >> 12.
    pub const MISC_WIN0_LIMIT_HI: u64 = 0x4084;
    /// MISC_CTRL_1.
    pub const MISC_CTRL1: u64 = 0x40a0;
    /// UBUS_CTRL.
    pub const MISC_UBUS_CTRL: u64 = 0x40a4;
    /// UBUS-timeout.
    pub const MISC_UBUS_TMO: u64 = 0x40a8;
    /// UBUS-remap van RC-BAR 1..3: +8, HI op +4.
    pub const MISC_UBUS_BAR1_RMP: u64 = 0x40ac;
    /// RC-BAR 4.. (inbound).
    pub const MISC_RC_BAR4_LO: u64 = 0x40d4;
    /// UBUS-remap van RC-BAR 4..
    pub const MISC_UBUS_BAR4_RMP: u64 = 0x410c;
    /// VDM-prioriteit naar AXI-QoS (hoog).
    pub const MISC_VDM_QOS_HI: u64 = 0x4164;
    /// VDM-prioriteit naar AXI-QoS (laag).
    pub const MISC_VDM_QOS_LO: u64 = 0x4168;
    /// AXI_INTF_CTRL.
    pub const MISC_AXI_INTF_CTRL: u64 = 0x416c;
    /// Wat een mislukte AXI-read oplevert.
    pub const MISC_AXI_RD_ERR_DATA: u64 = 0x4170;
    /// HARD_DEBUG op de BCM2712.
    pub const HARD_DEBUG: u64 = 0x4304;
    /// HARD_DEBUG op de BCM2711: zelfde bits, ander adres.
    pub const HARD_DEBUG_2711: u64 = 0x4204;
    /// Het 4 KB-configvenster van de geselecteerde functie.
    pub const EXT_CFG_DATA: u64 = 0x8000;
    /// ECAM-index: bus<<20 | dev<<15 | fn<<12.
    pub const EXT_CFG_INDEX: u64 = 0x9000;
    /// BCM2711: bridge-reset en PERST# in één register.
    pub const RGR1_SW_INIT: u64 = 0x9210;
}

/// De maat van het registerblok.
pub const MMIO_SIZE: u64 = 0x9214;

const STATUS_PHY_LINK_UP: u32 = 1 << 4;
const STATUS_DL_ACTIVE: u32 = 1 << 5;
const STATUS_RC_MODE: u32 = 1 << 7;
const HD_CLKREQ_DEBUG: u32 = 1 << 1;
const HD_REFCLK_OVRD_EN: u32 = 1 << 16;
const HD_REFCLK_OVRD_OUT: u32 = 1 << 20;
const HD_L1SS_ENABLE: u32 = 1 << 21;
const HD_SERDES_IDDQ: u32 = 1 << 27;
/// MISC_CTRL: SCB0_SIZE [31:27], het venster naar geheugencontroller 0 als
/// log2(maat) - 15.
const MISC_CTRL_SCB0_SHIFT: u32 = 27;
const MISC_CTRL_SCB0_MASK: u32 = 0x1f << MISC_CTRL_SCB0_SHIFT;
const RGR1_PERST: u32 = 1 << 0;
const RGR1_BRIDGE_RST: u32 = 1 << 1;

/// Welk silicium onder de controller zit. De MISC-kaart is grotendeels
/// gedeeld; HARD_DEBUG, de reset en de uitbreidingen (UBUS-remap, VDM-QoS,
/// de refclk-PLL) niet.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Soc {
    /// Pi 5: de BCM7712-codepaden.
    Bcm2712,
    /// Pi 4: oudere kaart, geen RESCAL, geen MDIO-PLL.
    Bcm2711,
}

/// Eén inbound-window: PCIe-adres naar CPU/DRAM. `size` is een macht van
/// twee (4 KB tot 64 GB).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct InWin {
    /// Het PCIe-adres.
    pub pcie: u64,
    /// Het CPU-adres.
    pub cpu: u64,
    /// De maat.
    pub size: u64,
}

/// Het outbound-window: CPU-adres naar PCIe-adres (MB-korrel).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct OutWin {
    /// Het CPU-adres.
    pub cpu: u64,
    /// Het PCIe-adres.
    pub pcie: u64,
    /// De maat.
    pub size: u64,
}

/// Eén BAR-toewijzing op de endpoint achter de RC (bus 1, dev 0, fn 0).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct EpBar {
    /// De config-offset (0x10, 0x14, ...).
    pub off: u64,
    /// Het basisadres.
    pub val: u32,
}

/// Waarom de bring-up faalde.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    /// RESCAL bevestigde niet.
    Rescal,
    /// De controller strapt niet als root-complex.
    NotRc {
        /// PCIE_STATUS.
        status: u32,
    },
    /// De link traint niet.
    NoLink {
        /// PCIE_STATUS.
        status: u32,
    },
    /// De endpoint is niet wie we verwachten.
    Endpoint {
        /// vendor | device << 16.
        found: u32,
        /// Wat het board verwacht.
        want: u32,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::Rescal => f.write_str("brcmpcie: RESCAL calibration not confirmed"),
            Self::NotRc { status } => {
                write!(
                    f,
                    "brcmpcie: not strapped as root complex (status {status:#x})"
                )
            }
            Self::NoLink { status } => {
                write!(f, "brcmpcie: PCIe link does not train (status {status:#x})")
            }
            Self::Endpoint { found, want } => {
                write!(
                    f,
                    "brcmpcie: endpoint reports {found:#x} (expected {want:#x})"
                )
            }
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

fn reg(pa: Pa) -> &'static Reg<u32> {
    // SAFETY: elke aanroeper geeft een adres binnen een blok dat de
    // voorwaarde van `Rc::new` of `rescal` gemapt noemt; een `Reg<u32>` is
    // één gealigneerd 32-bit register.
    unsafe { dev::regs(pa) }
}

fn delay(clock: fn() -> u64, ns: u64) {
    let end = clock().saturating_add(ns);
    while clock() < end {
        core::hint::spin_loop();
    }
}

/// Kalibreert het gedeelde analoge blok (brcm,bcm7216-pcie-sata-rescal;
/// BCM2712: 0x10_0011_9500). Eén keer per boot, vóór de eerste
/// bridge-reset: START zetten, teruglezen, STATUS pollen, START wissen
/// (`reset-brcmstb-rescal.c`).
///
/// # Safety
///
/// `base` is het gemapte RESCAL-blok (12 bytes).
pub unsafe fn rescal(base: Pa, clock: fn() -> u64) -> bool {
    let start = reg(base);
    start.update(|v| v | 1);
    if start.read() & 1 == 0 {
        return false; // schrijft niet: het blok bestaat hier niet
    }
    let status = reg(base.add(8));
    let mut ok = false;
    // Ruim boven Linux' 1 ms.
    for _ in 0..20 {
        if status.read() & 1 != 0 {
            ok = true;
            break;
        }
        delay(clock, 100_000);
    }
    start.update(|v| v & !1);
    ok
}

/// Codeert een inbound-window-grootte (SIZE [4:0] van RC_BAR_CONFIG_LO):
/// log2 12..15 wordt 0x1c + log2 - 12, log2 16..36 wordt log2 - 15; de rest
/// is uit.
#[must_use]
pub fn in_size_enc(size: u64) -> u32 {
    if size == 0 {
        return 0;
    }
    let l = 63 - size.leading_zeros();
    match l {
        12..=15 => 0x1c + l - 12,
        16..=36 => l - 15,
        _ => 0,
    }
}

/// Het SCB0_SIZE-veld van MISC_CTRL voor deze inbound-windows: hun totaal,
/// naar boven afgerond op een macht van twee, als log2 - 15 (Linux
/// `brcm_pcie_get_inbound_wins`, de "educated guess" zonder
/// `brcm,scb-sizes`, en `brcm_pcie_setup`). `None` als er niets inbound
/// is of het totaal onder 32 KB ligt.
#[must_use]
pub fn scb0_size(inb: &[Option<InWin>]) -> Option<u32> {
    let tot = inb
        .iter()
        .flatten()
        .fold(0u64, |n, w| n.saturating_add(w.size));
    let l = tot.checked_next_power_of_two()?.trailing_zeros();
    if tot == 0 || l < 15 {
        return None;
    }
    Some((l - 15).min(0x1f))
}

/// Het register van RC-BAR `n` (1-genummerd) en zijn UBUS-remap.
fn in_regs(n: u64) -> (u64, u64) {
    if n >= 4 {
        (
            off::MISC_RC_BAR4_LO + (n - 4) * 8,
            off::MISC_UBUS_BAR4_RMP + (n - 4) * 8,
        )
    } else {
        (
            off::MISC_RC_BAR1_LO + (n - 1) * 8,
            off::MISC_UBUS_BAR1_RMP + (n - 1) * 8,
        )
    }
}

/// Het BASE_LIMIT-woord van een outbound-window: base-MB [15:4], limit-MB
/// [31:20], plus de hoge MB-bits voor de HI-registers.
#[must_use]
pub fn out_encoding(w: OutWin) -> (u32, u32, u32) {
    let base_mb = w.cpu >> 20;
    let limit_mb = (w.cpu + w.size - 1) >> 20;
    (
        (((base_mb & 0xfff) as u32) << 4) | (((limit_mb & 0xfff) as u32) << 20),
        ((base_mb >> 12) & 0xff) as u32,
        ((limit_mb >> 12) & 0xff) as u32,
    )
}

/// De ECAM-index van bus/dev/fn.
#[must_use]
pub const fn ecam_index(bus: u8, dev: u8, func: u8) -> u32 {
    ((bus as u32) << 20) | (((dev & 0x1f) as u32) << 15) | (((func & 0x7) as u32) << 12)
}

/// Hoeveel inbound-windows een RC hier kan dragen.
pub const MAX_IN: usize = 4;

/// Eén root-complex.
pub struct Rc {
    soc: Soc,
    base: Pa,
    sw_init: Pa,
    sw_init_id: u32,
    speed: u32,
    out: OutWin,
    inb: [Option<InWin>; MAX_IN],
    clock: fn() -> u64,
}

impl Rc {
    /// Een RC op `base`. `sw_init` is de gedeelde brcm,brcmstb-reset
    /// (BCM2712: 0x10_0150_4318, id 42/43/44 voor pcie0/1/2); op de BCM2711
    /// ongebruikt. `speed` = het link-snelheidsplafond (generatie) (0 = laten).
    ///
    /// # Safety
    ///
    /// `base` ([`MMIO_SIZE`]) en `sw_init` (op de BCM2712) zijn gemapte
    /// blokken van dit board; de windows beschrijven het echte adresplan.
    #[must_use]
    #[expect(
        clippy::too_many_arguments,
        reason = "één constructor met het hele adresplan"
    )]
    pub const unsafe fn new(
        soc: Soc,
        base: Pa,
        sw_init: Pa,
        sw_init_id: u32,
        speed: u32,
        out: OutWin,
        inb: [Option<InWin>; MAX_IN],
        clock: fn() -> u64,
    ) -> Self {
        Self {
            soc,
            base,
            sw_init,
            sw_init_id,
            speed,
            out,
            inb,
            clock,
        }
    }

    fn r(&self, o: u64) -> &'static Reg<u32> {
        reg(self.base.add(o))
    }

    fn hard_debug(&self) -> u64 {
        match self.soc {
            Soc::Bcm2711 => off::HARD_DEBUG_2711,
            Soc::Bcm2712 => off::HARD_DEBUG,
        }
    }

    /// De SW_INIT-reset: bank = id>>5 (stride 0x18), SET op +0, CLEAR op +4,
    /// bit = id&31. Op de BCM2711 één bit in RGR1_SW_INIT_1.
    fn bridge_reset(&self, assert: bool) {
        if self.soc == Soc::Bcm2711 {
            self.rgr1(RGR1_BRIDGE_RST, assert);
            return;
        }
        let mut o = u64::from(self.sw_init_id >> 5) * 0x18;
        if !assert {
            o += 4;
        }
        reg(self.sw_init.add(o)).write(1 << (self.sw_init_id & 31));
        dev::mb();
    }

    /// PERST# naar de endpoint: BCM2712 PERSTB in PCIE_CTRL (1 = lossen),
    /// BCM2711 een bit in RGR1 (1 = vast).
    fn perst(&self, assert: bool) {
        if self.soc == Soc::Bcm2711 {
            self.rgr1(RGR1_PERST, assert);
            return;
        }
        let v = if assert { 0 } else { 1 << 2 };
        self.r(off::MISC_PCIE_CTRL).update(|c| (c & !(1 << 2)) | v);
    }

    fn rgr1(&self, bit: u32, set: bool) {
        self.r(off::RGR1_SW_INIT)
            .update(|v| if set { v | bit } else { v & !bit });
        dev::mb();
    }

    /// `brcm_pcie_setup` plus `post_setup_bcm2712`: bridge-resetcyclus,
    /// SerDes wekken, windows, de 54 MHz-refclk-PLL. De link blijft down
    /// (PERST# vast) tot [`start_link`](Self::start_link). Geeft (rc-modus,
    /// PLL bevestigd).
    pub fn setup(&self) -> (bool, bool) {
        // Op de BCM2711 mag de firmware PERST# al gelost hebben (Linux zet
        // hem expliciet terug), en een endpoint die tijdens de bridge-setup
        // uit reset staat traint half.
        self.bridge_reset(true);
        if self.soc == Soc::Bcm2711 {
            self.perst(true);
        }
        delay(self.clock, 200_000);
        self.bridge_reset(false);
        delay(self.clock, 200_000);
        self.r(self.hard_debug()).update(|v| v & !HD_SERDES_IDDQ);
        delay(self.clock, 200_000);

        // SCB_ACCESS_EN | CFG_READ_UR_MODE (een config-read naar niets geeft
        // all-ones in plaats van een abort) | RCB_MPS | RCB_64B, en
        // MAX_BURST 128 B: op de BCM2712 0x1 (Linux: "burst = 0x1 /* 128
        // bytes */"). Eerder stond hier 0x2/512 B, vier keer wat Linux dit
        // silicium toestaat, op precies het inbound-pad van de stille freeze
        // (13-07). Op de BCM2711 is 128 B de waarde 0x0.
        let burst = if self.soc == Soc::Bcm2711 {
            0
        } else {
            0x10_0000
        };
        self.r(off::MISC_CTRL)
            .update(|v| ((v | 0x1000 | 0x2000 | 0x400 | 0x80) & !0x30_0000) | burst);

        for (i, w) in self.inb.iter().enumerate() {
            let Some(w) = w else { continue };
            let (bar, ubus) = in_regs(i as u64 + 1);
            self.r(bar).write((w.pcie as u32) | in_size_enc(w.size));
            self.r(bar + 4).write((w.pcie >> 32) as u32);
            if self.soc == Soc::Bcm2712 {
                self.r(ubus).write(((w.cpu as u32) & !0xfff) | 1);
                self.r(ubus + 4).write((w.cpu >> 32) as u32);
            }
        }

        if self.r(off::MISC_PCIE_STATUS).read() & STATUS_RC_MODE == 0 {
            return (false, false);
        }
        // SCB0_SIZE: hoe groot het geheugen is dat een endpoint via RC_BAR2
        // mag lezen. Linux zet het op elke STB-chip behalve de 7712-familie
        // (de Pi 5 heeft het niet, daar keert `brcm_pcie_get_inbound_wins`
        // eerder terug). Op de Pi 4 is het de weg waarlangs de VL805 zijn
        // firmware binnenhaalt als de VideoCore hem laadt; wat de bridge-
        // reset of een vorige eigenaar hier liet staan, telt niet.
        if self.soc == Soc::Bcm2711
            && let Some(scb) = scb0_size(&self.inb)
        {
            self.r(off::MISC_CTRL)
                .update(|v| (v & !MISC_CTRL_SCB0_MASK) | (scb << MISC_CTRL_SCB0_SHIFT));
        }
        // ASPM: alleen L1 adverteren (DT aspm-no-l0s); klasse PCI-bridge.
        self.r(off::CFG_LINK_CAP)
            .update(|v| (v & !0xc00) | (2 << 10));
        self.r(off::CFG_ID_VAL3)
            .update(|v| (v & !0xff_ffff) | 0x06_0400);

        let (bl, bh, lh) = out_encoding(self.out);
        self.r(off::MISC_WIN0_LO).write(self.out.pcie as u32);
        self.r(off::MISC_WIN0_HI)
            .write((self.out.pcie >> 32) as u32);
        self.r(off::MISC_WIN0_BASE_LIMIT).write(bl);
        self.r(off::MISC_WIN0_BASE_HI).update(|v| (v & !0xff) | bh);
        self.r(off::MISC_WIN0_LIMIT_HI).update(|v| (v & !0xff) | lh);
        self.r(off::CFG_VENDOR_SPEC1).update(|v| v & !0xc);

        if self.soc == Soc::Bcm2711 {
            return (true, true);
        }
        (true, self.post_setup_2712())
    }

    /// De PHY-PLL naar de 54 MHz-kristalrefclk (blok 0x1600), en de
    /// UBUS/AXI-foutonderdrukking: een mislukte read geeft all-ones in
    /// plaats van een SError.
    fn post_setup_2712(&self) -> bool {
        let mut ok = self.mdio_write(0, 0x1f, 0x1600);
        for (r, v) in [
            (0x16, 0x50b9),
            (0x17, 0xbda1),
            (0x18, 0x0094),
            (0x19, 0x97b4),
            (0x1b, 0x5030),
            (0x1c, 0x5030),
            (0x1e, 0x0007),
        ] {
            ok &= self.mdio_write(0, r, v);
        }
        delay(self.clock, 200_000);
        // PM-klokperiode 18,52 ns = 1/54 MHz.
        self.r(off::CFG_PHY_CTL15).update(|v| (v & !0xff) | 0x12);
        self.r(off::MISC_UBUS_CTRL)
            .update(|v| v | (1 << 13) | (1 << 19));
        self.r(off::MISC_AXI_RD_ERR_DATA).write(u32::MAX);
        self.r(off::MISC_UBUS_TMO).write(0x0b2d_0000); // ~250 ms
        self.r(off::MISC_CFG_RETRY_TMO).write(0x0aba_0000); // ~240 ms
        let axi = self.r(off::MISC_AXI_INTF_CTRL);
        axi.update(|v| (v & !(1 << 7)) | (1 << 13) | (1 << 12) | (1 << 11));
        if axi.read() & (1 << 12) == 0 {
            // Silicium zonder de QoS-bits (C1: reserved-0): AXI-outstanding
            // op Linux' referentiewaarde 15.
            axi.update(|v| (v & !0x3f) | 15);
        }
        // VDM-QoS AAN: de Pi 5-DT eist dit voor de RP1-poort
        // (brcm,vdm-qos-map = 0xbbaa9888). De RP1 stuurt QoS-VDM's zodra
        // zijn FIFO's vollopen, precies het sustained-RX-moment; met
        // VDM-receptie uit landen die op een dove RC (13-07, delta 2).
        self.r(off::MISC_VDM_QOS_HI).write(0xbbaa_9888);
        self.r(off::MISC_VDM_QOS_LO).write(0xbbaa_9888);
        self.r(off::RC_TL_VDM_CTL1).write(0);
        self.r(off::RC_TL_VDM_CTL0).update(|v| v | 0x7_0000);
        self.r(off::MISC_CTRL1).update(|v| v | (1 << 5));
        ok
    }

    /// Schrijft een intern PHY-register (port [19:16], regad [15:0]) en wacht
    /// tot bit 31 zakt.
    fn mdio_write(&self, port: u32, regad: u32, data: u16) -> bool {
        self.r(off::MDIO_ADDR)
            .write(((port & 0xf) << 16) | (regad & 0xffff));
        let _ = self.r(off::MDIO_ADDR).read();
        let w = self.r(off::MDIO_WR_DATA);
        w.write((1 << 31) | u32::from(data));
        (0..1000).any(|_| w.read() & (1 << 31) == 0)
    }

    /// Lost PERST# en wacht op de link (PCIe CEM: 100 ms, dan pollen tot
    /// 200 ms; Linux wacht 100). Geeft (PHY-link, DL actief).
    pub fn start_link(&self) -> (bool, bool) {
        if self.speed != 0 {
            self.r(off::CFG_LNKCTL2).update(|v| (v & !0xf) | self.speed);
            self.r(off::CFG_LINK_CAP)
                .update(|v| (v & !0xf) | self.speed);
        }
        self.r(self.hard_debug()).update(|v| {
            v & !(HD_CLKREQ_DEBUG | HD_REFCLK_OVRD_EN | HD_REFCLK_OVRD_OUT | HD_L1SS_ENABLE)
        });
        self.perst(false);
        delay(self.clock, 100_000_000);
        for _ in 0..40 {
            let l = self.link_status();
            if l.0 && l.1 {
                return l;
            }
            delay(self.clock, 5_000_000);
        }
        self.link_status()
    }

    /// PHYLINKUP en DL_ACTIVE.
    #[must_use]
    pub fn link_status(&self) -> (bool, bool) {
        let s = self.status();
        (s & STATUS_PHY_LINK_UP != 0, s & STATUS_DL_ACTIVE != 0)
    }

    /// Het rauwe PCIE_STATUS (diagnose).
    #[must_use]
    pub fn status(&self) -> u32 {
        self.r(off::MISC_PCIE_STATUS).read()
    }

    fn cfg(&self, bus: u8, dev: u8, func: u8, o: u64) -> &'static Reg<u32> {
        if bus == 0 {
            return self.r(o & !3);
        }
        self.r(off::EXT_CFG_INDEX).write(ecam_index(bus, dev, func));
        dev::mb();
        self.r(off::EXT_CFG_DATA + (o & 0xffc))
    }

    /// Configruimte lezen: bus 0 = de RC zelf, dieper via het
    /// EXT_CFG-venster. Nooit bus 1 of dieper zonder DL_ACTIVE: dat is een
    /// bus-abort.
    #[must_use]
    pub fn cfg_read32(&self, bus: u8, dev: u8, func: u8, o: u64) -> u32 {
        self.cfg(bus, dev, func, o).read()
    }

    /// Configruimte schrijven.
    pub fn cfg_write32(&self, bus: u8, dev: u8, func: u8, o: u64, v: u32) {
        self.cfg(bus, dev, func, o).write(v);
        dev::mb();
    }

    /// De type-1-header van de RC: busnummers (secondary = subordinate =
    /// 1), het memory-window over het hele outbound-PCIe-bereik, en
    /// memory-decode plus bus-mastering.
    pub fn open_bridge(&self) {
        self.r(off::CFG_PRIMARY_BUS)
            .update(|v| (v & !0xff_ffff) | 0x01_0100);
        let base = self.out.pcie as u32;
        let limit = (self.out.pcie + self.out.size - 1) as u32;
        self.r(off::CFG_MEM_BASE)
            .write((limit & 0xfff0_0000) | ((base >> 16) & 0xfff0));
        self.r(off::CFG_COMMAND).update(|v| v | 0x6);
    }

    /// De hele bring-up: RESCAL, setup, link, bridge, de endpoint toetsen,
    /// zijn BAR's toewijzen (niemand anders doet het: HOP boot zonder
    /// firmware-hulp) en memory-decode plus bus-mastering aan.
    ///
    /// # Safety
    ///
    /// `rescal_base` is 0 of het gemapte RESCAL-blok.
    pub unsafe fn bring_up(&self, rescal_base: u64, want: u32, bars: &[EpBar]) -> Result {
        // SAFETY: de voorwaarde van deze functie.
        unsafe { self.bring_up_closed(rescal_base, want, bars) }?;
        self.open_endpoint();
        Ok(())
    }

    /// De bring-up tot en met de BAR's, met de endpoint nog dicht: zijn
    /// command-register blijft zoals de reset hem liet (geen memory-decode,
    /// geen bus-mastering). Dat is de stand waarin Linux een endpoint aan
    /// zijn driver geeft (`pci_host_probe` wijst BAR's toe, pas
    /// `pci_enable_device` in de driver opent hem), en de stand waarin de
    /// VideoCore de VL805 van de Pi 4 zijn firmware geeft. Daarna
    /// [`open_endpoint`](Self::open_endpoint).
    ///
    /// # Safety
    ///
    /// `rescal_base` is 0 of het gemapte RESCAL-blok.
    pub unsafe fn bring_up_closed(&self, rescal_base: u64, want: u32, bars: &[EpBar]) -> Result {
        // SAFETY: de voorwaarde van deze functie.
        if rescal_base != 0 && !unsafe { rescal(Pa(rescal_base), self.clock) } {
            return Err(Error::Rescal);
        }
        if !self.setup().0 {
            return Err(Error::NotRc {
                status: self.status(),
            });
        }
        if !self.start_link().1 {
            return Err(Error::NoLink {
                status: self.status(),
            });
        }
        self.open_bridge();
        if want != 0 {
            let found = self.cfg_read32(1, 0, 0, 0);
            if found != want {
                return Err(Error::Endpoint { found, want });
            }
        }
        for b in bars {
            self.cfg_write32(1, 0, 0, b.off, b.val);
        }
        Ok(())
    }

    /// Memory-decode en bus-mastering aan op de endpoint (bus 1, dev 0,
    /// fn 0): vanaf hier antwoordt hij op zijn BAR's.
    pub fn open_endpoint(&self) {
        let cmd = self.cfg_read32(1, 0, 0, off::CFG_COMMAND);
        self.cfg_write32(1, 0, 0, off::CFG_COMMAND, cmd | 0x6);
    }

    /// Het rauwe MISC_CTRL (diagnose: SCB0_SIZE staat in [31:27]).
    #[must_use]
    pub fn misc_ctrl(&self) -> u32 {
        self.r(off::MISC_CTRL).read()
    }
}
