//! Synopsys DesignWare MAC van de 4.x/5.x-generatie (DWMAC4/EQOS): de
//! descriptorringen, de DMA, de MTL-laag en de MDIO-master.
//!
//! Op de Radxa Zero 3E is dit het GMAC1-blok van de RK3566, dat zich meldt
//! als VERSION 0x3051 (snpsver 5.10; gemeten 05-08, en de DTS noemt hem
//! "snps,dwmac-4.20a").
//!
//! Waarom een eigen crate naast een DWMAC1000-driver: die generatie (3.x)
//! deelt met deze alleen de naam en de leverancier. Hier zit de MDIO op
//! 0x200/0x204 in plaats van 0x10/0x14, het MAC-adres op 0x300, de DMA per
//! kanaal op 0x1100 + n × 0x80 met een MTL-laag ertussen, en de descriptors
//! hebben vier woorden met een ander bitformaat, inclusief een tail-pointer
//! in plaats van een poll-demand-register.
//!
//! REFERENTIE (opgehaald 05-08, nagerekend): Linux v6.13
//! `drivers/net/ethernet/stmicro/stmmac`: `dwmac4.h` en `dwmac4_dma.h`
//! (registers), `dwmac4_descs.h` (descriptorbits), `dwmac4_core.c`
//! (MDIO-velden, `GMAC_CORE_INIT`, de snelheidsbits), `dwmac4_dma.c` en
//! `dwmac4_lib.c` (de init-volgorde, MTL, ringlengte en tail-pointer) en
//! `stmmac_main.c` voor de ringsemantiek.
//!
//! De SoC-glue zit NIET hier maar in het board (`board-rk3566`): pinmux,
//! GRF (RGMII-modus en delays), klokgates, de snelheidsdeler en de
//! PHY-reset. Deze crate is de IP-core; de clause-22-PHY-logica komt uit
//! `driver-mdio`, via [`Mdio`] op [`Probe`].
//!
//! De levensloop is een type (handboek §1.2): een [`Probe`] is het blok
//! vóór de ringen (versie, reset, MDIO), [`Probe::start`] maakt er een
//! [`Dwmac4`] van, en alleen die draagt frames.
//!
//! Batching: [`transmit`](netdev::Device::transmit) en de RX-teruggave
//! zetten descriptors klaar; de tail-pointers (de doorbell van deze
//! generatie) vallen pas in [`flush`](netdev::Device::flush), één keer per
//! burst.
//!
//! CACHE-COHERENTIE: de DMA-regio ligt buiten de kern-RAM en is ongecachet
//! gemapt (Normal-NC op de RK3566); de descriptors staan daarom gewoon 16
//! bytes uit elkaar en er is geen cache-onderhoud. Normal-NC is wel zwakker
//! geordend dan Device: de CPU mag loads herordenen, vandaar de `mb` tussen
//! de OWN-lees en de bufferlees (Linux: `dma_rmb()` op dezelfde plek).

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

mod regs;
#[cfg(test)]
mod tests;

use core::fmt;
use dev::Pa;
use driver_mdio::Mdio;
use netdev::{Mac, TxError};
use regs::{Chan, Regs};
use sync::Signal;

pub use regs::CSR_100_150M;

// GMAC_CONFIG-bits (dwmac4.h).
/// Jabber disable.
const CFG_JD: u32 = 1 << 17;
/// Jumbo enable: bewust UIT, zie [`CORE_INIT`].
const CFG_JE: u32 = 1 << 16;
/// Port select: MII (10/100) in plaats van GMII (1000).
const CFG_PS: u32 = 1 << 15;
/// Fast ethernet speed: 100 in plaats van 10.
const CFG_FES: u32 = 1 << 14;
/// Duplex mode: full.
const CFG_DM: u32 = 1 << 13;
/// Carrier sense negeren tijdens TX (half duplex).
const CFG_DCRS: u32 = 1 << 9;
/// Packet burst enable (half duplex).
const CFG_BE: u32 = 1 << 18;
/// Automatic pad/CRC strip: bewust UIT, [`rx_len`] trekt de FCS zelf af.
#[cfg(test)]
const CFG_ACS: u32 = 1 << 20;
/// Transmitter aan.
const CFG_TE: u32 = 1 << 1;
/// Receiver aan.
const CFG_RE: u32 = 1 << 0;

/// `GMAC_CORE_INIT` uit dwmac4.h is JD|PS|BE|DCRS|JE. Wij laten JE (jumbo)
/// eruit, en dat is een BEWUSTE afwijking: onze RX-buffers zijn
/// [`BUF_SIZE`] groot, en met jumbo aan zou de MAC frames tot 9018 bytes
/// aannemen die over meerdere descriptors binnenkomen. `receive` verwerkt
/// geen gesplitste frames (hij eist FD|LD in één descriptor), dus zou zo'n
/// frame een stille verliespost zijn in plaats van een afgekeurd frame. PS
/// hoort bij de snelheid en wordt in [`mac_config`] gezet.
const CORE_INIT: u32 = CFG_JD | CFG_BE | CFG_DCRS;
/// De snelheidsbits (dwmac4_core.c `mac->link.*`): 10 = PS, 100 = PS|FES,
/// 1000 = geen van beide.
const SPEED_MASK: u32 = CFG_PS | CFG_FES;

/// GMAC_RXQ_CTRL0: RX-queue 0 aan in DCB-modus
/// (`GMAC_RX_DCB_QUEUE_ENABLE(0)`); zonder dit komt er geen frame uit de
/// MTL.
const RXQ0_DCB_ENABLE: u32 = 1 << 1;

// MTL (dwmac4.h): één queue, dus alleen kanaal 0.
/// TX store-and-forward.
const MTL_TSF: u32 = 1 << 1;
/// TXQEN (niet-AVB).
const MTL_TXQ_EN: u32 = 1 << 3;
/// RX store-and-forward.
const MTL_RSF: u32 = 1 << 5;
/// TQS in [24:16]: de TX-queue-maat in eenheden van 256 B, min één.
const MTL_TQS_SHIFT: u32 = 16;
/// RQS in [29:20].
const MTL_RQS_SHIFT: u32 = 20;

/// DMA_BUS_MODE: de softreset.
const BUS_SOFT_RESET: u32 = 1 << 0;

// DMA_SYS_BUS_MODE is óók het AXI-configregister. De waarden komen ÉÉN OP
// ÉÉN uit de DTS van dit silicium (rk356x-base.dtsi,
// gmac1_stmmac_axi_setup): snps,mixed-burst = MB; snps,blen = <0 0 0 0 16 8
// 4> = BLEN16|BLEN8|BLEN4; snps,rd_osr_lmt = <8>; snps,wr_osr_lmt = <4>.
// Wat er NIET staat is even belangrijk: geen snps,fixed-burst en geen
// snps,aal. FB zetten zou hier schadelijk zijn: "mixed burst has no effect
// when fb is set", dus een welgemeende extra bit zou de instelling die de
// vendor wél vraagt uitschakelen.
const SYS_BUS_MB: u32 = 1 << 14;
const SYS_BUS_BLEN16: u32 = 1 << 3;
const SYS_BUS_BLEN8: u32 = 1 << 2;
const SYS_BUS_BLEN4: u32 = 1 << 1;
const SYS_BUS_RD_OSR: u32 = 8 << 16;
const SYS_BUS_WR_OSR: u32 = 4 << 24;
/// De OSR-velden: na reset niet noodzakelijk nul, en er blind bij OR-en
/// geeft een groter getal dan de vendor toestaat.
const SYS_BUS_OSR_MASK: u32 = (0xF << 16) | (0xF << 24);
const SYS_BUS_MODE: u32 =
    SYS_BUS_MB | SYS_BUS_BLEN16 | SYS_BUS_BLEN8 | SYS_BUS_BLEN4 | SYS_BUS_RD_OSR | SYS_BUS_WR_OSR;

/// DMA_CHAN_CONTROL: PBL maal 8.
const CHAN_PBL_X8: u32 = 1 << 16;
/// TX: operate on second packet.
const CHAN_OSP: u32 = 1 << 4;
/// ST (TX) of SR (RX): zelfde bit, ander register.
const CHAN_START: u32 = 1 << 0;
/// De burstlengte; met PBLx8 effectief 64 beats.
const PBL: u32 = 8;
const TX_PBL_SHIFT: u32 = 16;
const RX_PBL_SHIFT: u32 = 16;
/// RBSZ is het veld [14:1]: de maat staat er maal twee in.
const RX_RBSZ_SHIFT: u32 = 1;
const RX_RBSZ_MASK: u32 = 0x7FFE;

// De RX-interrupt van kanaal 0. Twee bit-indelingen bestaan er voor het
// enable-register en dát is de valkuil (dwmac4_dma.h): tot core 4.00 is NIE
// bit 16, vanaf 4.10 bit 15 (`DMA_CHAN_INTR_ENA_NIE_4_10`). Dit silicium is
// een 4.20a en dus de 4.10-indeling. GEMETEN 20-09 op de Radxa: met bit 16
// las het register 0x40 terug (alleen RIE bleef staan), stond de lijn nooit
// hoog en claimde de node 0 interrupts per seconde terwijl er 200 MB
// binnenkwam. Status is W1C met dezelfde nummering in beide indelingen.
/// Normal interrupt summary enable (4.10+).
const INTR_NIE: u32 = 1 << 15;
/// Receive interrupt enable.
const INTR_RIE: u32 = 1 << 6;
/// Status: receive interrupt.
const STAT_RI: u32 = 1 << 6;
/// Status: normal interrupt summary.
const STAT_NIS: u32 = 1 << 15;

/// Eén descriptor: vier woorden van 32 bits; ook de stap in de ring.
pub const DESC_SIZE: u64 = 16;

// TX (dwmac4_descs.h): TDES0/1 = bufferadres, TDES2 = bufferlengte, TDES3 =
// pakket.
const TX_BUF_LEN_MASK: u32 = 0x3FFF;
const TX_PKT_LEN_MASK: u32 = 0x7FFF;
const TX_LAST: u32 = 1 << 28;
const TX_FIRST: u32 = 1 << 29;
const TX_OWN: u32 = 1 << 31;

// RX: RDES0/1 = bufferadres, RDES3 = eigendom en status of lengte.
const RX_PKT_LEN_MASK: u32 = 0x7FFF;
const RX_ERR_SUMMARY: u32 = 1 << 15;
const RX_LAST: u32 = 1 << 28;
const RX_FIRST: u32 = 1 << 29;
/// Bij teruggeven: buffer 1 is geldig.
const RX_BUF1_VALID: u32 = 1 << 24;
/// Zonder dit bit vult de DMA de descriptor af zonder RI te zetten: de lijn
/// blijft dan stil terwijl het verkeer doorloopt (Radxa 20-09: 200 MB
/// binnen, 0 claims, status alleen ERI). Linux zet hem bij élke teruggave
/// (`dwmac4_set_rx_owner`).
const RX_IOC: u32 = 1 << 30;
const RX_OWN: u32 = 1 << 31;

/// De MAC meldt de lengte MÉT CRC (ACS staat uit), zie [`rx_len`].
const FCS_LEN: usize = 4;

/// RX-descriptors. Een gepolde ontvanger (300 µs) heeft de ring als buffer
/// tussen twee pomprondes, en 64 × 1,5 KB is 0,8 ms op 1 Gbit. De igb ging
/// van 5 naar 36 MB/s met 64 naar 256 (19-09); de Radxa zat op 15,9 MB/s
/// inbound tegen 118 op de borden met een grote ring (20-09).
pub const NUM_RX: u16 = 256;
/// TX-descriptors.
pub const NUM_TX: u16 = 64;
/// Eén buffer: een veelvoud van 16, ruim boven de 1518 van een
/// MTU-1500-frame.
pub const BUF_SIZE: usize = 1536;
/// Het grootste frame dat wij versturen; de TX-lengtevelden zijn hier ruim
/// (14/15 bits), dus dit is een MTU-grens en geen veldgrens.
pub const MAX_FRAME: usize = 1518;

const DESC_BYTES: u64 = (NUM_RX as u64 + NUM_TX as u64) * DESC_SIZE;
const BUF_BYTES: u64 = (NUM_RX as u64 + NUM_TX as u64) * BUF_SIZE as u64;

/// De DMA-regio die deze driver nodig heeft; het board reserveert dit in
/// zijn plan. 256 + 64 descriptors met buffers is 0,5 MB van de 8 MB
/// net-DMA van de RK3566.
///
/// GEPROBEERD EN TERUGGEDRAAID 21-09: de buffers op eigen 2 MB-blokken
/// zodat het board de zendkant gecached kan mappen, zoals de GEM op de Pi 5.
/// Daar wint het; hier niet: met Normal-NC op de hele regio doet dit
/// silicium al 56 MB/s in en 99 uit, en gecached zenden veranderde daar
/// niets aan.
pub const NEED_BYTES: u64 = DESC_BYTES + BUF_BYTES;

const _: () = {
    assert!(BUF_SIZE >= MAX_FRAME + FCS_LEN);
    assert!(BUF_SIZE.is_multiple_of(16));
    // RBSZ [14:1] moet de maat dragen: de vorige generatie kreeg een
    // buffergrootte die door een te smal veld naar nul werd geveegd (30-07).
    assert!(((BUF_SIZE as u32) << RX_RBSZ_SHIFT) & !RX_RBSZ_MASK == 0);
    assert!(NUM_RX.is_power_of_two() && NUM_TX.is_power_of_two());
};

/// Waarom de driver weigert.
#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    /// VERSION leest 0 of alles-één: een blok zonder klok of een dode bus.
    NoMac {
        /// De gelezen waarde.
        version: u32,
    },
    /// De DMA-softreset klaart niet: de AXI-kant staat nog in reset (05-08:
    /// `bus mode 0x00000001`).
    ResetStuck {
        /// DMA_BUS_MODE na de grens.
        bus_mode: u32,
    },
    /// De DMA-regio is te klein.
    DmaTooSmall {
        /// Wat nodig is.
        need: u64,
        /// Wat er is.
        have: u64,
    },
    /// De DMA-regio ligt niet op 16 bytes of boven 4 GB.
    DmaPlace {
        /// Het begin.
        base: u64,
    },
    /// HW_FEATURE1 geeft onmogelijke FIFO-maten: een blok zonder klok.
    Fifo {
        /// De TX-FIFO in bytes.
        tx: u32,
        /// De RX-FIFO in bytes.
        rx: u32,
        /// Het rauwe register.
        hw_feature1: u32,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::NoMac { version } => write!(f, "dwmac4: no MAC (version {version:#010x})"),
            Self::ResetStuck { bus_mode } => write!(
                f,
                "dwmac4: DMA soft reset does not clear (bus mode {bus_mode:#010x})"
            ),
            Self::DmaTooSmall { need, have } => write!(
                f,
                "dwmac4: DMA region {} KB, need at least {} KB",
                have >> 10,
                need >> 10
            ),
            Self::DmaPlace { base } => {
                write!(
                    f,
                    "dwmac4: DMA region at {base:#x} is unaligned or above 4 GB"
                )
            }
            Self::Fifo {
                tx,
                rx,
                hw_feature1,
            } => write!(
                f,
                "dwmac4: implausible FIFO sizes tx={tx}B rx={rx}B (hw-feature1 {hw_feature1:#010x}), is the block clocked?"
            ),
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Monotone nanoseconden; de klok van het board.
pub type Clock = fn() -> u64;

/// De MDIO-grens: een PHY die niet reageert mag de boot niet ophouden.
const MDIO_TIMEOUT_NS: u64 = 100_000_000;
/// De grens van de DMA-softreset.
const RESET_TIMEOUT_NS: u64 = 200_000_000;

// --- de rekenstukken, apart zodat ze op de host bewijsbaar zijn -----------
//
// Dit is de bit-arithmetiek van de driver, en daar struikelde de vorige
// generatie op ijzer (30-07: een buffergrootte die door een te smal veld
// naar nul werd geveegd). Op dit bordje kost een ronde een kaartwissel.

/// Het GMAC_CONFIG-woord: de bestaande inhoud met de snelheids-, duplex- en
/// jumbobits gewist, dan de core-init-bits en de snelheid erin. Idempotent.
#[must_use]
pub fn mac_config(cur: u32, mbps: u32, full_duplex: bool) -> u32 {
    let mut cfg = (cur & !(SPEED_MASK | CFG_DM | CFG_JE)) | CORE_INIT;
    match mbps {
        10 => cfg |= CFG_PS,
        100 => cfg |= CFG_PS | CFG_FES,
        _ => {} // 1000: PS en FES uit, GMII in plaats van MII.
    }
    if full_duplex {
        cfg |= CFG_DM;
    }
    cfg
}

/// Het RX_CONTROL-woord: RBSZ gewist, dan de burstlengte en de
/// buffergrootte erin. `None` als de maat niet in het veld [14:1] past: een
/// maat die er stil afgekapt in landt, levert een MAC die halve frames
/// aflevert.
#[must_use]
pub fn rx_control(cur: u32, size: usize) -> Option<u32> {
    let s = u32::try_from(size).ok()?;
    if s == 0 || s & 1 != 0 || (s << RX_RBSZ_SHIFT) & !RX_RBSZ_MASK != 0 {
        return None;
    }
    Some((cur & !RX_RBSZ_MASK) | (PBL << RX_PBL_SHIFT) | (s << RX_RBSZ_SHIFT))
}

/// TDES2 en TDES3 voor één frame: de bufferlengte in [13:0], en het pakket
/// (eerste én laatste descriptor, lengte in [14:0], OWN). Twee velden,
/// dezelfde lengte: wie er één vergeet, krijgt een MAC die de verkeerde
/// hoeveelheid bytes verstuurt. `None` buiten `1..=MAX_FRAME`.
#[must_use]
pub fn tx_desc23(len: usize) -> Option<(u32, u32)> {
    if len == 0 || len > MAX_FRAME {
        return None;
    }
    let l = len as u32;
    Some((
        l & TX_BUF_LEN_MASK,
        TX_OWN | TX_FIRST | TX_LAST | (l & TX_PKT_LEN_MASK),
    ))
}

/// De framelengte uit RDES3, zonder de vier FCS-bytes (ACS staat uit, zoals
/// bij stmmac voor deze generatie). Vergeet je dat, dan komt elk frame vier
/// bytes te lang de stack in en faalt elke checksum.
#[must_use]
pub fn rx_len(rdes3: u32) -> usize {
    ((rdes3 & RX_PKT_LEN_MASK) as usize).saturating_sub(FCS_LEN)
}

/// Het MDIO-commandowoord (dwmac4_core.c `dwmac4_setup`: addr_shift 21,
/// reg_shift 16, clk_csr_shift 8; stmmac_mdio.c: read = 3 << 2, write =
/// 1 << 2, busy = bit 0).
#[must_use]
pub fn mdio_cmd(phy: u8, reg: u8, csr: u32, write: bool) -> u32 {
    let op = if write {
        regs::MDIO_WRITE
    } else {
        regs::MDIO_READ
    };
    ((u32::from(phy) & 0x1F) << regs::MDIO_ADDR_SHIFT)
        | ((u32::from(reg) & 0x1F) << regs::MDIO_REG_SHIFT)
        | ((csr & 0xF) << regs::MDIO_CSR_SHIFT)
        | op
        | regs::MDIO_BUSY
}

/// De FIFO-maten uit HW_FEATURE1 (TXFIFOSIZE [10:6], RXFIFOSIZE [4:0], elk
/// 128 << n). Uit de hardware, niet uit een constante: TQS en RQS moeten
/// erop kloppen, en een verkeerde TQS is een MAC die frames in de FIFO laat
/// staan.
#[must_use]
pub fn fifo_sizes(hw_feature1: u32) -> (u32, u32) {
    let tx = (hw_feature1 >> 6) & 0x1F;
    let rx = hw_feature1 & 0x1F;
    (
        128u32.checked_shl(tx).unwrap_or(0),
        128u32.checked_shl(rx).unwrap_or(0),
    )
}

/// Het blok vóór de ringen: versie, reset en de MDIO-master. Het board
/// doet hiermee de PHY-scan en de autonegotiatie, en maakt er dan met
/// [`Probe::start`] een draaiende [`Dwmac4`] van.
pub struct Probe {
    base: Pa,
    csr: u32,
    now: Clock,
}

impl Probe {
    /// Het blok op `base`. Raakt nog niets aan.
    ///
    /// # Safety
    ///
    /// `base` is het gemapte (Device) registerblok van een DWMAC4 van
    /// minstens 0x1164 bytes, dat blijft zolang het programma draait. Zijn
    /// klokken (pclk voor de registers) staan open: een Rockchip-blok
    /// zonder klok geeft geen abort maar houdt de bus vast.
    #[must_use]
    pub unsafe fn new(base: Pa, csr: u32, now: Clock) -> Self {
        Self { base, csr, now }
    }

    fn regs(&self) -> &'static Regs {
        // SAFETY: de voorwaarde van `new`.
        unsafe { dev::regs(self.base) }
    }

    /// Het rauwe VERSION-register: snpsver in [7:0]. Het eerste dat een
    /// bring-up hoort te lezen.
    #[must_use]
    pub fn version(&self) -> u32 {
        self.regs().version.read()
    }

    /// Leeft de MAC? 0 of alles-één is een dode bus.
    pub fn check(&self) -> Result<u32> {
        match self.version() {
            v @ (0 | 0xFFFF_FFFF) => Err(Error::NoMac { version: v }),
            v => Ok(v),
        }
    }

    /// Het rauwe MDIO_ADDR-register, voor het meetinstrument: blijft BUSY
    /// staan, dan wacht de machine op een klok of een bus, en dat is een
    /// ander probleem dan "er zit geen PHY".
    #[must_use]
    pub fn mdio_state(&self) -> u32 {
        self.regs().mdio_addr.read()
    }

    /// De DMA-softreset. Apart van `start`: de reset zet ook de
    /// MDIO-machine schoon, en het board doet hem vóór de PHY-scan.
    pub fn reset(&mut self) -> Result {
        let r = self.regs();
        r.dma_bus_mode.update(|v| v | BUS_SOFT_RESET);
        let deadline = (self.now)().saturating_add(RESET_TIMEOUT_NS);
        loop {
            if r.dma_bus_mode.read() & BUS_SOFT_RESET == 0 {
                return Ok(());
            }
            if (self.now)() >= deadline {
                return Err(Error::ResetStuck {
                    bus_mode: r.dma_bus_mode.read(),
                });
            }
            core::hint::spin_loop();
        }
    }

    /// Wacht, begrensd, tot de MDIO-machine vrij is.
    fn mdio_wait(&self) -> bool {
        let r = self.regs();
        let deadline = (self.now)().saturating_add(MDIO_TIMEOUT_NS);
        loop {
            if r.mdio_addr.read() & regs::MDIO_BUSY == 0 {
                return true;
            }
            if (self.now)() >= deadline {
                return false;
            }
            core::hint::spin_loop();
        }
    }

    /// Legt de ringen in de DMA-regio en zet MAC, MTL en DMA aan: na de
    /// board-glue, na de reset en na de autonegotiatie, want `mbps` en
    /// `full_duplex` komen daaruit (het board zet de RGMII-klokdeler op
    /// dezelfde snelheid).
    ///
    /// # Safety
    ///
    /// `[dma, dma + dma_size)` is gemapt, ongecachet geheugen onder 4 GB
    /// dat alleen deze driver en het device gebruiken, nu en zolang het
    /// programma draait.
    pub unsafe fn start(
        mut self,
        dma: Pa,
        dma_size: u64,
        mac: Mac,
        mbps: u32,
        full_duplex: bool,
    ) -> Result<Dwmac4> {
        if dma_size < NEED_BYTES {
            return Err(Error::DmaTooSmall {
                need: NEED_BYTES,
                have: dma_size,
            });
        }
        if !dma.is_aligned(DESC_SIZE) || dma.0.saturating_add(NEED_BYTES) > 1 << 32 {
            return Err(Error::DmaPlace { base: dma.0 });
        }
        self.reset()?;
        let mut n = Dwmac4 {
            base: self.base,
            csr: self.csr,
            now: self.now,
            mac,
            ring: Rings::at(dma),
            rx_cur: 0,
            tx_cur: 0,
            rx_dirty: false,
            tx_dirty: false,
            irq: None,
            stats: Stats::default(),
        };
        n.program(mbps, full_duplex)?;
        Ok(n)
    }
}

impl Mdio for Probe {
    fn read(&mut self, phy: u8, reg: u8) -> driver_mdio::Result<u16> {
        mdio_read(self, phy, reg)
    }
    fn write(&mut self, phy: u8, reg: u8, val: u16) -> driver_mdio::Result {
        mdio_write(self, phy, reg, val)
    }
}

/// Eén clause-22-lees over de MDIO-master van `p`.
fn mdio_read(p: &Probe, phy: u8, reg: u8) -> driver_mdio::Result<u16> {
    let bus = driver_mdio::Error::Bus { phy, reg };
    if !p.mdio_wait() {
        return Err(bus);
    }
    p.regs().mdio_addr.write(mdio_cmd(phy, reg, p.csr, false));
    if !p.mdio_wait() {
        return Err(bus);
    }
    Ok((p.regs().mdio_data.read() & 0xFFFF) as u16)
}

/// Eén clause-22-schrijf over de MDIO-master van `p`.
fn mdio_write(p: &Probe, phy: u8, reg: u8, val: u16) -> driver_mdio::Result {
    let bus = driver_mdio::Error::Bus { phy, reg };
    if !p.mdio_wait() {
        return Err(bus);
    }
    p.regs().mdio_data.write(u32::from(val));
    p.regs().mdio_addr.write(mdio_cmd(phy, reg, p.csr, true));
    if p.mdio_wait() { Ok(()) } else { Err(bus) }
}

/// Waar de ringen en buffers liggen: rx-desc, tx-desc, rx-buf, tx-buf, op
/// een rij in de DMA-regio.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
struct Rings {
    rx_desc: Pa,
    tx_desc: Pa,
    rx_buf: Pa,
    tx_buf: Pa,
}

impl Rings {
    const fn at(dma: Pa) -> Self {
        Self {
            rx_desc: dma,
            tx_desc: dma.add(NUM_RX as u64 * DESC_SIZE),
            rx_buf: dma.add(DESC_BYTES),
            tx_buf: dma.add(DESC_BYTES + NUM_RX as u64 * BUF_SIZE as u64),
        }
    }

    fn rx(&self, i: u16) -> Pa {
        self.rx_desc.add(u64::from(i) * DESC_SIZE)
    }

    fn tx(&self, i: u16) -> Pa {
        self.tx_desc.add(u64::from(i) * DESC_SIZE)
    }

    fn rx_buf(&self, i: u16) -> Pa {
        self.rx_buf.add(u64::from(i) * BUF_SIZE as u64)
    }

    fn tx_buf(&self, i: u16) -> Pa {
        self.tx_buf.add(u64::from(i) * BUF_SIZE as u64)
    }
}

/// De laagste 32 bits van een DMA-adres (de regio ligt onder 4 GB, getoetst
/// in `start`).
fn lo(pa: Pa) -> u32 {
    (pa.0 & 0xFFFF_FFFF) as u32
}

/// De hoogste 32 bits.
fn hi(pa: Pa) -> u32 {
    (pa.0 >> 32) as u32
}

/// De meetlat van de driver.
#[derive(Copy, Clone, Default, Debug, PartialEq, Eq)]
pub struct Stats {
    /// Afgeleverde frames.
    pub rx_frames: u64,
    /// Afgekeurde descriptors (foutframe of gesplitst frame).
    pub rx_errors: u64,
    /// Te lange meldingen (lengte voorbij de buffer).
    pub rx_oversize: u64,
    /// De rauwe RDES3 van het laatst afgekeurde frame: zonder dat is
    /// "afgekeurd" op ijzer niet te ontleden.
    pub rx_last_err: u32,
    /// Verzonden frames (op de ring gezet).
    pub tx_frames: u64,
    /// Keren dat de TX-ring vol zat.
    pub tx_full: u64,
    /// Tail-pointer-schrijfacties: de doorbells.
    pub doorbells: u64,
    /// Keren dat de RX-interrupt weer open ging.
    pub rearms: u64,
}

/// Het interrupt-pad van de NIC: alleen het enable- en statusregister van
/// kanaal 0, die niets met de ringen delen. `Copy`, zodat het board hem
/// naast de driver houdt.
#[derive(Clone, Copy)]
pub struct IrqAck {
    base: Pa,
}

impl IrqAck {
    /// Laat de level-lijn los: masker dicht, status gewist (het
    /// rtl8126-ritme). De driver zet het masker weer open als de pomp de
    /// ring leeg las (`receive` die `None` geeft), dus één claim per burst
    /// in plaats van per frame. Geeft de kanaalstatus die stond.
    pub fn ack(&self) -> u32 {
        // SAFETY: `base` kwam uit een `Probe`, die een gemapt blok eiste;
        // het enable- en statusregister delen niets met de ringen.
        let c: &Chan = unsafe { &dev::regs::<Regs>(self.base).chan };
        let st = c.status.read();
        c.intr_ena.write(0);
        c.status.write(STAT_RI | STAT_NIS);
        st
    }
}

/// Eén draaiende DWMAC4.
pub struct Dwmac4 {
    base: Pa,
    csr: u32,
    now: Clock,
    mac: Mac,
    ring: Rings,
    /// De volgende RX-descriptor die wij lezen.
    rx_cur: u16,
    /// De volgende TX-descriptor die wij vullen.
    tx_cur: u16,
    /// Er zijn RX-descriptors teruggegeven sinds de laatste tail-schrijf.
    rx_dirty: bool,
    /// Er zijn TX-descriptors gevuld sinds de laatste tail-schrijf.
    tx_dirty: bool,
    irq: Option<&'static Signal>,
    /// De meetlat.
    pub stats: Stats,
}

impl Dwmac4 {
    fn regs(&self) -> &'static Regs {
        // SAFETY: de voorwaarde van `Probe::new`, waar `base` vandaan komt.
        unsafe { dev::regs(self.base) }
    }

    /// Zet één RX-descriptor in de leesvorm: bufferadres, geen tweede
    /// buffer, en OWN|BUF1V|IOC als laatste, zodat de DMA hem mag vullen.
    ///
    /// De DWMAC4-descriptor heeft een lees- en een schrijfvorm, en de DMA
    /// schrijft bij afronding ALLE VIER de woorden met status vol: het
    /// bufferadres is dan weg en moet er bij elke teruggave opnieuw in.
    fn give_rx(&self, i: u16) {
        let d = self.ring.rx(i);
        let b = self.ring.rx_buf(i);
        dev::write32(d, lo(b));
        dev::write32(d.add(4), hi(b));
        dev::write32(d.add(8), 0);
        dev::mb();
        dev::write32(d.add(12), RX_OWN | RX_BUF1_VALID | RX_IOC);
    }

    /// Bouwt de ringen en programmeert MAC, MTL en DMA; de reset is al
    /// gedaan. De volgorde is die van Linux (`stmmac_hw_setup`).
    fn program(&mut self, mbps: u32, full_duplex: bool) -> Result {
        let r = self.regs();
        let c = &r.chan;

        // 1. De AXI-kant, met de waarden uit de DTS van dit silicium.
        r.dma_sys_bus_mode
            .update(|v| (v & !SYS_BUS_OSR_MASK) | SYS_BUS_MODE);

        // 2. De ringen vóórdat de DMA er iets van weet: RX meteen van de
        //    DMA, TX van ons.
        for i in 0..NUM_RX {
            self.give_rx(i);
        }
        for i in 0..NUM_TX {
            dev::clear(self.ring.tx(i), DESC_SIZE as usize);
        }
        dev::mb();

        // 3. Kanaal 0: PBL, interrupts dicht (het board zet ze open), de
        //    ring-adressen en de lengtes: het AANTAL MIN ÉÉN.
        c.control.update(|v| v | CHAN_PBL_X8);
        c.intr_ena.write(0);
        c.tx_control
            .update(|v| v | (PBL << TX_PBL_SHIFT) | CHAN_OSP);
        c.tx_base_hi.write(hi(self.ring.tx_desc));
        c.tx_base.write(lo(self.ring.tx_desc));
        c.tx_ring_len.write(u32::from(NUM_TX) - 1);
        // RBSZ past (const-assertie bij BUF_SIZE); `None` kan hier niet.
        let rxc = rx_control(c.rx_control.read(), BUF_SIZE).unwrap_or(0);
        c.rx_control.write(rxc);
        c.rx_base_hi.write(hi(self.ring.rx_desc));
        c.rx_base.write(lo(self.ring.rx_desc));
        c.rx_ring_len.write(u32::from(NUM_RX) - 1);

        // Tail-pointers: er is geen poll-demand in deze generatie, de DMA
        // werkt tot de tail en stopt. RX staat vol, dus de tail is één
        // voorbij het einde; TX is leeg, dus de tail is de basis.
        c.rx_end
            .write(lo(self.ring.rx(0).add(u64::from(NUM_RX) * DESC_SIZE)));
        c.tx_end.write(lo(self.ring.tx_desc));

        // 4. MTL: store-and-forward beide kanten, de queue-maten uit de
        //    hardware (TQS/RQS = fifo/256 - 1). Een nul-register betekent
        //    een blok zonder klok, en dan loopt fifo/256-1 om.
        let f1 = r.hw_feature1.read();
        let (tx, rx) = fifo_sizes(f1);
        if tx < 256 || rx < 256 {
            return Err(Error::Fifo {
                tx,
                rx,
                hw_feature1: f1,
            });
        }
        r.mtl_tx_op_mode
            .write(MTL_TSF | MTL_TXQ_EN | ((tx / 256 - 1) << MTL_TQS_SHIFT));
        r.mtl_rx_op_mode
            .write(MTL_RSF | ((rx / 256 - 1) << MTL_RQS_SHIFT));

        // 5. Het MAC-adres in de perfect-filter vóór RX aan gaat; filter 0
        //    is perfect match plus broadcast, geen promiscuous.
        let m = self.mac.0;
        r.addr0_hi.write((u32::from(m[5]) << 8) | u32::from(m[4]));
        r.addr0_lo
            .write(u32::from_le_bytes([m[0], m[1], m[2], m[3]]));
        r.packet_filter.write(0);
        r.rxq_ctrl0.write(RXQ0_DCB_ENABLE);

        // 6. De MAC-config: core-init plus snelheid en duplex.
        r.config.update(|v| mac_config(v, mbps, full_duplex));

        // 7. Lopen: sticky bits van vóór de reset weg, dan de DMA, dan de
        //    MAC.
        r.dma_status.update(|v| v);
        c.status.update(|v| v);
        c.tx_control.update(|v| v | CHAN_START);
        c.rx_control.update(|v| v | CHAN_START);
        r.config.update(|v| v | CFG_TE | CFG_RE);
        dev::mb();
        Ok(())
    }

    /// Het interrupt-pad, voor het board.
    #[must_use]
    pub fn irq_ack(&self) -> IrqAck {
        IrqAck { base: self.base }
    }

    /// Hangt de bel van de NIC-interrupt aan de driver en zet de
    /// RX-interrupt open. De RX-pomp wacht dan op de bel in plaats van te
    /// pollen.
    pub fn set_irq(&mut self, bell: &'static Signal) {
        self.irq = Some(bell);
        self.rearm();
    }

    /// Status gewist, masker open.
    fn rearm(&mut self) {
        let c = &self.regs().chan;
        c.status.write(STAT_RI | STAT_NIS);
        c.intr_ena.write(INTR_NIE | INTR_RIE);
        self.stats.rearms += 1;
    }

    /// Het MAC-adres zoals het in de perfect-filter staat.
    #[must_use]
    pub fn filter_mac(&self) -> Mac {
        let r = self.regs();
        let h = r.addr0_hi.read().to_le_bytes();
        let l = r.addr0_lo.read().to_le_bytes();
        Mac([l[0], l[1], l[2], l[3], h[0], h[1]])
    }

    /// De MDIO-master, ook na de start (link-status, diagnose).
    #[must_use]
    pub fn mdio(&self) -> Probe {
        Probe {
            base: self.base,
            csr: self.csr,
            now: self.now,
        }
    }

    /// Eén regel voor een mislukte bring-up: liep de DMA, waar staan beide
    /// ringen, en wat vond de MAC ervan.
    #[must_use]
    pub fn diag(&self) -> Diag {
        let r = self.regs();
        let c = &r.chan;
        dev::mb();
        Diag {
            chan_status: c.status.read(),
            dma_status: r.dma_status.read(),
            dma_debug: r.dma_debug0.read(),
            mtl_tx: r.mtl_tx_debug.read(),
            mtl_rx: r.mtl_rx_debug.read(),
            mac_cfg: r.config.read(),
            rx_cur: self.rx_cur,
            rx_rdes3: dev::read32(self.ring.rx(self.rx_cur).add(12)),
            hw_rx: c.cur_rx_desc.read(),
            tx_cur: self.tx_cur,
            tx_tdes3: dev::read32(self.ring.tx(self.tx_cur).add(12)),
            hw_tx: c.cur_tx_desc.read(),
            stats: self.stats,
        }
    }
}

/// De diagnose van [`Dwmac4::diag`].
#[derive(Copy, Clone, Debug)]
pub struct Diag {
    chan_status: u32,
    dma_status: u32,
    dma_debug: u32,
    mtl_tx: u32,
    mtl_rx: u32,
    mac_cfg: u32,
    rx_cur: u16,
    rx_rdes3: u32,
    hw_rx: u32,
    tx_cur: u16,
    tx_tdes3: u32,
    hw_tx: u32,
    stats: Stats,
}

impl fmt::Display for Diag {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "chan-status {:#010x} dma-status {:#010x} dbg {:#010x} mtl tx {:#010x} rx {:#010x} \
             mac-cfg {:#010x} rx={}/{}(err) last-err {:#010x} tx={} \
             rxdesc[{}] {:#010x} hw-rx {:#010x} txdesc[{}] {:#010x} hw-tx {:#010x}",
            self.chan_status,
            self.dma_status,
            self.dma_debug,
            self.mtl_tx,
            self.mtl_rx,
            self.mac_cfg,
            self.stats.rx_frames,
            self.stats.rx_errors,
            self.stats.rx_last_err,
            self.stats.tx_frames,
            self.rx_cur,
            self.rx_rdes3,
            self.hw_rx,
            self.tx_cur,
            self.tx_tdes3,
            self.hw_tx,
        )
    }
}

impl netdev::Device for Dwmac4 {
    /// Zet één frame op de TX-ring; de tail-pointer volgt in `flush`. Vol
    /// is de ring als de descriptor van deze plek nog van de DMA is (hij
    /// verzond hem nog niet, of hij zag de tail nog niet).
    fn transmit(&mut self, frame: &[u8]) -> core::result::Result<(), TxError> {
        let Some((des2, des3)) = tx_desc23(frame.len()) else {
            return Err(TxError::Size(frame.len()));
        };
        let d = self.ring.tx(self.tx_cur);
        dev::mb();
        if dev::read32(d.add(12)) & TX_OWN != 0 {
            self.stats.tx_full += 1;
            return Err(TxError::Full);
        }
        let b = self.ring.tx_buf(self.tx_cur);
        dev::copy_in(b, frame);
        // De afgeronde descriptor draagt status in alle vier de woorden, dus
        // het bufferadres moet er per frame opnieuw in. Dit gaat pas fout ná
        // één ronde door de ring: het soort bug dat een korte test overleeft
        // en een download niet.
        dev::write32(d, lo(b));
        dev::write32(d.add(4), hi(b));
        dev::write32(d.add(8), des2);
        dev::mb();
        // OWN als laatste: pas dan mag de DMA hem zien.
        dev::write32(d.add(12), des3);
        self.tx_cur = (self.tx_cur + 1) % NUM_TX;
        self.tx_dirty = true;
        self.stats.tx_frames += 1;
        Ok(())
    }

    /// Haalt één frame op, of `None` als de RX-ring leeg is. De descriptor
    /// is device-eigen: een foutframe, een gesplitst frame of een lengte
    /// voorbij de buffer wordt geteld en teruggegeven zonder kopie.
    ///
    /// Leeg met een bedrade lijn: dan gaat het masker weer open (de
    /// dispatch sloot het bij de claim), en daarna kijkt de driver nog één
    /// keer, zodat een frame dat tussen de lees en het openen binnenkwam
    /// niet tot de vangrail blijft liggen.
    fn receive(&mut self, buf: &mut [u8]) -> Option<usize> {
        if let Some(n) = self.receive_one(buf) {
            return Some(n);
        }
        if self.irq.is_none() || self.regs().chan.intr_ena.read() != 0 {
            return None;
        }
        self.rearm();
        self.receive_one(buf)
    }

    /// De tail-pointers: één schrijf per ring per burst.
    fn flush(&mut self) {
        dev::mb();
        let c = &self.regs().chan;
        if self.tx_dirty {
            c.tx_end.write(lo(self.ring.tx(self.tx_cur)));
            self.tx_dirty = false;
            self.stats.doorbells += 1;
        }
        if self.rx_dirty {
            c.rx_end.write(lo(self.ring.rx(self.rx_cur)));
            self.rx_dirty = false;
            self.stats.doorbells += 1;
        }
    }

    fn mac(&self) -> Mac {
        self.mac
    }

    fn irq(&self) -> Option<&'static Signal> {
        self.irq
    }
}

impl Dwmac4 {
    /// Eén descriptor lezen en teruggeven. `None` = nog van de DMA, of
    /// afgekeurd en de ring is daarna leeg.
    fn receive_one(&mut self, buf: &mut [u8]) -> Option<usize> {
        loop {
            let i = self.rx_cur;
            let d = self.ring.rx(i);
            let sts = dev::read32(d.add(12));
            if sts & RX_OWN != 0 {
                return None;
            }
            // OWN is vrij: pas nú de rest lezen. Onder Normal-NC mag de CPU
            // de bufferloads vóór de OWN-load uitvoeren (een control
            // dependency ordent load naar store, niet load naar load).
            dev::mb();
            let got =
                if sts & RX_ERR_SUMMARY != 0 || sts & (RX_FIRST | RX_LAST) != RX_FIRST | RX_LAST {
                    self.stats.rx_errors += 1;
                    self.stats.rx_last_err = sts;
                    None
                } else {
                    let len = rx_len(sts);
                    if len > BUF_SIZE - FCS_LEN {
                        self.stats.rx_oversize += 1;
                        None
                    } else {
                        let n = len.min(buf.len());
                        if let Some(dst) = buf.get_mut(..n) {
                            dev::copy_out(dst, self.ring.rx_buf(i));
                        }
                        self.stats.rx_frames += 1;
                        Some(n)
                    }
                };
            self.give_rx(i);
            self.rx_cur = (i + 1) % NUM_RX;
            self.rx_dirty = true;
            if got.is_some() {
                return got;
            }
        }
    }
}
