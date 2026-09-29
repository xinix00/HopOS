//! Synopsys DesignWare MAC van de 3.x-generatie (DWMAC1000/GMAC): de
//! descriptorringen, de DMA en de MDIO-master.
//!
//! Op de Sipeed LicheeRV Nano (Sophgo SG2002, RISC-V T-Head C906) hangt de
//! 100M-poort via RMII aan de interne ePHY. Het eerste RISC-V-board in de
//! boom, en het enige waar de DMA-regio gecachet is (zie CACHE hieronder).
//!
//! Geschreven naar de vendor U-Boot-driver (`designware.c`, bindt
//! "cvitek,ethernet") en de Linux stmmac-glue (`dwmac-cvitek.c`); gepold,
//! één RX- en één TX-ring (Go: `metal/driver/nic/dwmac`).
//!
//! Waarom een eigen crate naast `driver-dwmac4`: die generatie (4.x/5.x)
//! deelt met deze alleen de naam en de leverancier. Hier zit de MDIO op
//! 0x10/0x14, het MAC-adres op 0x40, de DMA op 0x1000 zonder kanalen of
//! MTL, en de descriptors zijn het "normal format" van vier woorden met een
//! poll-demand-register in plaats van een tail-pointer.
//!
//! De SoC-glue zit NIET hier maar in het board: klokgates, ePHY-power-on en
//! de analoge kalibratie zijn board-kennis. Deze crate is de IP-core; de
//! clause-22-PHY-logica (scan, autonegotiatie zonder gigabit) komt uit
//! `driver-mdio`, via [`Mdio`] op [`Probe`].
//!
//! Read-only bevestigd op ijzer (probe 30-07, vóór de RJ45 gesoldeerd was):
//! klokgates open, versie-register 0x1037 op basis 0x04070000, en de interne
//! ePHY antwoordt op MDIO-adres 0 met id 0043:5649, precies het id dat de
//! ePHY-init zelf in de PHY schrijft, dus die keten liep. Wat dáármee nog
//! niet bewezen was, is alles daaronder: DMA.
//!
//! De levensloop is een type (handboek §1.2): een [`Probe`] is het blok
//! vóór de ringen (versie, reset, MDIO), [`Probe::start`] maakt er een
//! [`Dwmac`] van, en alleen die draagt frames.
//!
//! Batching: [`transmit`](netdev::Device::transmit) en de RX-teruggave
//! zetten descriptors klaar; de poll-demands (de doorbell van deze
//! generatie) vallen pas in [`flush`](netdev::Device::flush), één keer per
//! burst.
//!
//! CACHE-COHERENTIE, het echte verschil met de ARM-boards. Daar ligt de
//! DMA-regio buiten élke RAM-declaratie én ongecachet (Normal-NC); op de
//! C906 in M-mode is er geen tweede laag die dat kan afdwingen (geen MMU;
//! geheugenattributen komen uit de sysmap van de core), dus de regio is
//! gewoon cachebaar DRAM. Elke overdracht loopt daarom door [`dev::push`]
//! (clean, vóór de controller leest) en [`dev::pull`] (clean en invalidate,
//! vóór de CPU leest wat de controller schreef).
//!
//! En daarom staat elke descriptor en elke buffer op een eigen cacheline
//! van [`dev::LINE`] bytes: descriptors [`DESC_STRIDE`] uit elkaar via de
//! Descriptor Skip Length in DMA_BUS_MODE, buffers een veelvoud van een line
//! groot, en de regio op een line gealigneerd. Aaneengesloten
//! 16B-descriptors zoals de vendor ze gebruikt zetten er vier in één line,
//! en dan overschrijft onze write-back van descriptor i de updates die de
//! DMA net in i+1..i+3 deed; omgekeerd gooit een invalidate van de één een
//! CPU-schrijf naar de ander weg. Gemeten 30-07 (de LicheeRV-ring die
//! stilviel door het cache-onderhoud): DHCP lukte nog (twee frames, ver uit
//! elkaar), maar ping verloor de helft van de pakketten en een
//! TLS-handshake liep nooit af. U-Boot komt met DSL=0 weg omdat het één
//! frame per keer doet; een netstack niet.

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
use dev::{LINE, Pa};
use driver_mdio::Mdio;
use netdev::{Mac, TxError};
use regs::Regs;

pub use regs::CSR_250_300M;

// MAC_CONFIG-bits.
/// PS: MII/RMII (10/100) in plaats van GMII.
const CONF_PORT_MII: u32 = 1 << 15;
/// FES: 100 Mbit in plaats van 10.
const CONF_FES100: u32 = 1 << 14;
/// DO: eigen frames niet terugontvangen (half duplex).
const CONF_DIS_RX_OWN: u32 = 1 << 13;
/// DM: full duplex.
const CONF_DUPLEX: u32 = 1 << 11;
/// Transmitter aan.
const CONF_TX_EN: u32 = 1 << 3;
/// Receiver aan.
const CONF_RX_EN: u32 = 1 << 2;

// MAC_FRAME_FILTER-bits.
/// PR: promiscuous.
const FILTER_PR: u32 = 1 << 0;
/// PM: alle multicast doorlaten.
const FILTER_PM: u32 = 1 << 4;

// DMA_BUS_MODE-bits.
/// De softreset; klaart vanzelf.
const BUS_SW_RESET: u32 = 1 << 0;
/// PBL = 8 beats in [13:8].
const BUS_PBL8: u32 = 8 << 8;
/// Fixed burst.
const BUS_FIXED_BURST: u32 = 1 << 16;
/// Het DSL-veld [6:2].
const BUS_DSL_SHIFT: u32 = 2;
/// DSL is vijf bits breed.
const BUS_DSL_MAX: u64 = 0x1F;

// DMA_OP_MODE-bits.
/// TX pas versturen als het frame compleet in de FIFO staat.
const OP_STORE_FORWARD: u32 = 1 << 21;
/// De TX-FIFO leegmaken.
const OP_FLUSH_TX_FIFO: u32 = 1 << 20;
/// De TX-DMA loopt.
const OP_TX_START: u32 = 1 << 13;
/// De RX-DMA loopt.
const OP_RX_START: u32 = 1 << 1;

// Het descriptorformaat: "normal format", 16 bytes, géén ALTDESCRIPTOR (bit
// 7 van DMA_BUS_MODE blijft 0, net als bij de vendor).
/// RDES0/TDES0: status en OWN.
const DES_STATUS: u64 = 0;
/// RDES1/TDES1: controle en buffergrootte.
const DES_CNTL: u64 = 4;
/// RDES2/TDES2: het bufferadres.
const DES_BUF: u64 = 8;
/// RDES3/TDES3: buffer 2 of de volgende descriptor; ongebruikt in
/// ring-mode.
const DES_NEXT: u64 = 12;

/// OWN: 1 = van de DMA.
const DESC_OWN: u32 = 1 << 31;

/// De framelengte in RDES0 [29:16], inclusief de FCS.
const RX_LEN_SHIFT: u32 = 16;
/// Het lengteveld is veertien bits.
const RX_LEN_MASK: u32 = 0x3FFF;
/// ES: samengevatte fout.
const RX_STS_ERROR: u32 = 1 << 15;
/// FS: eerste descriptor van het frame.
const RX_STS_FIRST: u32 = 1 << 9;
/// LS: laatste descriptor van het frame.
const RX_STS_LAST: u32 = 1 << 8;

/// TDES1 LS.
const TX_CNTL_LAST: u32 = 1 << 30;
/// TDES1 FS.
const TX_CNTL_FIRST: u32 = 1 << 29;
/// RER/TER: de laatste descriptor van de ring; de DMA springt terug.
const RING_END: u32 = 1 << 25;
/// RBS1/TBS1: ELF bits, zie [`MAX_FRAME`].
const CNTL_SIZE1_MASK: u32 = 0x7FF;

/// De MAC meldt de lengte mét FCS.
const FCS_LEN: usize = 4;

/// Eén descriptor: vier woorden van 32 bits.
pub const DESC_SIZE: u64 = 16;

/// De afstand tussen twee descriptors: één cacheline.
///
/// Dat is geen optimalisatie maar een correctheidseis op dit board. Zonder
/// DSL staan vier descriptors in één 64B-line: als wij er één beschrijven
/// (OWN teruggeven) is die hele line dirty, en de write-back overschrijft
/// dan de updates die de DMA net in de BUURdescriptors zette. Gemeten 30-07:
/// DHCP (twee frames) lukte, maar ping verloor de helft en een TLS-handshake
/// liep nooit af. De vendor-U-Boot komt hier weg met DSL=0 omdat die één
/// frame per keer doet en tussendoor niets anders; een netstack doet dat
/// niet.
pub const DESC_STRIDE: u64 = LINE;

/// DSL (Descriptor Skip Length): hoeveel 32-bit woorden de DMA tussen twee
/// descriptors overslaat. Afgeleid, niet met de hand: 48 bytes skip, met een
/// 16B-descriptor precies [`DESC_STRIDE`], dus 12.
const BUS_DSL: u32 = (((DESC_STRIDE - DESC_SIZE) / 4) as u32) << BUS_DSL_SHIFT;

/// Het DMA_BUS_MODE-woord na de reset.
const BUS_MODE: u32 = BUS_FIXED_BURST | BUS_PBL8 | BUS_DSL;

/// Ringdiepte, per ring: een tijdsbudget, niet een smaak.
///
/// Bij 100 Mbit duurt één frame van 1500 B 120 µs op de draad, dus
/// `NUM_DESC` frames zijn `NUM_DESC` × 120 µs buffering: hoe lang deze
/// driver niet aan de beurt hoeft te komen zonder dat de MAC frames
/// weggooit.
///
/// 64 gaf ~8 ms, en dat bleek 10-08 op ijzer te krap, om een andere reden
/// dan waarvoor het gekozen was. GEMETEN met de netmeter-bank: het ophalen én
/// verwerken van een frame kost samen 47 µs (9 µs driver, 38 µs stack), dus
/// de lus is twee keer sneller dan de draad en kan per definitie niet
/// achterlopen. Wat hem wél wegjoeg was de Go-scheduler: op één hart deelde
/// de RX-lus de core met de goroutine die de download verwerkte, en het
/// preemptie-quantum was ~10 ms. In 10 ms komen er 83 frames binnen, meer
/// dan 64. Meetbaar gevolg: 3 tot 41 verloren frames per 16 MB-download
/// (teller missed-ring in [`Diag`]), en omdat elk verlies ~28 ms herstel
/// kost zakte de doorvoer van 5,8 naar 4,2 MB/s, bimodaal per run.
///
/// 128 geeft ~15 ms en dekt dus een heel quantum. De hele set is dan
/// [`NEED_BYTES`] = 432 KB en past nog in de OS-staart van dit board. Dieper
/// kan pas als die staart meegroeit, en dat is pas te verantwoorden als een
/// meting zegt dat 15 ms nog niet genoeg is.
pub const NUM_DESC: u16 = 128;

/// Eén buffer: 26 × 64 B, net boven [`MAX_FRAME`], en een veelvoud van een
/// cacheline zodat twee buffers nooit een line delen.
pub const BUF_SIZE: usize = 1664;

/// Wat we de MAC als buffergrootte MELDEN, en tegelijk het grootste frame
/// dat we versturen. Niet hetzelfde als [`BUF_SIZE`]: het RBS1-veld is 11
/// bits, dus 2048 past er níet in. Dat maskeert naar nul en dan denkt de
/// MAC dat elke buffer nul bytes groot is. Gemeten gevolg (30-07, eerste
/// DMA-boot): de ring gaf 128 descriptors terug zonder één bruikbaar frame;
/// link stond, TX liep, RX kapot. De vendor programmeert hier 1600
/// (`designware.h`, `MAC_MAX_FRAME_SZ`) terwijl zijn buffers óók 2048 zijn;
/// dat is precies dit onderscheid. Een const-assertie bewaakt het nu, in
/// plaats van een stil masker (Go: een panic bij het bouwen van de ring).
pub const MAX_FRAME: usize = 1600;

/// De twee ringen samen: RX, dan TX, elk [`DESC_STRIDE`] per descriptor.
///
/// Afgeleid, niet met de hand: de buffers beginnen hier precies achter. Met
/// een hardgecodeerde 0x1000 lag de TX-ring bovenop de eerste RX-buffers
/// zodra de ring dieper werd dan 32; gemeten 30-07 bij het verdiepen naar
/// 64: link stond, RX en TX bewogen, en DHCP kreeg geen lease meer.
const DESC_BYTES: u64 = 2 * NUM_DESC as u64 * DESC_STRIDE;

/// Alle buffers: RX, dan TX.
const BUF_BYTES: u64 = 2 * NUM_DESC as u64 * BUF_SIZE as u64;

/// De DMA-regio die deze driver nodig heeft (descriptors plus 2 × 128
/// frame-buffers): 432 KB. Het board reserveert dit in zijn plan, op een
/// cacheline gealigneerd en onder 4 GB.
pub const NEED_BYTES: u64 = DESC_BYTES + BUF_BYTES;

const _: () = {
    // Eén descriptor per line, en de DSL past in zijn vijf bits.
    assert!(DESC_SIZE <= DESC_STRIDE);
    assert!(DESC_STRIDE.is_multiple_of(LINE));
    assert!((DESC_STRIDE - DESC_SIZE) / 4 <= BUS_DSL_MAX);
    assert!(BUS_DSL == 12 << BUS_DSL_SHIFT);
    // Eén buffer per line-reeks, en de buffers beginnen op een line.
    assert!((BUF_SIZE as u64).is_multiple_of(LINE));
    assert!(DESC_BYTES.is_multiple_of(LINE));
    // RBS1 moet de maat dragen, de buffer moet het frame dragen, en het
    // frame moet boven een volledig ethernetframe (1518) liggen.
    assert!(MAX_FRAME as u32 & !CNTL_SIZE1_MASK == 0);
    assert!(MAX_FRAME <= BUF_SIZE);
    assert!(MAX_FRAME >= 1518);
    // De frames van de kern passen.
    assert!(netdev::MAX_FRAME <= MAX_FRAME);
};

/// Waarom de driver weigert.
#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    /// VERSION leest 0 of alles-één: een blok zonder klok of een dode bus.
    NoMac {
        /// De gelezen waarde.
        version: u32,
    },
    /// De DMA-softreset klaart niet. Die hangt als de klokgates dicht
    /// staan; het board opent ze vóór de reset, dus dit betekent iets
    /// anders, en de melding zegt dat in plaats van te gokken.
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
    /// De DMA-regio ligt niet op een cacheline of niet onder 4 GB.
    DmaPlace {
        /// Het begin.
        base: u64,
    },
    /// Een snelheid die deze MAC via RMII niet kan: alleen 10 en 100.
    Speed {
        /// De gevraagde snelheid.
        mbps: u32,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::NoMac { version } => write!(f, "dwmac: no MAC (version {version:#010x})"),
            Self::ResetStuck { bus_mode } => write!(
                f,
                "dwmac: DMA soft reset does not clear (bus mode {bus_mode:#010x})"
            ),
            Self::DmaTooSmall { need, have } => write!(
                f,
                "dwmac: DMA region {} KB, need at least {} KB",
                have >> 10,
                need >> 10
            ),
            Self::DmaPlace { base } => write!(
                f,
                "dwmac: DMA region at {base:#x} is not cacheline aligned or above 4 GB"
            ),
            Self::Speed { mbps } => write!(f, "dwmac: {mbps} Mbps is not a RMII speed"),
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Monotone nanoseconden; de klok van het board.
pub type Clock = fn() -> u64;

/// De MDIO-grens: 100 ms, wat Go als 10000 × 10 µs spinde.
const MDIO_TIMEOUT_NS: u64 = 100_000_000;
/// De grens van de DMA-softreset: 1 s, wat Go als 100000 × 10 µs spinde.
const RESET_TIMEOUT_NS: u64 = 1_000_000_000;

// --- de rekenstukken, apart zodat ze op de host bewijsbaar zijn -----------
//
// De descriptorwoorden zijn bit-arithmetiek en horen op de host bewezen te
// worden, niet op een bordje waar één ronde een kaartwissel kost. Precies
// dit ging fout op de eerste DMA-boot (30-07).

/// Het RDES1-woord van een RX-descriptor: de buffergrootte die we de MAC
/// melden, plus de ring-end-bit op de laatste. Aparte functie omdat dit het
/// woord is waar de eerste DMA-boot op stukliep; dat [`MAX_FRAME`] in RBS1
/// past is een const-assertie.
#[must_use]
pub const fn rx_cntl(last: bool) -> u32 {
    let c = MAX_FRAME as u32;
    if last { c | RING_END } else { c }
}

/// Het TDES1-woord voor één frame: eerste én laatste descriptor van het
/// frame (wij versturen nooit gefragmenteerd), de lengte, en de
/// ring-end-bit op de laatste descriptor van de ring. `None` buiten
/// `1..=MAX_FRAME`: een lengte die niet in TBS1 past, wordt geweigerd en
/// niet gemaskeerd.
#[must_use]
pub fn tx_cntl(len: usize, last: bool) -> Option<u32> {
    if len == 0 || len > MAX_FRAME {
        return None;
    }
    let c = TX_CNTL_FIRST | TX_CNTL_LAST | len as u32;
    Some(if last { c | RING_END } else { c })
}

/// De framelengte uit RDES0 zonder de vier FCS-bytes, of `None` als de
/// melding niet kan: korter dan een FCS, of langer dan wat we de MAC als
/// buffer meldden. Een lengte uit het device mag nooit bytes uit de
/// naburige DMA-buffer blootgeven.
#[must_use]
pub fn rx_len(rdes0: u32) -> Option<usize> {
    let raw = ((rdes0 >> RX_LEN_SHIFT) & RX_LEN_MASK) as usize;
    if raw <= FCS_LEN || raw > MAX_FRAME {
        return None;
    }
    Some(raw - FCS_LEN)
}

/// Is RDES0 een goed frame in één descriptor? Een foutframe (ES) of een
/// frame over meerdere descriptors (kan niet: wat we als buffergrootte
/// melden ligt boven de MTU) is dat niet.
#[must_use]
pub fn rx_whole(rdes0: u32) -> bool {
    rdes0 & RX_STS_ERROR == 0 && rdes0 & (RX_STS_FIRST | RX_STS_LAST) == RX_STS_FIRST | RX_STS_LAST
}

/// Het MAC_CONFIG-woord voor een link: altijd MII/RMII (PS), DO, TX en RX
/// aan; FES op 100 en DM bij full duplex. `None` voor een snelheid die RMII
/// niet kent.
#[must_use]
pub fn mac_conf(mbps: u32, full_duplex: bool) -> Option<u32> {
    let mut conf = CONF_PORT_MII | CONF_DIS_RX_OWN | CONF_TX_EN | CONF_RX_EN;
    match mbps {
        10 => {}
        100 => conf |= CONF_FES100,
        _ => return None,
    }
    if full_duplex {
        conf |= CONF_DUPLEX;
    }
    Some(conf)
}

/// Het GMII_ADDR-commandowoord (designware.h: adres in `[15:11]`, register in
/// `[10:6]`, CSR-range in `[5:2]`, write = bit 1, busy = bit 0).
#[must_use]
pub fn mdio_cmd(phy: u8, reg: u8, csr: u32, write: bool) -> u32 {
    let op = if write { regs::MII_WRITE } else { 0 };
    ((u32::from(phy) & 0x1F) << regs::MII_ADDR_SHIFT)
        | ((u32::from(reg) & 0x1F) << regs::MII_REG_SHIFT)
        | ((csr & 0xF) << regs::MII_CSR_SHIFT)
        | op
        | regs::MII_BUSY
}

/// Wat de MAC weggooide zonder het ons aan te bieden, uit één lezing van
/// de Missed Frame and Buffer Overflow Counter: (geen vrije descriptor,
/// volle RX-FIFO, overlopen tellers).
#[must_use]
pub fn missed(v: u32) -> (u64, u64, u64) {
    let ring = u64::from(v & 0xFFFF);
    let fifo = u64::from((v >> 17) & 0x7FF);
    let ovf = u64::from((v >> 16) & 1) + u64::from((v >> 28) & 1);
    (ring, fifo, ovf)
}

/// Het blok vóór de ringen: versie, reset en de MDIO-master. Het board
/// doet hiermee de PHY-scan en de autonegotiatie, en maakt er dan met
/// [`Probe::start`] een draaiende [`Dwmac`] van.
pub struct Probe {
    base: Pa,
    csr: u32,
    now: Clock,
}

impl Probe {
    /// Het blok op `base` (0x04070000 op de SG2002), met CSR-klokrange
    /// `csr` (op de SG2002 [`CSR_250_300M`]). Raakt nog niets aan.
    ///
    /// # Safety
    ///
    /// `base` is het gemapte registerblok van een DWMAC1000 van minstens
    /// 0x1050 bytes, dat blijft zolang het programma draait, en de
    /// klokgates staan open (het board doet dat vóór deze aanroep).
    #[must_use]
    pub unsafe fn new(base: Pa, csr: u32, now: Clock) -> Self {
        Self { base, csr, now }
    }

    fn regs(&self) -> &'static Regs {
        // SAFETY: de voorwaarde van `new`.
        unsafe { dev::regs(self.base) }
    }

    /// Het rauwe VERSION-register (0x1037 op dit silicium). Het eerste dat
    /// een bring-up hoort te lezen.
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

    /// Het rauwe GMII_ADDR-register, voor het meetinstrument: blijft BUSY
    /// staan, dan wacht de machine op een klok, en dat is een ander
    /// probleem dan "er zit geen PHY".
    #[must_use]
    pub fn mdio_state(&self) -> u32 {
        self.regs().gmii_addr.read()
    }

    /// De DMA-softreset. Apart van `start`: de reset zet ook de
    /// MDIO-machine schoon, en het board mag hem vóór de PHY-scan doen.
    pub fn reset(&mut self) -> Result {
        let r = self.regs();
        r.bus_mode.update(|v| v | BUS_SW_RESET);
        let deadline = (self.now)().saturating_add(RESET_TIMEOUT_NS);
        loop {
            if r.bus_mode.read() & BUS_SW_RESET == 0 {
                return Ok(());
            }
            if (self.now)() >= deadline {
                return Err(Error::ResetStuck {
                    bus_mode: r.bus_mode.read(),
                });
            }
            core::hint::spin_loop();
        }
    }

    /// Wacht, begrensd, tot de MDIO-machine vrij is. Een bool en geen
    /// fout: de aanroepers maken er [`driver_mdio::Error::Bus`] van, wat
    /// de scan als leeg adres leest (Go: 0xffff, "geen PHY").
    fn mdio_wait(&self) -> bool {
        let r = self.regs();
        let deadline = (self.now)().saturating_add(MDIO_TIMEOUT_NS);
        loop {
            if r.gmii_addr.read() & regs::MII_BUSY == 0 {
                return true;
            }
            if (self.now)() >= deadline {
                return false;
            }
            core::hint::spin_loop();
        }
    }

    /// Legt de ringen in de DMA-regio en zet MAC en DMA aan: na de
    /// board-glue (klokgates, ePHY) en na de autonegotiatie, want `mbps` en
    /// `full_duplex` komen daaruit. Doet zelf de softreset.
    ///
    /// # Safety
    ///
    /// `[dma, dma + dma_size)` is gemapt geheugen onder 4 GB dat alleen
    /// deze driver en het device gebruiken, nu en zolang het programma
    /// draait. Het mag gecachet zijn: de driver doet het cache-onderhoud
    /// zelf, en weigert een regio die niet op een cacheline begint.
    pub unsafe fn start(
        mut self,
        dma: Pa,
        dma_size: u64,
        mac: Mac,
        mbps: u32,
        full_duplex: bool,
    ) -> Result<Dwmac> {
        if dma_size < NEED_BYTES {
            return Err(Error::DmaTooSmall {
                need: NEED_BYTES,
                have: dma_size,
            });
        }
        if !dma.is_aligned(LINE) || dma.0.saturating_add(NEED_BYTES) > 1 << 32 {
            return Err(Error::DmaPlace { base: dma.0 });
        }
        let Some(conf) = mac_conf(mbps, full_duplex) else {
            return Err(Error::Speed { mbps });
        };
        self.reset()?;
        let mut n = Dwmac {
            base: self.base,
            csr: self.csr,
            now: self.now,
            mac,
            ring: Rings::at(dma),
            rx_cur: 0,
            tx_cur: 0,
            rx_dirty: false,
            tx_dirty: false,
            stats: Stats::default(),
        };
        n.program(conf);
        Ok(n)
    }
}

impl Mdio for Probe {
    fn read(&mut self, phy: u8, reg: u8) -> driver_mdio::Result<u16> {
        let bus = driver_mdio::Error::Bus { phy, reg };
        if !self.mdio_wait() {
            return Err(bus);
        }
        let r = self.regs();
        r.gmii_addr.write(mdio_cmd(phy, reg, self.csr, false));
        if !self.mdio_wait() {
            return Err(bus);
        }
        Ok((r.gmii_data.read() & 0xFFFF) as u16)
    }

    fn write(&mut self, phy: u8, reg: u8, val: u16) -> driver_mdio::Result {
        let bus = driver_mdio::Error::Bus { phy, reg };
        if !self.mdio_wait() {
            return Err(bus);
        }
        let r = self.regs();
        r.gmii_data.write(u32::from(val));
        r.gmii_addr.write(mdio_cmd(phy, reg, self.csr, true));
        if self.mdio_wait() { Ok(()) } else { Err(bus) }
    }
}

/// Waar de ringen en buffers liggen: rx-desc, tx-desc, rx-buf, tx-buf, op
/// een rij in de DMA-regio.
///
/// # Invariants
///
/// Met een regio die op een cacheline begint, begint elke descriptor en
/// elke buffer op een eigen cacheline en deelt geen twee er één.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
struct Rings {
    rx_desc: Pa,
    tx_desc: Pa,
    rx_buf: Pa,
    tx_buf: Pa,
}

impl Rings {
    const fn at(dma: Pa) -> Self {
        // INVARIANT: DESC_STRIDE en BUF_SIZE zijn veelvouden van LINE en
        // DESC_BYTES ook (const-asserties); `start` eist een gealigneerde
        // `dma`.
        Self {
            rx_desc: dma,
            tx_desc: dma.add(NUM_DESC as u64 * DESC_STRIDE),
            rx_buf: dma.add(DESC_BYTES),
            tx_buf: dma.add(DESC_BYTES + NUM_DESC as u64 * BUF_SIZE as u64),
        }
    }

    fn rx(&self, i: u16) -> Pa {
        self.rx_desc.add(u64::from(i) * DESC_STRIDE)
    }

    fn tx(&self, i: u16) -> Pa {
        self.tx_desc.add(u64::from(i) * DESC_STRIDE)
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

/// De meetlat van de driver.
#[derive(Copy, Clone, Default, Debug, PartialEq, Eq)]
pub struct Stats {
    /// Afgeleverde frames.
    pub rx_frames: u64,
    /// Afgekeurde descriptors (foutframe of gesplitst frame).
    pub rx_errors: u64,
    /// Onmogelijke lengtes (korter dan een FCS of voorbij de gemelde
    /// buffer): geteld en overgeslagen, nooit gekopieerd.
    pub rx_bad_len: u64,
    /// De rauwe RDES0 van het laatst afgekeurde frame: zonder dat is
    /// "afgekeurd" op ijzer niet te ontleden.
    pub rx_last_err: u32,
    /// Frames die de MAC weggooide omdat er geen vrije descriptor was (de
    /// ring liep leeg omdat wij te langzaam waren). Onzichtbaar in
    /// `rx_frames`/`rx_errors`: dit is het getal dat een RX-jacht nodig
    /// heeft, want de RU-bit in DMA_STATUS is sticky en zegt alleen "ooit
    /// gebeurd". De hardwareteller wist zichzelf bij lezen, dus accumuleren
    /// we hier.
    pub rx_missed_ring: u64,
    /// Frames weg door een volle RX-FIFO.
    pub rx_missed_fifo: u64,
    /// Hoe vaak een van de twee hardwaretellers zelf overliep.
    pub rx_missed_ovf: u64,
    /// Verzonden frames (op de ring gezet).
    pub tx_frames: u64,
    /// Keren dat de TX-ring vol zat.
    pub tx_full: u64,
    /// Poll-demand-schrijfacties: de doorbells.
    pub doorbells: u64,
}

/// Eén draaiende DWMAC1000.
pub struct Dwmac {
    base: Pa,
    csr: u32,
    now: Clock,
    mac: Mac,
    ring: Rings,
    /// De volgende RX-descriptor die wij lezen.
    rx_cur: u16,
    /// De volgende TX-descriptor die wij vullen.
    tx_cur: u16,
    /// Er zijn RX-descriptors teruggegeven sinds de laatste poll-demand.
    rx_dirty: bool,
    /// Er zijn TX-descriptors gevuld sinds de laatste poll-demand.
    tx_dirty: bool,
    /// De meetlat.
    pub stats: Stats,
}

impl Dwmac {
    fn regs(&self) -> &'static Regs {
        // SAFETY: de voorwaarde van `Probe::new`, waar `base` vandaan komt.
        unsafe { dev::regs(self.base) }
    }

    /// Legt beide ringen en hun buffers in de regio. Ring-mode (geen
    /// chaining): de DMA loopt de descriptors met DSL-stap af en springt
    /// terug bij de descriptor met de ring-end-bit.
    fn init_rings(&mut self) {
        let last = NUM_DESC - 1;
        for i in 0..NUM_DESC {
            let rx = self.ring.rx(i);
            dev::write32(rx.add(DES_STATUS), DESC_OWN); // meteen van de DMA
            dev::write32(rx.add(DES_CNTL), rx_cntl(i == last));
            dev::write32(rx.add(DES_BUF), lo(self.ring.rx_buf(i)));
            dev::write32(rx.add(DES_NEXT), 0);

            let tx = self.ring.tx(i);
            dev::write32(tx.add(DES_STATUS), 0); // van ons
            dev::write32(tx.add(DES_CNTL), 0);
            dev::write32(tx.add(DES_BUF), lo(self.ring.tx_buf(i)));
            dev::write32(tx.add(DES_NEXT), 0);
        }
        self.rx_cur = 0;
        self.tx_cur = 0;
        // De descriptors naar het geheugen, en de buffers schoon en uit de
        // cache: een vuile line van vóór ons (de bootloader) mag later niet
        // over een frame heen worden teruggeschreven dat de DMA erin zette.
        dev::push(self.ring.rx_desc, DESC_BYTES as usize);
        dev::pull(self.ring.rx_buf, BUF_BYTES as usize);
    }

    /// Bouwt de ringen en programmeert MAC en DMA; de reset is al gedaan.
    /// De volgorde is die van de Go-driver (en van de vendor).
    fn program(&mut self, conf: u32) {
        self.init_rings();
        let r = self.regs();

        // Het MAC-adres in de perfect-filter (Addr0). Vóór RX aan, anders
        // draait de MAC even met een 00:00:00:00:00:00-filter.
        let m = self.mac.0;
        r.addr0_hi.write((u32::from(m[5]) << 8) | u32::from(m[4]));
        r.addr0_lo
            .write(u32::from_le_bytes([m[0], m[1], m[2], m[3]]));
        // Álle multicast (PM) + promiscuous (PR). PM: mDNS/matter leeft op
        // 224.0.0.251 en 33:33-groepen, en de hash-filter per groep is meer
        // administratie dan de paar ruisframes waard. PR: de slots zijn met
        // hun éigen MAC's (02:00:00:00:00:XX) L2-burgers op het LAN geworden
        // voor IPv6 (matter praat unicast terug op de MAC die NDP
        // adverteerde), en de perfect-filter kent maar één adres. Ruis is
        // dit op een geswitcht LAN niet: vreemde unicast bereikt onze poort
        // überhaupt niet, de switch leert. De netstack filtert op
        // lidmaatschap en bestemming.
        r.filter.write(FILTER_PM | FILTER_PR);

        r.bus_mode.write(BUS_MODE);
        r.rx_list.write(lo(self.ring.rx_desc));
        r.tx_list.write(lo(self.ring.tx_desc));
        r.op_mode.write(OP_STORE_FORWARD | OP_FLUSH_TX_FIFO);
        r.status.update(|v| v); // sticky bits van vóór de reset wissen (W1C)
        r.conf.write(conf);
        r.op_mode.update(|v| v | OP_TX_START | OP_RX_START);
        dev::mb();
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

    /// Leest de Missed Frame and Buffer Overflow Counter en telt hem op bij
    /// de meetlat. Het register is read-and-clear, dus élke lezing is
    /// "sinds de vorige lezing" en niemand anders mag hem lezen; daarom
    /// `&mut self`.
    fn sample_missed(&mut self) {
        let (ring, fifo, ovf) = missed(self.regs().missed.read());
        self.stats.rx_missed_ring += ring;
        self.stats.rx_missed_fifo += fifo;
        self.stats.rx_missed_ovf += ovf;
    }

    /// Het meetinstrument voor een mislukte bring-up: één regel die zegt of
    /// de DMA liep, waar beide ringen staan en wat de MAC ervan vond. Het
    /// board hangt hem aan de fout als DHCP niets oplevert; dan is één boot
    /// genoeg om te weten of TX de deur uit ging, of RX niets binnenkreeg,
    /// of geen van beide.
    pub fn diag(&mut self) -> Diag {
        let rx = self.ring.rx(self.rx_cur);
        let tx = self.ring.tx(self.tx_cur);
        dev::pull(rx, DESC_SIZE as usize);
        dev::pull(tx, DESC_SIZE as usize);
        self.sample_missed();
        let r = self.regs();
        Diag {
            dma_status: r.status.read(),
            op_mode: r.op_mode.read(),
            rx_cur: self.rx_cur,
            rx_rdes0: dev::read32(rx.add(DES_STATUS)),
            hw_rx: r.cur_rx_desc.read(),
            tx_cur: self.tx_cur,
            tx_tdes0: dev::read32(tx.add(DES_STATUS)),
            hw_tx: r.cur_tx_desc.read(),
            stats: self.stats,
        }
    }

    /// Eén descriptor lezen en teruggeven. `None` = nog van de DMA; een
    /// afgekeurde descriptor wordt geteld, teruggegeven en overgeslagen.
    fn receive_one(&mut self, buf: &mut [u8]) -> Option<usize> {
        loop {
            let i = self.rx_cur;
            let d = self.ring.rx(i);
            // De line vers uit het geheugen: de DMA schreef hem buiten de
            // cache om.
            dev::pull(d, DESC_SIZE as usize);
            let sts = dev::read32(d.add(DES_STATUS));
            if sts & DESC_OWN != 0 {
                return None;
            }
            let got = self.take(i, sts, buf);
            // Teruggeven aan de DMA. RDES1 en RDES2 (buffergrootte,
            // ring-end, adres) staan er nog van `init_rings`: in het normal
            // format schrijft de DMA alleen RDES0 terug. Eigen line, dus deze
            // clean raakt geen buurdescriptor.
            dev::write32(d.add(DES_STATUS), DESC_OWN);
            dev::push(d, DESC_SIZE as usize);
            self.rx_cur = (i + 1) % NUM_DESC;
            self.rx_dirty = true;
            if got.is_some() {
                return got;
            }
        }
    }

    /// Het frame uit descriptor `i` met status `sts` naar `buf`, of `None`
    /// voor een afgekeurde descriptor.
    fn take(&mut self, i: u16, sts: u32, buf: &mut [u8]) -> Option<usize> {
        if !rx_whole(sts) {
            self.stats.rx_errors += 1;
            self.stats.rx_last_err = sts;
            return None;
        }
        let Some(len) = rx_len(sts) else {
            self.stats.rx_bad_len += 1;
            self.stats.rx_last_err = sts;
            return None;
        };
        let n = len.min(buf.len());
        let src = self.ring.rx_buf(i);
        // De buffer vers uit het geheugen vóór de kopie; de lines zijn
        // alleen van deze buffer (BUF_SIZE is een veelvoud van een line).
        dev::pull(src, n);
        if let Some(dst) = buf.get_mut(..n) {
            dev::copy_out(dst, src);
        }
        self.stats.rx_frames += 1;
        Some(n)
    }

    /// De RX-poll-demand, als er descriptors terug zijn.
    fn kick_rx(&mut self) {
        if self.rx_dirty {
            self.regs().rx_poll.write(1);
            self.rx_dirty = false;
            self.stats.doorbells += 1;
        }
    }
}

/// De diagnose van [`Dwmac::diag`].
#[derive(Copy, Clone, Debug)]
pub struct Diag {
    dma_status: u32,
    op_mode: u32,
    rx_cur: u16,
    rx_rdes0: u32,
    hw_rx: u32,
    tx_cur: u16,
    tx_tdes0: u32,
    hw_tx: u32,
    stats: Stats,
}

impl fmt::Display for Diag {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let s = &self.stats;
        write!(
            f,
            "dma-status {:#010x} op-mode {:#010x} rx={}/{}(err)/{}(len) last-err {:#010x} tx={} \
             missed-ring={} missed-fifo={} ctr-ovf={} \
             rxdesc[{}] {:#010x} hw-rx {:#010x} txdesc[{}] {:#010x} hw-tx {:#010x}",
            self.dma_status,
            self.op_mode,
            s.rx_frames,
            s.rx_errors,
            s.rx_bad_len,
            s.rx_last_err,
            s.tx_frames,
            s.rx_missed_ring,
            s.rx_missed_fifo,
            s.rx_missed_ovf,
            self.rx_cur,
            self.rx_rdes0,
            self.hw_rx,
            self.tx_cur,
            self.tx_tdes0,
            self.hw_tx,
        )
    }
}

impl netdev::Device for Dwmac {
    /// Zet één frame op de TX-ring; de poll-demand volgt in `flush`. Vol is
    /// de ring als de descriptor van deze plek nog van de DMA is. (Go wachtte
    /// hier tot 1 s; het contract zegt `Full` en laat de pomp kiezen.)
    fn transmit(&mut self, frame: &[u8]) -> core::result::Result<(), TxError> {
        let last = self.tx_cur == NUM_DESC - 1;
        let Some(cntl) = tx_cntl(frame.len(), last) else {
            return Err(TxError::Size(frame.len()));
        };
        let d = self.ring.tx(self.tx_cur);
        // De DMA schrijft TDES0 buiten de cache om als hij klaar is.
        dev::pull(d, DESC_SIZE as usize);
        if dev::read32(d.add(DES_STATUS)) & DESC_OWN != 0 {
            self.stats.tx_full += 1;
            return Err(TxError::Full);
        }
        // Eerst de buffer naar het geheugen, dan pas de descriptor die
        // ernaar wijst.
        let b = self.ring.tx_buf(self.tx_cur);
        dev::copy_in(b, frame);
        dev::push(b, frame.len());
        // TDES2 (het adres) staat er nog van `init_rings`: in het normal
        // format schrijft de DMA alleen TDES0 terug.
        dev::write32(d.add(DES_CNTL), cntl);
        dev::mb();
        // OWN als laatste: pas hierna is hij van de DMA. Op een gecachete
        // regio ziet de DMA hem pas na de push, en dan de hele line tegelijk.
        dev::write32(d.add(DES_STATUS), DESC_OWN);
        dev::push(d, DESC_SIZE as usize);
        self.tx_cur = (self.tx_cur + 1) % NUM_DESC;
        self.tx_dirty = true;
        self.stats.tx_frames += 1;
        Ok(())
    }

    /// Haalt één frame op, of `None` als de RX-ring leeg is. Een foutframe,
    /// een gesplitst frame of een onmogelijke lengte wordt geteld en
    /// teruggegeven zonder kopie; een frame groter dan `buf` wordt
    /// afgekapt.
    ///
    /// Gaf deze ronde alleen afgekeurde descriptors terug en is de ring nu
    /// leeg, dan valt de RX-poll-demand meteen: de pomp flusht alleen na
    /// een frame, en een RX-DMA die stilstond op een volle ring moet het
    /// horen.
    fn receive(&mut self, buf: &mut [u8]) -> Option<usize> {
        let got = self.receive_one(buf);
        if got.is_none() {
            self.kick_rx();
        }
        got
    }

    /// De poll-demands: één schrijf per ring per burst.
    fn flush(&mut self) {
        dev::mb();
        if self.tx_dirty {
            self.regs().tx_poll.write(1);
            self.tx_dirty = false;
            self.stats.doorbells += 1;
        }
        self.kick_rx();
    }

    fn mac(&self) -> Mac {
        self.mac
    }
}
