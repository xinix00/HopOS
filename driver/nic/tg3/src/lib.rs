//! Broadcom's NetXtreme-familie (tg3), zoals hij in élke Mac mini zit: de
//! BCM57762 achter een Apple-PCIe-rootpoort (14e4:1682, PCIe x1 Gen1, ASIC
//! 0x57766).
//!
//! Referentie is Linux' `drivers/net/ethernet/broadcom/tg3.c`: daar komen de
//! registerkaart, de resetvolgorde en de firmware-handshake vandaan. Wat hier
//! staat is het kleinste deel daarvan dat een node aan het netwerk krijgt. De
//! Go-voorganger is `OLD/metal/driver/nic/tg3`, en de avonden die hem aan de
//! praat kregen staan in `OLD/docs/v1/archief/apple-m4.md` ("Het netwerk:
//! WERKT", 29-08).
//!
//! Wat deze driver NIET doet, en waarom dat mag: geen MSI (het board bedraadt
//! INTx, of pollt), geen jumbo-frames, geen TSO of checksum-offload, geen
//! statistieken-DMA, geen WoL, geen ASF/firmware-management. De jumbo- en
//! mini-ringen blijven leeg: één standaard-ring van 2 KB-buffers dekt
//! 1500-byte frames.
//!
//! DMA-model: de ringen en buffers liggen in één aaneengesloten regio die het
//! board aanwijst (op Apple de NetDMA-regio uit het PA-plan, met de DART in
//! bypass: een DMA-adres is dan gewoon een fysiek adres). De driver doet
//! barrières rond elke ring-update, want de NIC leest ze asynchroon, en
//! `push`/`pull` rond de buffers, zodat een board het bufferblok gecached mag
//! mappen ([`BUF_OFF`], [`BUF_LEN`]).
//!
//! Wie `&mut self` heeft, is de enige die de ringen aanraakt. Volgorde, zoals
//! Go's `New`, `Reset`, `SetMAC`, `Init`: [`Tg3::new`] (reset, MAC, MDIO en
//! PHY-id, ringen, engines), dan [`Tg3::link_up`] (wachten op de PHY, en de
//! poortmodus die bij de snelheid hoort).
//!
//! # De drie lessen van 29-08
//!
//! Elk goed voor een avond, en elk staat bij de plek waar hij geldt:
//!
//! 1. `MISC_HOST_CTRL_INDIR_ACCESS` moet aan vóór de eerste toegang, en
//!    opnieuw na de core-clock-reset (`enable_reg_access`).
//! 2. De core-clock-reset wist ook `PCI_COMMAND`: bus-master weg is geen DMA
//!    (`reset`).
//! 3. `RCVDBDI_STD_BD + NIC_ADDR` moet geschreven worden: een 57766 is
//!    `57765_CLASS` en `57765_PLUS`, maar géén `5717_PLUS` (`announce_rx_std`).

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

mod diag;
mod regs;

use core::fmt;
use core::mem::{offset_of, size_of};
use dev::{Pa, Reg};
use driver_mdio::{self as mdio, Mdio};
use netdev::{Mac, TxError};
use regs::*;
use sync::Signal;

pub use diag::{Counters, Describe, IrqDiag, RcbDump, SelfTest, Stats};
pub use driver_mdio::Link;

/// Broadcom.
pub const VENDOR: u16 = 0x14e4;
/// BCM57762: de NIC van de Mac mini (ASIC 0x57766).
pub const DEVICE_57762: u16 = 0x1682;

/// Drijft deze driver `vendor:device`? Alleen wat op ijzer bewezen is: de
/// 57765-afstemming staat onvoorwaardelijk in de reset, dus een andere
/// tg3-familie krijgt hem niet stilzwijgend.
#[must_use]
pub fn drives(vendor: u16, device: u16) -> bool {
    vendor == VENDOR && device == DEVICE_57762
}

/// Hoeveel van BAR0 het board moet mappen: het hoogste register dat de
/// driver raakt is TG3_PCIE_DL_LO_FTSMAX op 0x7c0c.
pub const BAR0_LEN: u64 = 0x8000;

const _: () = assert!(size_of::<Regs>() as u64 <= BAR0_LEN);

// ── De DMA-regio ────────────────────────────────────────────────────────────
//
// Eén standaard-ring van 2 KB-buffers (geen jumbo, geen mini), één
// return-ring en één send-ring. De NIC schrijft zijn voortgang in het
// status-blok. De descriptors staan in host-volgorde (little-endian); de
// swap-bits in GRC_MODE zorgen daarvoor.

/// Producer-ring: TG3_RX_STD_MAX_SIZE_5700, genoeg, en zonder
/// LRG_PROD_RING_CAP.
const RX_STD_RING: u32 = 512;
/// Return-ring: even groot als de producer-ring.
const RX_RET_RING: u32 = 512;
/// Send-ring: TG3_TX_RING_SIZE.
const TX_RING: u32 = 512;
/// Eén pakketbuffer: 1536 payload (TG3_RX_STD_DMA_SZ) op een 2 KB-korrel.
const BUF_SIZE: u64 = 2048;
/// Wat de chip in een RX-buffer mag schrijven, FCS inbegrepen.
const RX_DMA_SIZE: u32 = 1536;
/// De FCS telt mee in de ontvangen lengte en hoort niet bij het frame
/// (`tg3_rx`: `- ETH_FCS_LEN`).
const ETH_FCS_LEN: u32 = 4;
/// Korter dan een Ethernet-kop is geen frame.
const ETH_HLEN: u32 = 14;
/// MAC_RX_MTU_SIZE: MTU plus Ethernet-kop, FCS en een VLAN-tag.
const RX_MTU: u32 = netdev::MTU as u32 + ETH_HLEN + ETH_FCS_LEN + 4;
/// Bij uitgestelde doorbells flusht RX zelf om de zoveel frames: de NIC mag
/// nooit zonder std-buffers zitten, ook niet in een lange burst.
const RX_SELF_FLUSH: u32 = 32;

/// `struct tg3_rx_buffer_desc`: de producer- en de return-ring.
#[repr(C)]
struct RxBd {
    addr_hi: u32,
    addr_lo: u32,
    /// Lengte in de lage helft (met FCS), de index in de hoge.
    idx_len: u32,
    type_flags: u32,
    ip_tcp_csum: u32,
    err_vlan: u32,
    reserved: u32,
    /// Wat de host erin zette, komt ongewijzigd terug: bij ons de index van
    /// de buffer.
    opaque: u32,
}

/// `struct tg3_tx_buffer_desc`.
#[repr(C)]
struct TxBd {
    addr_hi: u32,
    addr_lo: u32,
    /// Lengte in de hoge helft, vlaggen in de lage.
    len_flags: u32,
    vlan_tag: u32,
}

const RX_BD: u64 = size_of::<RxBd>() as u64;
const TX_BD: u64 = size_of::<TxBd>() as u64;

/// Status-blok (80 bytes; ruim op een eigen pagina).
const OFF_STATUS: u64 = 0x0000;
/// De lengte van het status-blok (`struct tg3_hw_status`).
const STATUS_LEN: u64 = 80;
/// Het status-woord (bit 0 = UPDATED).
const STATUS_WORD: u64 = 0;
/// `idx[0]`: rx_producer in de lage helft, tx_consumer in de hoge.
const STATUS_IDX0: u64 = 16;
/// De producer-ring.
const OFF_RX_STD: u64 = 0x1000;
/// De return-ring.
const OFF_RX_RET: u64 = OFF_RX_STD + RX_STD_RING as u64 * RX_BD;
/// De send-ring.
const OFF_TX_BD: u64 = OFF_RX_RET + RX_RET_RING as u64 * RX_BD;

/// Waar de pakketbuffers beginnen: op een eigen 2 MB-grens. Descriptors,
/// status-blok en ringen blijven in het eerste blok (device of NC, per woord
/// gepold); de buffers mag het board gecached mappen, want de driver doet
/// `pull` na DMA-in en `push` vóór DMA-uit. RX en TX samen zijn precies 2 MB.
pub const BUF_OFF: u64 = 0x20_0000;
const OFF_RX_BUF: u64 = BUF_OFF;
const OFF_TX_BUF: u64 = OFF_RX_BUF + RX_STD_RING as u64 * BUF_SIZE;
/// Wat de driver aan DMA-geheugen vraagt (het board reserveert het in zijn
/// PA-plan; op Apple is dat de NetDMA-regio).
pub const DMA_NEED: u64 = OFF_TX_BUF + TX_RING as u64 * BUF_SIZE;
/// De maat van het bufferblok vanaf [`BUF_OFF`] (RX en TX).
pub const BUF_LEN: u64 = DMA_NEED - BUF_OFF;
/// De uitlijning die de driver van de DMA-regio eist: een pagina. Wie de
/// buffers gecached wil mappen, geeft een 2 MB-gealigneerde regio.
const DMA_ALIGN: u64 = 0x1000;

const _: () = {
    assert!(RX_BD == 32);
    assert!(TX_BD == 16);
    assert!(offset_of!(RxBd, addr_hi) == 0);
    assert!(offset_of!(RxBd, addr_lo) == 4);
    assert!(offset_of!(RxBd, idx_len) == 8);
    assert!(offset_of!(RxBd, type_flags) == 12);
    assert!(offset_of!(RxBd, ip_tcp_csum) == 16);
    assert!(offset_of!(RxBd, err_vlan) == 20);
    assert!(offset_of!(RxBd, reserved) == 24);
    assert!(offset_of!(RxBd, opaque) == 28);
    assert!(offset_of!(TxBd, addr_hi) == 0);
    assert!(offset_of!(TxBd, addr_lo) == 4);
    assert!(offset_of!(TxBd, len_flags) == 8);
    assert!(offset_of!(TxBd, vlan_tag) == 12);
    // Het status-blok en de drie ringen passen achter elkaar vóór de buffers.
    assert!(OFF_STATUS + STATUS_LEN <= OFF_RX_STD);
    assert!(OFF_RX_STD == 0x1000);
    assert!(OFF_RX_RET == 0x5000);
    assert!(OFF_TX_BD == 0x9000);
    assert!(OFF_TX_BD + TX_RING as u64 * TX_BD <= BUF_OFF);
    assert!(OFF_RX_STD.is_multiple_of(DMA_ALIGN));
    // De buffers: RX en TX samen precies 2 MB, de regio 4 MB (Go's
    // NeedBytes).
    assert!(BUF_LEN == 0x20_0000);
    assert!(DMA_NEED == 0x40_0000);
    // Een buffer draagt het grootste frame, met FCS, en de ringmaten zijn
    // machten van twee (de chip rekent modulo).
    assert!(RX_DMA_SIZE as u64 <= BUF_SIZE);
    assert!(RX_MTU <= RX_DMA_SIZE);
    assert!(netdev::MAX_FRAME as u64 <= BUF_SIZE);
    assert!(RX_STD_RING.is_power_of_two() && RX_RET_RING.is_power_of_two());
    assert!(TX_RING.is_power_of_two());
    // De index past in `opaque` en in de 16-bit mailboxen.
    assert!(RX_STD_RING <= 0xffff && TX_RING <= 0xffff);
};

// ── Fouten ──────────────────────────────────────────────────────────────────

/// Waarom de driver weigert.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    /// De DMA-regio is te klein, niet gealigneerd of nul.
    Dma {
        /// De basis.
        base: u64,
        /// De maat.
        size: u64,
    },
    /// Na de core-clock-reset draagt de config-shadow geen Broadcom-id.
    NoAnswer {
        /// Wat er op offset 0 van BAR0 stond.
        id: u32,
    },
    /// De buffer-manager ging niet aan.
    BufMgr {
        /// BUFMGR_MODE.
        mode: u32,
    },
    /// Een MDIO-transactie rondde niet af binnen 50 ms.
    Mdio {
        /// Het PHY-adres.
        phy: u8,
        /// Het register.
        reg: u8,
        /// Schrijven (anders lezen).
        write: bool,
    },
    /// Niemand thuis op de MDIO-bus: id 0 of alles-enen.
    NoPhy {
        /// Het PHY-id (reg 2 en 3).
        id: u32,
    },
    /// Geen link binnen de grens.
    NoLink {
        /// De laatste BMSR.
        bmsr: u16,
        /// De grens in milliseconden.
        ms: u64,
    },
    /// De auxiliary status meldt een snelheid die we niet kennen.
    LinkState {
        /// MII_AUX_STAT.
        aux: u16,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::Dma { base, size } => write!(
                f,
                "tg3: dma region {size:#x} at {base:#x} invalid (need {DMA_NEED:#x}, {DMA_ALIGN:#x} aligned)"
            ),
            Self::NoAnswer { id } => {
                write!(f, "tg3: chip did not answer after reset (id {id:#x})")
            }
            Self::BufMgr { mode } => {
                write!(f, "tg3: buffer manager did not start (mode {mode:#x})")
            }
            Self::Mdio { phy, reg, write } => write!(
                f,
                "tg3: MDIO {} phy {phy} reg {reg} timed out",
                if write { "write" } else { "read" }
            ),
            Self::NoPhy { id } => write!(f, "tg3: no PHY on the MDIO bus (id {id:#x})"),
            Self::NoLink { bmsr, ms } => write!(
                f,
                "tg3: no link within {ms} ms (cable plugged in? BMSR {bmsr:#06x})"
            ),
            Self::LinkState { aux } => write!(f, "tg3: unknown link state (aux {aux:#x})"),
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

// ── Tijden ──────────────────────────────────────────────────────────────────

const US: u64 = 1_000;
const MS: u64 = 1_000_000;
/// Hoe lang een MDIO-transactie mag duren.
const MDIO_NS: u64 = 50 * MS;
/// Tussen twee link-kijkjes.
const LINK_POLL_NS: u64 = 20 * MS;

/// Schrijft een register en zet er een barrière achter, zoals Go's `wr`: de
/// volgorde van de init is die van tg3, en die moet het device ook zo zien.
fn wr(reg: &Reg<u32>, v: u32) {
    reg.write(v);
    dev::mb();
}

/// Het interrupt-pad: alleen de interrupt-mailbox. `Copy`, zodat het board
/// hem naast de driver houdt voor de ack.
#[derive(Clone, Copy)]
pub struct IrqAck {
    bar0: Pa,
}

impl IrqAck {
    /// Maskeert de interrupt (mailbox 1, `tg3_disable_ints`); INTA# valt en
    /// de poortlijn zakt mee.
    ///
    /// Het UPDATED-bit van het status-blok wissen (zoals `tg3_interrupt`
    /// doet) is in Go HIER geprobeerd en teruggedraaid: daarna kwam er geen
    /// interrupt meer (bundel 37, 20-09: 0 irq/s, cyclus terug op 21 ms).
    pub fn ack(&self) {
        // SAFETY: `bar0` kwam uit `Tg3::new`, dat een gemapt blok eiste; de
        // mailbox deelt niets met de ringen.
        let r: &Regs = unsafe { dev::regs(self.bar0) };
        r.mb_interrupt.write(1);
        // Terug lezen duwt de posted write de PCIe-brug uit
        // (`tw32_mailbox_f`).
        let _ = r.mb_interrupt.read();
    }
}

/// Eén BCM57762.
pub struct Tg3 {
    bar0: Pa,
    /// De ECAM-config-space van de functie; `Pa(0)` = de BAR0-spiegel.
    cfg: Pa,
    mac: Mac,
    now: fn() -> u64,
    dma: Pa,
    /// Volgende plek in de producer-ring die wij vullen.
    rx_std_idx: u32,
    /// Onze consumer-index in de return-ring.
    rx_ret_idx: u32,
    /// Onze producer-index in de send-ring.
    tx_prod: u32,
    /// Frames sinds de laatste RX-flush.
    rx_since: u32,
    /// Mailboxen nog niet geschreven sinds de laatste flush.
    rx_dirty: bool,
    tx_dirty: bool,
    /// Laatste waarde uit de firmware-mailbox (diagnose).
    fw_mbox: u32,
    /// Het PHY-id uit de probe.
    phy_id: u32,
    irq: Option<&'static Signal>,
    /// Meetlat: RX-descriptors met een fout of een kromme lengte.
    pub rx_bad: u64,
    /// Meetlat: TX-doorbells.
    pub doorbells: u64,
    /// Meetlat: `transmit` op een volle ring.
    pub tx_full: u64,
}

impl Tg3 {
    /// Reset, MAC, MDIO en PHY-id, de ringen, en de engines aan: Go's `New`,
    /// `Reset`, `SetMAC`, de PHY-controle van het board, en `Init`. De link
    /// komt daarna ([`link_up`](Self::link_up)). `now` geeft monotone
    /// nanoseconden; alle wachttijden zijn begrensd pollen op die klok.
    ///
    /// Het MAC komt van buiten omdat de chip het na een PERST niet meer weet:
    /// er staat dan Broadcom's default (00:10:18:00:00:00) in MAC_ADDR_0, en
    /// het echte adres woont in de ADT (Apple) of in NVRAM.
    ///
    /// # Safety
    ///
    /// `bar0` is BAR0 van een BCM57762, gemapt als Device voor minstens
    /// [`BAR0_LEN`] bytes en voor altijd. `cfg` is de config-space (ECAM, 4
    /// KB, Device) van dezelfde functie, voor altijd gemapt, of `Pa(0)` voor
    /// de spiegel in BAR0. `[dma, dma + dma_size)` is gemapt geheugen dat
    /// alleen deze driver en het device gebruiken, nu en zolang het programma
    /// draait, op een adres dat het device ziet zoals de CPU (de DART in
    /// bypass), en niet gecached gemapt buiten het bufferblok.
    pub unsafe fn new(
        bar0: Pa,
        cfg: Pa,
        mac: Mac,
        dma: Pa,
        dma_size: u64,
        now: fn() -> u64,
    ) -> Result<Self> {
        if dma_size < DMA_NEED || !dma.is_aligned(DMA_ALIGN) || dma.0 == 0 {
            return Err(Error::Dma {
                base: dma.0,
                size: dma_size,
            });
        }
        let mut n = Self::at(bar0, cfg, mac, dma, now);
        n.reset()?;
        n.set_mac();
        n.phy_id = n.phy_id()?;
        if n.phy_id == 0 || n.phy_id == u32::MAX {
            return Err(Error::NoPhy { id: n.phy_id });
        }
        n.init()?;
        Ok(n)
    }

    /// De staat zonder één registertoegang (ook voor de tests).
    fn at(bar0: Pa, cfg: Pa, mac: Mac, dma: Pa, now: fn() -> u64) -> Self {
        Self {
            bar0,
            cfg,
            mac,
            now,
            dma,
            rx_std_idx: 0,
            rx_ret_idx: 0,
            tx_prod: 0,
            rx_since: 0,
            rx_dirty: false,
            tx_dirty: false,
            fw_mbox: 0,
            phy_id: 0,
            irq: None,
            rx_bad: 0,
            doorbells: 0,
            tx_full: 0,
        }
    }

    fn regs(&self) -> &'static Regs {
        // SAFETY: de voorwaarde van `new`: `bar0` is een gemapt BAR0 van
        // minstens `BAR0_LEN` bytes, en `Regs` past daarin (assertie).
        unsafe { dev::regs(self.bar0) }
    }

    /// De echte PCI-config-space van deze functie, of de spiegel aan het
    /// begin van BAR0 als het board geen ECAM-adres gaf.
    fn config(&self) -> &'static Config {
        if self.cfg.0 == 0 {
            return &self.regs().cfg;
        }
        // SAFETY: de voorwaarde van `new`: `cfg` is de gemapte config-space
        // (4 KB) van deze functie, en `Config` is er de eerste 0x100 bytes
        // van.
        unsafe { dev::regs(self.cfg) }
    }

    /// Spint `ns` nanoseconden op de klok.
    fn sleep(&self, ns: u64) {
        let until = (self.now)().saturating_add(ns);
        while (self.now)() < until {
            core::hint::spin_loop();
        }
    }

    /// Wacht begrensd tot `done`, met `pause` nanoseconden tussen twee
    /// blikken; na de grens telt nog één laatste blik.
    fn wait(&self, budget: u64, pause: u64, mut done: impl FnMut(&Self) -> bool) -> bool {
        let deadline = (self.now)().saturating_add(budget);
        loop {
            if done(self) {
                return true;
            }
            if (self.now)() >= deadline {
                return done(self);
            }
            if pause > 0 {
                self.sleep(pause);
            } else {
                core::hint::spin_loop();
            }
        }
    }

    // ── Reset ───────────────────────────────────────────────────────────────

    /// `tg3_enable_register_access`: MISC_HOST_CTRL zo zetten dat
    /// register-toegang werkt. Het bit dat telt is INDIR_ACCESS: zonder dat
    /// bestaat het indirecte venster niet, en dus ook de weg naar NIC-SRAM
    /// niet. Dat is waar de ring-control-blocks van de send- en return-ring
    /// wonen; blijven die leeg, dan weet de chip niet waar zijn ringen liggen
    /// en doet hij geen enkele DMA (29-08: het venster las nul, de link stond
    /// keurig op 1 Gb/s en er gebeurde verder niets). De CHIPREV-bits (31:16)
    /// zijn read-only maar schrijven we mee terug, precies zoals tg3.
    fn enable_reg_access(&self) {
        let c = self.config();
        let v = (c.misc_host_ctrl.read() & MISC_CHIPREV_MASK)
            | MISC_MASK_PCI_INT
            | MISC_WORD_SWAP
            | MISC_INDIR_ACCESS
            | MISC_PCISTATE_RW;
        wr(&c.misc_host_ctrl, v);
    }

    /// De core-clock-reset, en de chip terug in een staat waarin hij
    /// aanspreekbaar is: `tg3_chip_reset` teruggebracht tot wat een kale
    /// bring-up nodig heeft. Geen NVRAM-lock, geen APE-lock, geen
    /// ASF-handshake (management-firmware draait hier niet).
    ///
    /// Wat er WEL in moet, en wat drie avonden kostte om te vinden (29-08):
    /// de reset wist het indirecte venster én de command-bits in de config
    /// space. Zonder ze terug te zetten staat de link keurig op 1 Gb/s en
    /// gebeurt er verder niets.
    fn reset(&mut self) -> Result {
        // Vóór élke andere toegang: het indirecte venster aan.
        self.enable_reg_access();

        // De core-clock-reset wist memory- en bus-master-enable in
        // PCI_COMMAND (`tg3_save_pci_state`); zonder bus-master doet de chip
        // daarna geen DMA.
        let c = self.config();
        let cmd = c.command.read();

        // MAC uit, dan de reset zelf. Op PCIe-chips schrijft tg3
        // GRC_MISC_CFG in twee stappen: eerst bit 29 alleen, daarna bit 29
        // mét CORECLK_RESET. Geen read-modify-write: de resetwaarde van het
        // register is wat we willen.
        let r = self.regs();
        wr(&r.mac_mode, MAC_MODE_HALF_DUPLEX);
        self.sleep(MS);
        wr(&r.grc_misc_cfg, GRC_MISC_CFG_PCIE);
        wr(
            &r.grc_misc_cfg,
            GRC_MISC_CFG_PCIE | GRC_MISC_CFG_CORECLK_RESET,
        );
        self.sleep(120 * US);
        let _ = c.command.read(); // posted write eruit duwen; tg3 doet exact dit
        self.sleep(120 * US);

        // `tg3_restore_pci_state`: venster terug, retry-gedrag, command-bits
        // terug.
        self.enable_reg_access();
        wr(&c.pci_state, PCISTATE_ROM_ENABLE | PCISTATE_ROM_RETRY);
        wr(&c.command, cmd);

        // De memory arbiter moet aan voordat er iets uit NIC-SRAM komt: hij
        // regelt het verkeer naar dat geheugen, en zonder hem kan de chip
        // ook zijn eigen ring-control-blocks niet ophalen.
        wr(&r.memarb_mode, r.memarb_mode.read() | MODE_ENABLE);

        // Antwoordt hij weer? De config-shadow op offset 0 draagt
        // vendor/device.
        let answers = |n: &Self| n.regs().cfg.id.read() & 0xffff == u32::from(VENDOR);
        if !self.wait(500 * MS, 5 * MS, answers) {
            return Err(Error::NoAnswer {
                id: r.cfg.id.read(),
            });
        }

        self.fw_mbox = self.poll_firmware();
        wr(&r.grc_mode, GRC_MODE_SWAP_DATA | GRC_MODE_WSWAP_NONFRM);
        self.tune_57765();
        Ok(())
    }

    /// `tg3_poll_fw`: wacht tot de bootcode van de chip klaar is. Die zet
    /// MAGIC1 in de firmware-mailbox en daarna het complement. Uitblijven is
    /// geen fout (niet elke kaart draagt firmware), maar de waarde is wél de
    /// eerlijkste meting dat het SRAM-venster werkt: een dood venster geeft
    /// nul, een levend `0xb49a89ab`.
    fn poll_firmware(&self) -> u32 {
        let mut val = 0;
        self.wait(1_000 * MS, 0, |n| {
            val = n.read_mem(SRAM_FW_MBOX);
            val == !FW_MBOX_MAGIC1
        });
        val
    }

    /// Wat tg3 extra doet voor de 57765/57766-familie, in dezelfde volgorde
    /// als `tg3_reset_hw`. Drie dingen die je niet zelf verzint: een
    /// pad-ring-deler tegen zend-hangers, een PCIe-DL-instelling voor het
    /// aantal fast-training-sequences, en de klok voor 10 Mb/s. En daarna
    /// DMA_RW_CTRL: de lees/schrijf-commando's en de cache-uitlijning van de
    /// DMA-engines, die tg3 uit zijn eigen DMA-test haalt en voor deze
    /// familie op een vaste waarde zet.
    fn tune_57765(&self) {
        let r = self.regs();
        wr(
            &r.cpmu_padrng_ctl,
            r.cpmu_padrng_ctl.read() | CPMU_PADRNG_CTL_RDIV2,
        );

        // De lage 1K van het PCIe-DL-blok is alleen zichtbaar via een venster
        // in GRC_MODE; daarna het venster netjes terugzetten.
        let grc = r.grc_mode.read();
        wr(
            &r.grc_mode,
            (grc & !GRC_MODE_PCIE_PORT_MASK) | GRC_MODE_PCIE_DL_SEL,
        );
        let v = r.pcie_dl_lo_ftsmax.read() & !PCIE_DL_LO_FTSMAX_MASK;
        wr(&r.pcie_dl_lo_ftsmax, v | PCIE_DL_LO_FTSMAX_VAL);
        wr(&r.grc_mode, grc);

        let clk = r.cpmu_lspd_10mb_clk.read() & !CPMU_LSPD_10MB_MACCLK_MASK;
        wr(&r.cpmu_lspd_10mb_clk, clk | CPMU_LSPD_10MB_MACCLK_6_25);

        let c = &r.cfg;
        let dma = c.dma_rw_ctrl.read() & !DMA_RWCTRL_DIS_CACHE_ALIGN;
        wr(
            &c.dma_rw_ctrl,
            dma | DMA_RWCTRL_WRITE_CMD | DMA_RWCTRL_READ_CMD | DMA_RWCTRL_DIS_CACHE_ALIGN,
        );
    }

    /// Het MAC-adres in MAC_ADDR_0 (en daarmee in het RX-filter).
    fn set_mac(&self) {
        let r = self.regs();
        let m = self.mac.0;
        wr(&r.mac_addr0_high, u32::from(m[0]) << 8 | u32::from(m[1]));
        wr(
            &r.mac_addr0_low,
            u32::from_be_bytes([m[2], m[3], m[4], m[5]]),
        );
    }

    // ── NIC-SRAM ────────────────────────────────────────────────────────────

    /// Schrijft een woord in NIC-SRAM via het geheugenvenster
    /// (`tg3_write_mem`). Het venster gaat daarna terug op nul, zo laat tg3
    /// het ook achter. Werkt alleen met INDIR_ACCESS aan.
    ///
    /// LET OP de twee vensters: 0x78/0x80 is het REGISTER-venster, 0x7c/0x84
    /// het GEHEUGEN-venster. Ze door elkaar halen kostte drie boots: je
    /// schrijft de offset in het ene venster en de data in het andere, en
    /// leest dan je eigen offset terug (gemeten 29-08).
    fn write_mem(&self, off: u32, val: u32) {
        let c = &self.regs().cfg;
        c.mem_win_base.write(off);
        c.mem_win_data.write(val);
        c.mem_win_base.write(0);
        dev::mb();
    }

    /// Leest een woord uit NIC-SRAM via hetzelfde venster als `write_mem`.
    fn read_mem(&self, off: u32) -> u32 {
        let c = &self.regs().cfg;
        c.mem_win_base.write(off);
        let v = c.mem_win_data.read();
        c.mem_win_base.write(0);
        v
    }

    /// Een ring-control-block in NIC-SRAM: waar de ring in host-geheugen
    /// staat, hoe groot hij is, en (alleen voor de send-ring) waar zijn
    /// spiegel in NIC-SRAM ligt.
    fn set_bdinfo(&self, sram: u32, addr: Pa, maxlen_flags: u32, nic_addr: u32) {
        self.write_mem(sram, (addr.0 >> 32) as u32);
        self.write_mem(sram + 4, addr.0 as u32);
        self.write_mem(sram + 8, maxlen_flags);
        self.write_mem(sram + 12, nic_addr);
    }

    /// Schrijft een mailbox en leest hem terug: dat duwt de posted write de
    /// PCIe-brug uit, zoals tg3's `tw32_mailbox_f`.
    fn wr_mbox(reg: &Reg<u32>, v: u32) {
        reg.write(v);
        let _ = reg.read();
    }

    // ── Ringen en engines ───────────────────────────────────────────────────

    /// De ringen, dan de datapaden aan: `tg3_reset_hw` in zijn volgorde, en
    /// dat is geen toeval: de chip wil zijn ringen kennen vóór de engines
    /// lopen, en de engines vóór de MAC. Wat hier afwijkt van Linux is
    /// alleen wat we niet doen: geen jumbo, geen RSS/TSS, geen TSO-firmware,
    /// geen statistieken-DMA, geen MSI.
    fn init(&mut self) -> Result {
        let r = self.regs();
        dev::clear(self.dma, OFF_RX_BUF as usize); // status-blok en ringen leeg

        // GRC: host bezit de send-BD's, pseudo-header-checksum voor TX doen
        // wij niet in hardware, en de swap-bits horen erbij.
        wr(
            &r.grc_mode,
            GRC_MODE_SWAP_DATA
                | GRC_MODE_WSWAP_NONFRM
                | GRC_MODE_HOST_STACKUP
                | GRC_MODE_HOST_SENDBDS
                | GRC_MODE_NO_TX_PHDR_CSUM
                | GRC_MODE_IRQ_ON_MAC_ATTN,
        );

        self.start_bufmgr()?;

        // Wanneer vult de chip zijn eigen BD-cache bij
        // (`tg3_setup_rxbd_thresholds`).
        wr(&r.rcvbdi_std_thresh, RX_STD_RING / 8);
        wr(&r.std_replenish_lwm, BDCACHE_MAX);

        self.fill_rx_std();
        self.announce_rx_std();
        self.reset_rings();

        let st = self.dma.add(OFF_STATUS);
        wr(&r.hostcc_status_hi, (st.0 >> 32) as u32);
        wr(&r.hostcc_status_lo, st.0 as u32);

        // Return- en send-ring: hun RCB's staan in NIC-SRAM.
        self.set_bdinfo(
            SRAM_SEND_RCB,
            self.dma.add(OFF_TX_BD),
            TX_RING << BDINFO_MAXLEN_SHIFT,
            SRAM_TX_BUFFER_DESC,
        );
        self.set_bdinfo(
            SRAM_RCV_RET_RCB,
            self.dma.add(OFF_RX_RET),
            RX_RET_RING << BDINFO_MAXLEN_SHIFT,
            0,
        );

        self.setup_mac_rx();
        self.start_coalescing();
        self.start_engines();
        Ok(())
    }

    /// De buffer-manager: de waterlijnen voor 57765-plus
    /// (`tg3_init_bufmgr_config`), en wachten tot hij loopt.
    fn start_bufmgr(&self) -> Result {
        let r = self.regs();
        wr(&r.bufmgr_mb_rdma_low, 0x0);
        wr(&r.bufmgr_mb_macrx_low, 0x2a);
        wr(&r.bufmgr_mb_high, 0xa0);
        wr(&r.bufmgr_dma_low, 0x5);
        wr(&r.bufmgr_dma_high, 0xa);
        wr(&r.bufmgr_mode, MODE_ENABLE | MODE_ATTN);
        let on = |n: &Self| n.regs().bufmgr_mode.read() & MODE_ENABLE != 0;
        if self.wait(100 * MS, 0, on) {
            return Ok(());
        }
        Err(Error::BufMgr {
            mode: r.bufmgr_mode.read(),
        })
    }

    /// De producer-ring vullen: elke descriptor wijst naar zijn eigen buffer,
    /// en draagt in `opaque` zijn eigen index. De return-ring geeft die
    /// terug, en zo weten we welke buffer een binnengekomen frame draagt.
    fn fill_rx_std(&self) {
        for i in 0..RX_STD_RING {
            let d = self.rx_std_desc(i);
            let buf = self.rx_buf(i);
            dev::write32(d.add(off(offset_of!(RxBd, addr_hi))), (buf.0 >> 32) as u32);
            dev::write32(d.add(off(offset_of!(RxBd, addr_lo))), buf.0 as u32);
            // idx_len: de lengte in de lage helft.
            dev::write32(d.add(off(offset_of!(RxBd, idx_len))), RX_DMA_SIZE);
            // type_flags: END, anders neemt de chip hem niet (29-08).
            dev::write32(d.add(off(offset_of!(RxBd, type_flags))), RXD_FLAG_END);
            dev::write32(
                d.add(off(offset_of!(RxBd, opaque))),
                i | RXD_OPAQUE_RING_STD,
            );
        }
        dev::mb();
    }

    /// De producer-ring aanmelden, en alle buffers aanbieden.
    fn announce_rx_std(&mut self) {
        let r = self.regs();
        let bd = &r.rcvdbdi_std_bd;
        let ring = self.dma.add(OFF_RX_STD);
        wr(&bd.host_hi, (ring.0 >> 32) as u32);
        wr(&bd.host_lo, ring.0 as u32);
        // Waar de chip zijn eigen kopie van de producer-ring bewaart. Dit
        // veld is geen decoratie: laat je het staan, dan zoekt de
        // ontvangsteenheid zijn descriptors op het adres dat er toevallig in
        // stond (0x415a0ec3), leest daar rommel als bufferlengte, en meldt
        // voor élk frame FRM_TOO_BIG. tg3 slaat deze regel alleen over op de
        // 5717-familie; een 57766 is 57765_CLASS maar géén 5717_PLUS, en
        // heeft hem dus nodig (gekost: een avond, gevonden 29-08).
        wr(&bd.nic_addr, SRAM_RX_BUFFER_DESC);
        wr(&r.rcvdbdi_jumbo_bd.maxlen_flags, BDINFO_FLAGS_DISABLED); // geen jumbo
        // Op 57765-plus staat de ringgrootte in de bovenste helft van
        // maxlen_flags en de DMA-lengte maal vier in de lage.
        wr(
            &bd.maxlen_flags,
            RX_STD_RING << BDINFO_MAXLEN_SHIFT | RX_DMA_SIZE << 2,
        );

        // Alle buffers aanbieden: de producer wijst één voorbij de laatste.
        self.rx_std_idx = RX_STD_RING - 1;
        Self::wr_mbox(&r.mb_rx_std_prod, self.rx_std_idx);
    }

    /// `tg3_rings_reset`: de ongebruikte RCB's expliciet uit, en de
    /// mailboxen op nul. De chip loopt de RCB's anders af en leest rommel.
    /// De 57765-klasse heeft 2 send-ringen en 4 return-ringen, waarvan wij
    /// er van elk één gebruiken. Verder gaan dan de chip kent is niet
    /// onschuldig: dan schrijf je BDINFO-vlaggen over SRAM dat van de
    /// bootcode is.
    fn reset_rings(&mut self) {
        for i in 1..2 {
            self.write_mem(SRAM_SEND_RCB + i * BDINFO_SIZE + 8, BDINFO_FLAGS_DISABLED);
        }
        for i in 1..4 {
            self.write_mem(
                SRAM_RCV_RET_RCB + i * BDINFO_SIZE + 8,
                BDINFO_FLAGS_DISABLED,
            );
        }
        let r = self.regs();
        Self::wr_mbox(&r.mb_interrupt, 1); // gemaskeerd tot `irq_unmask`
        Self::wr_mbox(&r.mb_tx_prod, 0);
        Self::wr_mbox(&r.mb_rx_ret_cons, 0);
        self.tx_prod = 0;
        self.rx_ret_idx = 0;
    }

    /// MAC-adres, frame-maten en de ontvangstregel.
    fn setup_mac_rx(&self) {
        let r = self.regs();
        self.set_mac();
        // Het multicast-filter helemaal open. Een node hoort mDNS,
        // ARP-achtigen en IPv6-buurontdekking te zien; een hash bijhouden per
        // aangemeld adres is werk dat pas loont als er iets te winnen valt.
        for h in &r.hash {
            wr(h, u32::MAX);
        }
        // MAC_RX_MTU_SIZE staat na een reset niet vanzelf goed: is hij te
        // klein, dan gooit de MAC élk frame weg.
        wr(&r.rx_mtu_size, RX_MTU);
        wr(&r.tx_lengths, 2 << 12 | 6 << 8 | 32);
        wr(&r.rcv_rule_cfg, RCV_RULE_DEFAULT_CLASS);
        wr(&r.rcvlpc_config, 0x0181);

        // De statistieken-tellers. Wij lezen ze via de registers, maar de
        // blokken willen ze aan hebben staan (tg3 doet dit onvoorwaardelijk).
        wr(
            &r.rcvlpc_stats_enable,
            r.rcvlpc_stats_enable.read() & !RCVLPC_STATSENAB_DACK_FIX,
        );
        wr(&r.rcvlpc_stats_ctrl, 1);
        wr(&r.snddatai_stats_enab, 0x00ff_ffff);
        wr(&r.snddatai_stats_ctrl, 0x3); // ENABLE | FASTUPD
    }

    /// Host coalescing: eerst uit, dan de drempels, dan aan met een 32-byte
    /// status-blok. Dat is wat de chip DMA't en waar wij in kijken.
    fn start_coalescing(&self) {
        let r = self.regs();
        wr(&r.hostcc_mode, 0);
        let off = |n: &Self| n.regs().hostcc_mode.read() & MODE_ENABLE == 0;
        let _ = self.wait(50 * MS, 0, off);
        wr(&r.hostcc_rx_ticks, 60);
        wr(&r.hostcc_tx_ticks, 60);
        wr(&r.hostcc_rx_frames, 1); // één frame is genoeg om het blok bij te werken
        wr(&r.hostcc_tx_frames, 1);
        wr(&r.hostcc_mode, MODE_ENABLE | HOSTCC_MODE_32BYTE);
    }

    /// De ontvangst- en zendblokken, de DMA-engines en de MAC, in tg3's
    /// volgorde.
    fn start_engines(&self) {
        let r = self.regs();
        wr(&r.rcvcc_mode, MODE_ENABLE | MODE_ATTN);
        // De attentie-bits erbij (tg3 zet alleen ENABLE): zonder deze latcht
        // RCVLPC_STATUS niet, en dan zegt een weggegooid frame niets over de
        // reden. CLASS0 = frame in de wegwerpklasse, MAPOOR = mbuf-pool op.
        wr(
            &r.rcvlpc_mode,
            MODE_ENABLE | RCVLPC_CLASS0_ATTN | RCVLPC_MAPOOR_ATTN | RCVLPC_STATSOFLOW_ATTN,
        );

        // De MAC-data-engines aan. Zonder deze drie staat de link en gebeurt
        // er niets: geen enkel frame komt binnen (gemeten 29-08).
        wr(
            &r.mac_mode,
            r.mac_mode.read() | MAC_MODE_RUN | MAC_MODE_RXSTAT_CLEAR | MAC_MODE_TXSTAT_CLEAR,
        );
        self.sleep(40 * US);

        // De DMA-engines. De FIFO-overflow-fix hoort bij 57765-plus.
        wr(
            &r.rdma_rsrvctrl,
            r.rdma_rsrvctrl.read() | RDMA_RSRVCTRL_FIFO_OFLW_FIX,
        );
        wr(
            &r.wdmac_mode,
            MODE_ENABLE | DMAC_ERR_ENAB | WDMAC_STATUS_TAG_FIX,
        );
        self.sleep(40 * US);
        wr(
            &r.rdmac_mode,
            MODE_ENABLE
                | DMAC_ERR_ENAB
                | RDMAC_FIFO_LONG_BURST
                | RDMAC_IPV6_LSO_EN
                | RDMAC_JMB_2K_MMRR,
        );
        self.sleep(40 * US);

        // En de blokken erboven.
        wr(&r.rcvdcc_mode, MODE_ENABLE | MODE_ATTN);
        wr(&r.snddatac_mode, MODE_ENABLE);
        wr(&r.sndbdc_mode, MODE_ENABLE | MODE_ATTN);
        wr(&r.rcvbdi_mode, MODE_ENABLE | RCVBDI_RCB_ATTN);
        wr(&r.rcvdbdi_mode, MODE_ENABLE | RCVDBDI_INV_RING_SZ);
        wr(&r.snddatai_mode, MODE_ENABLE);
        wr(&r.sndbdi_mode, MODE_ENABLE | MODE_ATTN);
        wr(&r.sndbds_mode, MODE_ENABLE | MODE_ATTN);

        // Pas nu de MAC zelf.
        wr(&r.tx_mode, TX_MODE_ENABLE | TX_MODE_MBUF_LOCKUP_FIX);
        self.sleep(100 * US);
        wr(&r.rx_mode, RX_MODE_ENABLE | RX_MODE_IPV6_CSUM);
        self.sleep(10 * US);
        wr(&r.mi_stat, MI_STAT_LNKSTAT_ATTN);
        wr(&r.low_wmark, 1); // 57765-klasse: niet droppen bij flow control
    }

    // ── MDIO en de link ─────────────────────────────────────────────────────

    /// Auto-polling uit: zolang de MAC zelf de PHY pollt, botsen onze
    /// MI_COM-transacties met de zijne.
    fn mdio_setup(&self) {
        wr(&self.regs().mi_mode, MI_MODE_BASE);
        self.sleep(40 * US);
    }

    /// Eén MI_COM-transactie, begrensd op 50 ms.
    fn mdio(&self, phy: u8, reg: u8, cmd: u32) -> Result<u16> {
        let r = self.regs();
        let addr =
            u32::from(phy & 0x1f) << MI_COM_PHY_SHIFT | u32::from(reg & 0x1f) << MI_COM_REG_SHIFT;
        wr(&r.mi_com, cmd | MI_COM_BUSY | addr);
        let idle = |n: &Self| n.regs().mi_com.read() & MI_COM_BUSY == 0;
        if self.wait(MDIO_NS, 0, idle) {
            return Ok((r.mi_com.read() & MI_COM_DATA_MASK) as u16);
        }
        Err(Error::Mdio {
            phy,
            reg,
            write: cmd & MI_COM_CMD_WRITE != 0,
        })
    }

    fn mdio_read(&self, phy: u8, reg: u8) -> Result<u16> {
        self.mdio(phy, reg, MI_COM_CMD_READ)
    }

    fn mdio_write(&self, phy: u8, reg: u8, val: u16) -> Result {
        self.mdio(phy, reg, MI_COM_CMD_WRITE | u32::from(val))
            .map(|_| ())
    }

    /// De PHY-identificatie (OUI plus model): de goedkoopste controle dat de
    /// MDIO-bus werkt; 0x0000/0xffff betekent "niemand thuis".
    fn phy_id(&self) -> Result<u32> {
        self.mdio_setup();
        let hi = self.mdio_read(PHY_ADDR, mdio::reg::ID1)?;
        let lo = self.mdio_read(PHY_ADDR, mdio::reg::ID2)?;
        Ok(u32::from(hi) << 16 | u32::from(lo))
    }

    /// Wacht tot de PHY een link meldt, en zet de MAC in de poortmodus die
    /// bij de gemeten snelheid hoort (Go's `LinkUp` plus `SetPortMode`).
    /// Autonegotiatie wordt aangezet als hij uit stond: de kabel kan er net
    /// in zijn gegaan.
    pub fn link_up(&mut self, timeout_ns: u64) -> Result<Link> {
        self.mdio_setup();
        let bmcr = self.mdio_read(PHY_ADDR, mdio::reg::BMCR)?;
        if bmcr & mdio::BMCR_AN_ENABLE == 0 {
            let v = bmcr | mdio::BMCR_AN_ENABLE | mdio::BMCR_AN_RESTART;
            self.mdio_write(PHY_ADDR, mdio::reg::BMCR, v)?;
        }
        let deadline = (self.now)().saturating_add(timeout_ns);
        loop {
            // BMSR twee keer: het link-bit is latching-laag.
            let _ = self.mdio_read(PHY_ADDR, mdio::reg::BMSR)?;
            let bmsr = self.mdio_read(PHY_ADDR, mdio::reg::BMSR)?;
            if bmsr & mdio::BMSR_LINK != 0 {
                let link = decode_aux(self.mdio_read(PHY_ADDR, MII_AUX_STAT)?)?;
                self.set_port_mode(link);
                return Ok(link);
            }
            if (self.now)() >= deadline {
                return Err(Error::NoLink {
                    bmsr,
                    ms: timeout_ns / MS,
                });
            }
            self.sleep(LINK_POLL_NS);
        }
    }

    /// De MAC in de modus die bij de snelheid hoort: GMII voor gigabit, MII
    /// daaronder, half duplex als de PHY dat zegt. De engines blijven aan.
    fn set_port_mode(&self, link: Link) {
        let r = self.regs();
        let mut m = r.mac_mode.read() & !(MAC_MODE_PORT_MASK | MAC_MODE_HALF_DUPLEX);
        m |= if link.mbps == 1000 {
            MAC_MODE_PORT_GMII
        } else {
            MAC_MODE_PORT_MII
        };
        if !link.full_duplex {
            m |= MAC_MODE_HALF_DUPLEX;
        }
        wr(&r.mac_mode, m | MAC_MODE_RUN);
    }

    // ── Diagnose ────────────────────────────────────────────────────────────

    /// De ASIC-revisie. Meestal staat die in MISC_HOST_CTRL, maar de waarde
    /// 0xf betekent daar "kijk in het product-ID-register", en pas dán weet
    /// je met welke familie je te maken hebt. Op de mini komt daar 0x57766
    /// uit, en dat verschil telt: een 57766 is 57765_CLASS maar géén
    /// 5717_PLUS, en dat bepaalt onder meer of de standaard-ring een
    /// SRAM-adres nodig heeft.
    #[must_use]
    pub fn asic_rev(&self) -> u32 {
        let c = self.config();
        let rev = c.misc_host_ctrl.read() >> 16;
        if rev >> 12 == 0xf {
            return c.prodid_asicrev.read() >> 12;
        }
        rev >> 12
    }

    /// MISC_HOST_CTRL (diagnose: staat MASK_PCI_INT nog?).
    #[must_use]
    pub fn misc_host_ctrl(&self) -> u32 {
        self.config().misc_host_ctrl.read()
    }

    /// Eén regel over de chip: ASIC, MAC, PHY, firmware, en de twee
    /// registers waar de drie lessen van 29-08 in staan.
    #[must_use]
    pub fn describe(&self) -> Describe {
        let c = self.config();
        Describe {
            asic: self.asic_rev(),
            mac: self.mac,
            phy_id: self.phy_id,
            fw_mbox: self.fw_mbox,
            misc_host_ctrl: c.misc_host_ctrl.read(),
            pci_cmd: c.command.read() & 0xffff,
        }
    }

    /// Meet in één regel waar de ringen op staan of vallen: doet het
    /// SRAM-venster het (via BAR0 zoals tg3, en via de config space als
    /// tweede mening), staat INDIR_ACCESS aan, staat bus-mastering aan, en
    /// wat zei de bootcode. Het board drukt hem af als er iets hapert, zodat
    /// een dode ring meteen een oorzaak heeft in plaats van een symptoom.
    /// Schrijft een proefwoord in een ongebruikt woord van de send-RCB.
    pub fn self_test(&mut self) -> SelfTest {
        const PROBE: u32 = 0x5a5a_1234;
        let at = SRAM_SEND_RCB + 8;
        self.write_mem(at, PROBE);
        let bar = self.read_mem(at);
        let cfg = (self.cfg.0 != 0).then(|| {
            let c = self.config();
            c.mem_win_base.write(at);
            c.mem_win_data.write(!PROBE);
            c.mem_win_base.write(at);
            let v = c.mem_win_data.read();
            c.mem_win_base.write(0);
            v
        });
        let c = self.config();
        SelfTest {
            off: at,
            bar,
            cfg,
            ok: bar == PROBE,
            misc_host_ctrl: c.misc_host_ctrl.read(),
            pci_cmd: c.command.read() & 0xffff,
            fw_mbox: self.fw_mbox,
        }
    }

    /// De MAC-tellers, rechtstreeks uit hun registers
    /// (`tg3_periodic_fetch_stats`). Ze staan los van DMA: lopen ze op
    /// terwijl het status-blok nul blijft, dan ontvángt de MAC wel degelijk
    /// en strandt het verkeer pas op weg naar het geheugen van de host. Dat
    /// is het verschil tussen een MAC-probleem en een transport-probleem.
    #[must_use]
    pub fn stats(&self) -> Stats {
        let r = self.regs();
        Stats {
            rx_octets: r.rx_octets.read(),
            rx_ucast: r.rx_ucast.read(),
            rx_mcast: r.rx_mcast.read(),
            rx_bcast: r.rx_bcast.read(),
            rx_fcs_err: r.rx_fcs_err.read(),
            tx_octets: r.tx_octets.read(),
            tx_ucast: r.tx_ucast.read(),
            tx_bcast: r.tx_bcast.read(),
        }
    }

    /// De tellers van de list-placement-eenheid, precies waar het misgaat als
    /// de MAC ontvangt en er niets in de ringen belandt. Elke teller wijst
    /// een andere schuldige aan: geen BD beschikbaar, door een filter
    /// gevallen, of een volle werkrij.
    #[must_use]
    pub fn counters(&self) -> Counters {
        let r = self.regs();
        Counters {
            lpc_status: r.rcvlpc_status.read(),
            lpc_nonempty: r.rcvlpc_nonempty.read(),
            drop_filter: r.lpc_drop_filter.read(),
            wq_full: r.lpc_wq_full.read(),
            no_rcv_bd: r.lpc_no_rcv_bd.read(),
            in_discards: r.lpc_in_discards.read(),
            in_errors: r.lpc_in_errors.read(),
            thresh_hit: r.lpc_thresh_hit.read(),
            dbdi_status: r.rcvdbdi_status.read(),
            dbdi_std_con: r.rcvdbdi_std_con.read(),
            bdi_status: r.rcvbdi_status.read(),
            bdi_std_prod: r.rcvbdi_std_prod.read(),
        }
    }

    /// De twee ring-control-blocks terug uit NIC-SRAM: staat er wat we
    /// dachten te schrijven, dan weet de chip waar zijn ringen liggen.
    #[must_use]
    pub fn rcb_dump(&self) -> RcbDump {
        let one = |base: u32| {
            [
                self.read_mem(base),
                self.read_mem(base + 4),
                self.read_mem(base + 8),
                self.read_mem(base + 12),
            ]
        };
        RcbDump {
            send: one(SRAM_SEND_RCB),
            ret: one(SRAM_RCV_RET_RCB),
        }
    }

    /// Het frame-bufferbereik (basis, maat), voor het board dat het gecached
    /// wil mappen.
    #[must_use]
    pub fn buf_region(&self) -> (Pa, u64) {
        (self.dma.add(BUF_OFF), BUF_LEN)
    }

    // ── Interrupts (INTx) ───────────────────────────────────────────────────

    /// Opent de interrupt: MASK_PCI_INT eraf (`enable_reg_access` zet hem,
    /// zoals tg3 bij init, en zonder deze stap trekt de chip nooit zijn lijn)
    /// en de interrupt-mailbox op 0 (`tg3_enable_ints` zonder tagged
    /// status). De chip meldt zich dan bij elke update van het status-blok.
    pub fn irq_unmask(&mut self) {
        let c = self.config();
        wr(
            &c.misc_host_ctrl,
            c.misc_host_ctrl.read() & !MISC_MASK_PCI_INT,
        );
        Self::wr_mbox(&self.regs().mb_interrupt, 0);
    }

    /// Maskeert de interrupt (mailbox 1), zoals `tg3_msi` doet; zie
    /// [`IrqAck::ack`].
    pub fn ack_irq(&self) {
        self.irq_ack().ack();
    }

    /// Het interrupt-pad, voor het board.
    #[must_use]
    pub fn irq_ack(&self) -> IrqAck {
        IrqAck { bar0: self.bar0 }
    }

    /// Opent de mailbox weer, en doet dan wat `tg3_int_reenable` doet: kwam
    /// er werk binnen terwijl de mailbox dicht stond, dan trekt de chip daar
    /// NIET alsnog de lijn voor. Bij bulk merk je dat niet (het volgende
    /// frame werkt het status-blok opnieuw bij), maar een los frame (een
    /// SYN, de ACK van een klein venster) bleef liggen tot de vangrail van
    /// de pomp (10 ms). Dat was de héle 45 MB/s over de draad: venster 480
    /// KB gedeeld door een RTT van 10,5 ms, op elk paar waar de M4 in zat
    /// (L83, 20-09; gepold deed dezelfde draad 100-118 MB/s). Linux: na het
    /// openen kijken of er werk staat en dan HOSTCC_MODE_NOW zetten, zodat
    /// de chip meteen een status-update plus interrupt afgeeft.
    pub fn rearm_irq(&mut self) {
        let r = self.regs();
        Self::wr_mbox(&r.mb_interrupt, 0);
        if self.rx_pending() {
            wr(&r.hostcc_mode, r.hostcc_mode.read() | HOSTCC_MODE_NOW);
        }
    }

    /// Het status-woord van het status-blok (bit 0 = UPDATED), HOSTCC_MODE,
    /// en de PCI-status (bit 3 = INTx# asserted): trekt de chip zijn lijn?
    #[must_use]
    pub fn irq_diag(&self) -> IrqDiag {
        IrqDiag {
            status: self.status_word(STATUS_WORD),
            hostcc: self.regs().hostcc_mode.read(),
            pci_status: self.config().command.read() >> 16,
        }
    }

    /// Hangt de bel aan de driver en opent de interrupt. Aanroepen nadat de
    /// lijn bij de controller scherp staat; zonder blijft de mailbox dicht
    /// en pollt de pomp. Daarna heropent [`flush`](netdev::Device::flush) de
    /// interrupt na elke burst.
    pub fn set_irq(&mut self, bell: &'static Signal) {
        self.irq = Some(bell);
        self.irq_unmask();
    }

    // ── De ringen ───────────────────────────────────────────────────────────

    fn rx_std_desc(&self, i: u32) -> Pa {
        self.dma.add(OFF_RX_STD + u64::from(i) * RX_BD)
    }

    fn rx_ret_desc(&self, i: u32) -> Pa {
        self.dma.add(OFF_RX_RET + u64::from(i) * RX_BD)
    }

    fn tx_desc(&self, i: u32) -> Pa {
        self.dma.add(OFF_TX_BD + u64::from(i) * TX_BD)
    }

    fn rx_buf(&self, i: u32) -> Pa {
        self.dma.add(OFF_RX_BUF + u64::from(i) * BUF_SIZE)
    }

    fn tx_buf(&self, i: u32) -> Pa {
        self.dma.add(OFF_TX_BUF + u64::from(i) * BUF_SIZE)
    }

    /// Een 32-bit woord uit het status-blok.
    fn status_word(&self, at: u64) -> u32 {
        dev::read32(self.dma.add(OFF_STATUS + at))
    }

    /// Tot waar de NIC in de return-ring geschreven heeft
    /// (`idx[0].rx_producer`).
    fn rx_producer(&self) -> u32 {
        self.status_word(STATUS_IDX0) & 0xffff
    }

    /// Hoever de NIC met de send-ring is (`idx[0].tx_consumer`).
    fn tx_consumer(&self) -> u32 {
        self.status_word(STATUS_IDX0) >> 16
    }

    /// Staat er een frame in de return-ring? Een producer buiten de ring is
    /// rommel en telt als leeg: anders loopt `receive` nooit op hem vast.
    fn rx_pending(&self) -> bool {
        let prod = self.rx_producer();
        prod < RX_RET_RING && prod != self.rx_ret_idx
    }

    /// Leest de return-descriptor op de kop en kopieert zijn frame, zonder
    /// FCS, naar `buf`. `None` voor een fout, een index buiten de ring, of
    /// een lengte die niet klopt of niet in `buf` past: dat is invoer van
    /// het device, en die wordt begrensd, niet vertrouwd.
    fn take_rx(&self, buf: &mut [u8]) -> Option<usize> {
        let d = self.rx_ret_desc(self.rx_ret_idx);
        let idx_len = dev::read32(d.add(off(offset_of!(RxBd, idx_len))));
        let flags = dev::read32(d.add(off(offset_of!(RxBd, type_flags)))) & 0xffff;
        let err_vlan = dev::read32(d.add(off(offset_of!(RxBd, err_vlan))));
        let opaque = dev::read32(d.add(off(offset_of!(RxBd, opaque))));
        if flags & RXD_FLAG_ERROR != 0 || err_vlan & RXD_ERR_MASK != 0 {
            return None;
        }
        // De lengte in de descriptor telt de FCS mee; die hoort niet bij het
        // frame. Vier bytes te veel doorgeven betekent vier bytes rommel
        // achter élk pakket dat de stack krijgt (29-08).
        let len = (idx_len & 0xffff).checked_sub(ETH_FCS_LEN)?;
        let index = opaque & 0xffff;
        let fits = (ETH_HLEN..=RX_DMA_SIZE - ETH_FCS_LEN).contains(&len);
        if index >= RX_STD_RING || !fits {
            return None;
        }
        let dst = buf.get_mut(..len as usize)?;
        let src = self.rx_buf(index);
        dev::pull(src, dst.len());
        dev::copy_out(dst, src);
        Some(dst.len())
    }

    /// Geeft de return-descriptor en één std-buffer terug. De chip verbruikt
    /// de producer-ring op volgorde en elke descriptor wijst altijd naar zijn
    /// eigen buffer, dus één plek verder aanbieden is precies de buffer die
    /// net vrijkwam (of een die nog nooit uit stond). De mailboxen gaan pas
    /// bij de flush: twee PCIe-writes per burst in plaats van per frame.
    fn recycle_rx(&mut self) {
        // Eerst de kopie afmaken; pas daarna mag DMA deze buffer opnieuw
        // vullen.
        dev::mb();
        self.rx_ret_idx = (self.rx_ret_idx + 1) % RX_RET_RING;
        self.rx_std_idx = (self.rx_std_idx + 1) % RX_STD_RING;
        self.rx_dirty = true;
        self.rx_since += 1;
        if self.rx_since >= RX_SELF_FLUSH {
            self.flush_rx();
        }
    }

    /// Meldt de NIC hoever we met de return-ring zijn en hoeveel std-buffers
    /// weer vrij zijn.
    fn flush_rx(&mut self) {
        self.rx_since = 0;
        if self.rx_dirty {
            let r = self.regs();
            wr(&r.mb_rx_ret_cons, self.rx_ret_idx);
            wr(&r.mb_rx_std_prod, self.rx_std_idx);
            self.rx_dirty = false;
        }
    }

    /// De send-producer-mailbox (de doorbell) voor alles wat sinds de vorige
    /// flush klaargezet is.
    fn flush_tx(&mut self) {
        if self.tx_dirty {
            dev::mb();
            wr(&self.regs().mb_tx_prod, self.tx_prod);
            self.tx_dirty = false;
            self.doorbells = self.doorbells.wrapping_add(1);
        }
    }
}

/// Een offset binnen een descriptor als `u64`, voor `Pa::add`.
const fn off(o: usize) -> u64 {
    o as u64
}

/// Snelheid en duplex uit Broadcom's auxiliary status (reg 0x19), bits
/// 10:8: dezelfde tabel als `tg3_setup_copper_phy`.
fn decode_aux(aux: u16) -> Result<Link> {
    let (mbps, full_duplex) = match (aux >> 8) & 0x7 {
        1 => (10, false),
        2 => (10, true),
        3 => (100, false),
        5 => (100, true),
        6 => (1000, false),
        7 => (1000, true),
        _ => return Err(Error::LinkState { aux }),
    };
    Ok(Link { mbps, full_duplex })
}

impl Mdio for Tg3 {
    /// Clause 22 via MI_COM.
    fn read(&mut self, phy: u8, reg: u8) -> mdio::Result<u16> {
        self.mdio_read(phy, reg)
            .map_err(|_| mdio::Error::Bus { phy, reg })
    }

    fn write(&mut self, phy: u8, reg: u8, val: u16) -> mdio::Result {
        self.mdio_write(phy, reg, val)
            .map_err(|_| mdio::Error::Bus { phy, reg })
    }
}

impl netdev::Device for Tg3 {
    /// Zet één frame op de send-ring; de doorbell volgt bij `flush`. Een
    /// volle ring krijgt eerst de doorbell voor wat al klaarstaat (anders
    /// raakt hij nooit leeg) en dan [`TxError::Full`]; Go wachtte hier tot
    /// 100 ms, de pomp beslist nu zelf.
    fn transmit(&mut self, frame: &[u8]) -> core::result::Result<(), TxError> {
        if frame.is_empty() || frame.len() as u64 > BUF_SIZE {
            return Err(TxError::Size(frame.len()));
        }
        let next = (self.tx_prod + 1) % TX_RING;
        if next == self.tx_consumer() {
            self.flush_tx();
            self.tx_full = self.tx_full.wrapping_add(1);
            return Err(TxError::Full);
        }
        let buf = self.tx_buf(self.tx_prod);
        dev::copy_in(buf, frame);
        dev::push(buf, frame.len()); // gecachte buffer: naar het geheugen vóór de descriptor
        let d = self.tx_desc(self.tx_prod);
        dev::write32(d.add(off(offset_of!(TxBd, addr_hi))), (buf.0 >> 32) as u32);
        dev::write32(d.add(off(offset_of!(TxBd, addr_lo))), buf.0 as u32);
        dev::write32(
            d.add(off(offset_of!(TxBd, len_flags))),
            (frame.len() as u32) << 16 | TXD_FLAG_END,
        );
        dev::write32(d.add(off(offset_of!(TxBd, vlan_tag))), 0);
        dev::mb();
        self.tx_prod = next;
        self.tx_dirty = true;
        Ok(())
    }

    /// Haalt één frame op. Een fout of een kromme lengte wordt teruggegeven
    /// aan de chip zonder kopie en geteld ([`Tg3::rx_bad`]); daarna kijkt
    /// hij naar de volgende. Hoogstens één ronde over de ring per aanroep.
    fn receive(&mut self, buf: &mut [u8]) -> Option<usize> {
        for _ in 0..RX_RET_RING {
            if !self.rx_pending() {
                return None;
            }
            dev::mb(); // de completion zien vóór zijn descriptor en pakket
            let got = self.take_rx(buf);
            self.recycle_rx();
            if got.is_some() {
                return got;
            }
            self.rx_bad = self.rx_bad.wrapping_add(1);
        }
        None
    }

    /// De uitgestelde mailboxen (return-consumer, std-producer, en de
    /// send-doorbell), en met een bedrade lijn de heropening van de
    /// interrupt ([`Tg3::rearm_irq`]).
    fn flush(&mut self) {
        self.flush_rx();
        self.flush_tx();
        if self.irq.is_some() {
            self.rearm_irq();
        }
    }

    fn mac(&self) -> Mac {
        self.mac
    }

    fn irq(&self) -> Option<&'static Signal> {
        self.irq
    }
}

#[cfg(test)]
mod tests;
