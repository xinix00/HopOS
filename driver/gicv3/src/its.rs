//! De GICv3 Interrupt Translation Service: van een MSI-schrijf naar een LPI.
//!
//! Een PCIe-device meldt zich met MSI-X door een woord (de EventID) naar
//! GITS_TRANSLATER te schrijven; de ITS kent aan de bus-kant zijn DeviceID
//! (de requester-id, via de IORT-afbeelding), zoekt in zijn tabellen welke
//! LPI daarbij hoort en naar welke redistributor (collectie) hij gaat. Dit
//! bezit:
//!
//! - de tabellen van de ITS (commandorij, device-tabel plat of in twee
//!   niveaus, collectie-tabel, de ITT per device) in één geheugenregio die
//!   het board geeft ([`MEM_LEN`], Normal non-cacheable);
//! - de LPI-configuratie- en pending-tabel van de redistributor (in
//!   dezelfde regio; het aanzetten zelf is [`crate::Gic::enable_lpis`]);
//! - de toewijzing DeviceID en EventID naar LPI (`MAPD`, `MAPTI`), en het
//!   aan- en uitzetten van één LPI (configuratiebyte plus `INV`).
//!
//! Niet van hier: welke DeviceID een device heeft (de IORT, `fw::acpi`), de
//! MSI-X-tabel van het device (`driver-pcie`), en de claim (dat blijft
//! ICC_IAR1 van [`crate::Gic`]: een LPI is daar gewoon een INTID vanaf
//! 8192).
//!
//! Eén collectie (ICID 0) op de redistributor van de kern-core: interrupts
//! zijn uitsluitend werk van de OS-core (`cpu::irq`), een app-core is nooit
//! een doel. Zo is het bij de SPI's (IRM = 0) en zo is het hier.
//!
//! De Go-kern had geen ITS: elke PCIe-NIC op UEFI pollde, of hing aan een
//! INTx-lijn die het board uit zijn hoofd kende (de O6N-tabel per
//! root-poort). De vorm hier volgt ARM IHI 0069 (hoofdstuk 5 en 6) en de
//! volgorde van Linux' `irq-gic-v3-its.c` (tabellen, CBASER, enable, MAPC,
//! dan per device MAPD, MAPTI, INV, SYNC).

use core::fmt;
use core::mem::offset_of;
use dev::{Pa, Reg};

/// De maat van de geheugenregio die het board de ITS geeft: 1 MB,
/// 64 KB-gealigneerd, Normal non-cacheable, alleen van deze driver.
pub const MEM_LEN: u64 = 0x10_0000;

/// De indeling van die regio, elk op 64 KB: de pending-tabel moet
/// 64 KB-gealigneerd, een tabel met pagina's van 64 KB ook.
const PEND_OFF: u64 = 0x0_0000;
const PROP_OFF: u64 = 0x1_0000;
const CMDQ_OFF: u64 = 0x2_0000;
const COLL_OFF: u64 = 0x3_0000;
const ITT_OFF: u64 = 0x4_0000;
const DEV_OFF: u64 = 0x5_0000;
/// De commandorij: 64 KB, 2048 commando's van 32 bytes.
const CMDQ_LEN: u64 = 0x1_0000;
/// Eén ITT per device, 4 KB: 32 events (EventID-bits 5) van hoogstens 16
/// bytes passen er ruim in (256-byte-alignment eist de spec).
const ITT_SLOT: u64 = 0x1000;
/// Hoeveel devices een ITT kunnen krijgen.
pub const MAX_DEVICES: usize = 16;
/// De EventID-bits per device: 32 MSI-X-vectoren is ruim voor NIC en NVMe.
const EVENT_BITS: u32 = 5;
/// Hoeveel LPI's deze driver uitdeelt (een per device-vector).
pub const MAX_LPIS: usize = 32;
/// De prioriteit van een LPI: die van de SPI's, plus bit 1 (RES1) en
/// Enable in bit 0.
const LPI_PRIO: u8 = 0xa0 | 0b10;

/// De ruimte voor de device-tabel: van [`DEV_OFF`] tot het einde. Plat als
/// hij helemaal past; anders twee niveaus, met het eerste niveau in de
/// eerste 64 KB en de pagina's van het tweede in de rest.
const DEV_LEN: u64 = MEM_LEN - DEV_OFF;
const L1_LEN: u64 = 0x1_0000;

const _: () = {
    assert!(PEND_OFF + crate::LPI_PEND_LEN <= PROP_OFF);
    assert!(PROP_OFF + crate::LPI_PROP_LEN <= CMDQ_OFF);
    assert!(CMDQ_OFF + CMDQ_LEN <= COLL_OFF);
    assert!(ITT_OFF + MAX_DEVICES as u64 * ITT_SLOT <= DEV_OFF);
};

/// Het ITS-controleframe (ARM IHI 0069, tabel 12-17).
#[repr(C)]
struct Gits {
    ctlr: Reg<u32>,
    iidr: Reg<u32>,
    typer: Reg<u64>,
    _r0: [u32; 28],
    cbaser: Reg<u64>,
    cwriter: Reg<u64>,
    creadr: Reg<u64>,
    _r1: [u64; 13],
    baser: [Reg<u64>; 8],
}

const _: () = {
    assert!(offset_of!(Gits, ctlr) == 0x0000);
    assert!(offset_of!(Gits, iidr) == 0x0004);
    assert!(offset_of!(Gits, typer) == 0x0008);
    assert!(offset_of!(Gits, cbaser) == 0x0080);
    assert!(offset_of!(Gits, cwriter) == 0x0088);
    assert!(offset_of!(Gits, creadr) == 0x0090);
    assert!(offset_of!(Gits, baser) == 0x0100);
};

/// GITS_TRANSLATER: in het tweede 64 KB-frame, op +0x40. Het adres dat in
/// de MSI-X-tabel van een device komt.
pub const TRANSLATER: u64 = 0x1_0040;

const CTLR_ENABLED: u32 = 1;
const CTLR_QUIESCENT: u32 = 1 << 31;
const TYPER_PHYSICAL: u64 = 1;
const TYPER_PTA: u64 = 1 << 19;
const BASER_VALID: u64 = 1 << 63;
const BASER_INDIRECT: u64 = 1 << 62;
/// InnerCache [61:59] = 0b001: Normal non-cacheable (de regio is NC).
const BASER_INNER_NC: u64 = 1 << 59;
/// CBASER heeft zijn InnerCache op dezelfde plek.
const CBASER_INNER_NC: u64 = 1 << 59;
const BASER_TYPE_DEVICES: u64 = 1;
const BASER_TYPE_COLLECTIONS: u64 = 4;
/// Hoe lang we op de ITS wachten (commando's, quiescent): ruim boven wat
/// een GIC-600 of -700 doet (microseconden), ver onder een hang.
const POLLS: u32 = 1 << 20;

/// De commando's (ARM IHI 0069, §5.13).
mod cmd {
    pub(super) const INT: u64 = 0x03;
    pub(super) const SYNC: u64 = 0x05;
    pub(super) const MAPD: u64 = 0x08;
    pub(super) const MAPC: u64 = 0x09;
    pub(super) const MAPTI: u64 = 0x0a;
    pub(super) const INV: u64 = 0x0c;
}

/// Waarom de ITS iets weigert.
#[derive(Debug, PartialEq, Eq, Clone, Copy)]
pub enum Error {
    /// Geen fysieke LPI's op deze ITS (GITS_TYPER.Physical).
    NotPhysical,
    /// De ITS werd niet quiescent na uitzetten.
    Busy,
    /// Een tabel kreeg geen paginamaat of adres die de ITS aannam.
    Table(&'static str),
    /// De commandorij liep niet leeg (`creadr` bleef staan).
    Timeout {
        /// Waar de ITS stond.
        creadr: u64,
        /// Waar wij schreven.
        cwriter: u64,
    },
    /// De ITS meldde een commandofout (CREADR.Stalled).
    Stalled {
        /// De offset van het commando.
        at: u64,
    },
    /// Een DeviceID die de tabel niet dekt.
    DeviceId(u32),
    /// Geen ITT of LPI meer vrij.
    Full,
    /// De ITS is (nog) niet opgezet.
    Down,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::NotPhysical => f.write_str("its: no physical LPIs (GITS_TYPER.Physical = 0)"),
            Self::Busy => f.write_str("its: not quiescent after disable"),
            Self::Table(what) => write!(f, "its: {what} table refused"),
            Self::Timeout { creadr, cwriter } => write!(
                f,
                "its: command queue stuck (CREADR {creadr:#x}, CWRITER {cwriter:#x})"
            ),
            Self::Stalled { at } => write!(f, "its: command at {at:#x} stalled the queue"),
            Self::DeviceId(id) => write!(f, "its: DeviceID {id:#x} outside the device table"),
            Self::Full => write!(
                f,
                "its: out of ITT slots ({MAX_DEVICES}) or LPIs ({MAX_LPIS})"
            ),
            Self::Down => f.write_str("its: not initialised"),
        }
    }
}

/// De `Result` van de ITS.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Eén commando: vier woorden van 64 bits.
pub type Command = [u64; 4];

/// `MAPD`: DeviceID `dev` krijgt de ITT op `itt` met `bits` EventID-bits.
#[must_use]
pub const fn mapd(dev: u32, bits: u32, itt: u64) -> Command {
    [
        cmd::MAPD | ((dev as u64) << 32),
        (bits.saturating_sub(1) & 0x1f) as u64,
        BASER_VALID | (itt & 0x000f_ffff_ffff_ff00),
        0,
    ]
}

/// `MAPC`: collectie `icid` gaat naar redistributor `rd` (het RDbase-veld:
/// PA >> 16 bij PTA = 1, anders het processornummer).
#[must_use]
pub const fn mapc(icid: u16, rd: u64) -> Command {
    [
        cmd::MAPC,
        0,
        BASER_VALID | ((rd << 16) & 0x000f_ffff_ffff_0000) | icid as u64,
        0,
    ]
}

/// `MAPTI`: event `ev` van `dev` wordt LPI `lpi` in collectie `icid`.
#[must_use]
pub const fn mapti(dev: u32, ev: u32, lpi: u32, icid: u16) -> Command {
    [
        cmd::MAPTI | ((dev as u64) << 32),
        ev as u64 | ((lpi as u64) << 32),
        icid as u64,
        0,
    ]
}

/// `INV`: de redistributor leest de configuratiebyte van dit event opnieuw.
#[must_use]
pub const fn inv(dev: u32, ev: u32) -> Command {
    [cmd::INV | ((dev as u64) << 32), ev as u64, 0, 0]
}

/// `INT`: de ITS doet alsof `dev` event `ev` schreef. De proef van de
/// ITS-kant zonder device: komt de LPI hierop wel en op de MSI niet, dan
/// zit de fout tussen device en ITS.
#[must_use]
pub const fn int(dev: u32, ev: u32) -> Command {
    [cmd::INT | ((dev as u64) << 32), ev as u64, 0, 0]
}

/// `SYNC`: alles vóór dit commando is bij redistributor `rd` aangekomen.
#[must_use]
pub const fn sync(rd: u64) -> Command {
    [cmd::SYNC, 0, (rd << 16) & 0x000f_ffff_ffff_0000, 0]
}

/// Hoe de device-tabel eruitziet.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
enum DevTable {
    /// Nog niet gezet (of er was geen device-BASER).
    None,
    /// Plat: DeviceID's `0..ids`.
    Flat {
        /// Hoeveel DeviceID's de tabel dekt.
        ids: u64,
    },
    /// Twee niveaus: een L1-entry per pagina van het tweede niveau.
    Indirect {
        /// De paginamaat.
        page: u64,
        /// De maat van een entry.
        es: u64,
        /// Hoeveel L1-entries er zijn.
        l1: u64,
    },
}

/// Eén toegewezen event: welk device, welk event, welke LPI.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
struct Route {
    dev: u32,
    ev: u32,
    lpi: u32,
}

/// Wat [`Its::init`] vond, voor de bootlog.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Describe {
    /// GITS_IIDR.
    pub iidr: u32,
    /// GITS_TYPER.
    pub typer: u64,
    /// DeviceID-bits.
    pub dev_bits: u32,
    /// Plat of in twee niveaus, en hoeveel ID's het eerste niveau dekt.
    pub indirect: bool,
    /// De paginamaat van de device-tabel.
    pub page: u64,
    /// Waar een device zijn MSI heen schrijft.
    pub doorbell: Pa,
}

impl fmt::Display for Describe {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "ITS IIDR {:#x} TYPER {:#x}, {} DeviceID bits, device table {} ({} KB pages), doorbell {:#x}",
            self.iidr,
            self.typer,
            self.dev_bits,
            if self.indirect { "two-level" } else { "flat" },
            self.page >> 10,
            self.doorbell.0
        )
    }
}

/// De ITS en zijn tabellen. Eén eigenaar: het board, op de kern-core, bij
/// boot en bij het bedraden van een device (`&mut self`).
///
/// # Invariants
///
/// `base` is een gemapt ITS-frame (128 KB) en `[mem, mem + MEM_LEN)` is
/// geheugen dat alleen deze driver en de GIC gebruiken, Normal-NC gemapt.
pub struct Its {
    base: Pa,
    mem: Pa,
    /// Waar het volgende commando komt (offset in de rij).
    cwriter: u64,
    /// Het RDbase-veld van de kern-core, voor MAPC en SYNC.
    rd: u64,
    ite: u64,
    dev_table: DevTable,
    /// De volgende vrije L2-pagina (index in de pool).
    l2_next: u64,
    /// De devices met een ITT.
    devices: [Option<u32>; MAX_DEVICES],
    routes: [Option<Route>; MAX_LPIS],
    up: bool,
}

impl Its {
    /// De ITS op `base` met zijn tabellen in `mem`.
    ///
    /// # Safety
    ///
    /// `base` is het GITS-frame uit de MADT, gemapt als Device, en
    /// `[mem, mem + MEM_LEN)` is 64 KB-gealigneerd, Normal-NC gemapt, en
    /// van niemand anders dan deze ITS en de redistributor van de kern-core,
    /// zolang het programma draait.
    #[must_use]
    pub const unsafe fn new(base: Pa, mem: Pa) -> Self {
        // INVARIANT: de voorwaarde hierboven.
        Self {
            base,
            mem,
            cwriter: 0,
            rd: 0,
            ite: 8,
            dev_table: DevTable::None,
            l2_next: 0,
            devices: [None; MAX_DEVICES],
            routes: [None; MAX_LPIS],
            up: false,
        }
    }

    fn g(&self) -> &'static Gits {
        // SAFETY: de invariant van `Its`.
        unsafe { dev::regs(self.base) }
    }

    /// De LPI-configuratietabel voor [`crate::Gic::enable_lpis`].
    #[must_use]
    pub const fn prop_table(&self) -> Pa {
        self.mem.add(PROP_OFF)
    }

    /// De pending-tabel voor [`crate::Gic::enable_lpis`].
    #[must_use]
    pub const fn pend_table(&self) -> Pa {
        self.mem.add(PEND_OFF)
    }

    /// Het adres dat een device met MSI(-X) beschrijft.
    #[must_use]
    pub const fn doorbell(&self) -> Pa {
        self.base.add(TRANSLATER)
    }

    /// Wist de tabellen van de ITS en de LPI-configuratie: tabellen die nul
    /// zijn, zijn leeg (geen geldige entries, alles uit). Vóór
    /// [`Its::init`]. De pending-tabel niet: die leest de redistributor
    /// zodra de LPI's aan staan (zie [`Its::clear_pending`]).
    pub fn clear(&self) {
        dev::clear(self.mem.add(PROP_OFF), (MEM_LEN - PROP_OFF) as usize);
        dev::mb();
    }

    /// Wist de pending-tabel. Alleen vóór [`crate::Gic::enable_lpis`]:
    /// daarna is hij van de redistributor.
    pub fn clear_pending(&self) {
        dev::clear(self.mem.add(PEND_OFF), crate::LPI_PEND_LEN as usize);
        dev::mb();
    }

    /// Zet de ITS op voor de redistributor `rd_pa` (RD_base van de
    /// kern-core) met GICR_TYPER `rd_typer`: uit, tabellen, commandorij,
    /// aan, en collectie 0 naar die redistributor.
    pub fn init(&mut self, rd_pa: Pa, rd_typer: u64) -> Result<Describe> {
        let g = self.g();
        let typer = g.typer.read();
        if typer & TYPER_PHYSICAL == 0 {
            return Err(Error::NotPhysical);
        }
        // Uit en quiescent: alleen dan mogen BASER en CBASER veranderen (een
        // vorige kern, of firmware die hem aanliet).
        g.ctlr.update(|c| c & !CTLR_ENABLED);
        let mut n = 0;
        while g.ctlr.read() & CTLR_QUIESCENT == 0 {
            n += 1;
            if n > POLLS {
                return Err(Error::Busy);
            }
        }
        self.ite = ((typer >> 4) & 0xf) + 1;
        let dev_bits = (((typer >> 13) & 0x1f) + 1) as u32;
        self.rd = if typer & TYPER_PTA != 0 {
            rd_pa.0 >> 16
        } else {
            (rd_typer >> 8) & 0xffff
        };
        // De commandorij: CBASER.Size is pagina's van 4 KB min één.
        g.cbaser.write(
            BASER_VALID
                | CBASER_INNER_NC
                | (self.mem.add(CMDQ_OFF).0 & 0x000f_ffff_ffff_f000)
                | (CMDQ_LEN / 0x1000 - 1),
        );
        g.cwriter.write(0);
        self.cwriter = 0;
        for i in 0..8 {
            self.setup_baser(i, dev_bits)?;
        }
        dev::mb();
        g.ctlr.update(|c| c | CTLR_ENABLED);
        dev::mb();
        self.up = true;
        self.run(&[mapc(0, self.rd), sync(self.rd)])?;
        let (indirect, page) = match self.dev_table {
            DevTable::Indirect { page, .. } => (true, page),
            _ => (false, self.dev_page()),
        };
        Ok(Describe {
            iidr: g.iidr.read(),
            typer,
            dev_bits,
            indirect,
            page,
            doorbell: self.doorbell(),
        })
    }

    fn dev_page(&self) -> u64 {
        match self.dev_table {
            DevTable::Indirect { page, .. } => page,
            _ => 0x1000,
        }
    }

    /// Eén GITS_BASER: de device-tabel of de collectie-tabel, elk op de
    /// kleinste paginamaat die de ITS aanneemt (4, 16 of 64 KB: de
    /// Page_Size-bits zijn op sommige GIC's vast, en dan leest de schrijf
    /// iets anders terug).
    fn setup_baser(&mut self, i: usize, dev_bits: u32) -> Result {
        let Some(reg) = self.g().baser.get(i) else {
            return Ok(());
        };
        let v = reg.read();
        let ty = (v >> 56) & 7;
        let es = ((v >> 48) & 0x1f) + 1;
        let (off, what) = match ty {
            BASER_TYPE_DEVICES => (DEV_OFF, "device"),
            BASER_TYPE_COLLECTIONS => (COLL_OFF, "collection"),
            _ => return Ok(()),
        };
        for (psz, bits) in [(0x1000u64, 0u64), (0x4000, 1), (0x1_0000, 2)] {
            let (pages, indirect, table) = if ty == BASER_TYPE_DEVICES {
                let ids = 1u64 << dev_bits;
                let flat = ids.saturating_mul(es);
                if flat <= DEV_LEN {
                    (
                        flat.div_ceil(psz),
                        false,
                        DevTable::Flat {
                            ids: DEV_LEN / es.max(1),
                        },
                    )
                } else {
                    let per = (psz / es).max(1);
                    let l1 = ids.div_ceil(per).min(L1_LEN / 8);
                    (
                        (l1 * 8).div_ceil(psz),
                        true,
                        DevTable::Indirect { page: psz, es, l1 },
                    )
                }
            } else {
                (1, false, DevTable::None)
            };
            let pages = pages.clamp(1, 256);
            let want = BASER_VALID
                | if indirect { BASER_INDIRECT } else { 0 }
                | BASER_INNER_NC
                | (ty << 56)
                | ((es - 1) << 48)
                | (self.mem.add(off).0 & 0x0000_ffff_ffff_f000)
                | (bits << 8)
                | (pages - 1);
            reg.write(want);
            let got = reg.read();
            let psz_ok = (got >> 8) & 3 == bits;
            let ind_ok = !indirect || got & BASER_INDIRECT != 0;
            if psz_ok && ind_ok {
                if ty == BASER_TYPE_DEVICES {
                    self.dev_table = match table {
                        DevTable::Flat { .. } => DevTable::Flat {
                            ids: (pages * psz) / es.max(1),
                        },
                        t => t,
                    };
                }
                return Ok(());
            }
        }
        Err(Error::Table(what))
    }

    /// Schrijft de commando's in de rij, schuift CWRITER op en wacht tot de
    /// ITS ze gelezen heeft (CREADR = CWRITER).
    fn run(&mut self, cmds: &[Command]) -> Result {
        if !self.up {
            return Err(Error::Down);
        }
        let g = self.g();
        for c in cmds {
            let at = self.mem.add(CMDQ_OFF + self.cwriter);
            for (k, w) in c.iter().enumerate() {
                dev::write64(at.add(8 * k as u64), *w);
            }
            self.cwriter = (self.cwriter + 32) % CMDQ_LEN;
        }
        dev::mb();
        g.cwriter.write(self.cwriter);
        let mut n = 0;
        loop {
            let r = g.creadr.read();
            if r & 1 != 0 {
                return Err(Error::Stalled { at: r & !0x1f });
            }
            if r & 0xf_ffe0 == self.cwriter {
                return Ok(());
            }
            n += 1;
            if n > POLLS {
                return Err(Error::Timeout {
                    creadr: r,
                    cwriter: self.cwriter,
                });
            }
        }
    }

    /// Zorgt dat DeviceID `dev` in de device-tabel past; in twee niveaus
    /// krijgt zijn L1-entry zo nodig een (al gewiste) pagina uit de pool.
    fn cover(&mut self, dev: u32) -> Result {
        match self.dev_table {
            DevTable::None => Err(Error::Table("device")),
            DevTable::Flat { ids } => {
                if u64::from(dev) < ids {
                    Ok(())
                } else {
                    Err(Error::DeviceId(dev))
                }
            }
            DevTable::Indirect { page, es, l1 } => {
                let idx = u64::from(dev) / (page / es).max(1);
                if idx >= l1 {
                    return Err(Error::DeviceId(dev));
                }
                let entry = self.mem.add(DEV_OFF + idx * 8);
                if dev::read64(entry) & BASER_VALID != 0 {
                    return Ok(());
                }
                let pool = DEV_OFF + L1_LEN;
                let at = pool.next_multiple_of(page) + self.l2_next * page;
                if at + page > MEM_LEN {
                    return Err(Error::Full);
                }
                self.l2_next += 1;
                dev::write64(entry, BASER_VALID | self.mem.add(at).0);
                dev::mb();
                Ok(())
            }
        }
    }

    /// Geeft event `ev` van DeviceID `dev` een eigen LPI, en zet hem aan:
    /// `MAPD` (eerste keer voor dit device), `MAPTI`, de configuratiebyte,
    /// `INV`, `SYNC`. Geeft de LPI (de INTID die ICC_IAR1 straks meldt).
    pub fn route(&mut self, dev: u32, ev: u32) -> Result<u32> {
        if !self.up {
            return Err(Error::Down);
        }
        if ev >= 1 << EVENT_BITS {
            return Err(Error::Full);
        }
        if let Some(r) = self
            .routes
            .iter()
            .flatten()
            .find(|r| r.dev == dev && r.ev == ev)
        {
            return Ok(r.lpi);
        }
        let slot = self
            .routes
            .iter()
            .position(Option::is_none)
            .ok_or(Error::Full)?;
        let lpi = crate::FIRST_LPI + slot as u32;
        let mut cmds: [Command; 5] = [[0; 4]; 5];
        let mut n = 0;
        if !self.devices.contains(&Some(dev)) {
            self.cover(dev)?;
            let d = self
                .devices
                .iter()
                .position(Option::is_none)
                .ok_or(Error::Full)?;
            let itt = self.mem.add(ITT_OFF + d as u64 * ITT_SLOT);
            debug_assert!(self.ite << EVENT_BITS <= ITT_SLOT);
            if let Some(s) = self.devices.get_mut(d) {
                *s = Some(dev);
            }
            if let Some(c) = cmds.get_mut(n) {
                *c = mapd(dev, EVENT_BITS, itt.0);
                n += 1;
            }
        }
        self.set_prop(lpi, true);
        for c in [mapti(dev, ev, lpi, 0), inv(dev, ev), sync(self.rd)] {
            if let Some(s) = cmds.get_mut(n) {
                *s = c;
                n += 1;
            }
        }
        self.run(cmds.get(..n).unwrap_or(&[]))?;
        if let Some(s) = self.routes.get_mut(slot) {
            *s = Some(Route { dev, ev, lpi });
        }
        Ok(lpi)
    }

    /// De configuratiebyte van `lpi`: prioriteit en Enable.
    fn set_prop(&self, lpi: u32, on: bool) {
        let at = self
            .prop_table()
            .add(u64::from(lpi.saturating_sub(crate::FIRST_LPI)));
        dev::write8(at, LPI_PRIO | u8::from(on));
        dev::mb();
    }

    /// Zet een toegewezen LPI aan of uit (configuratiebyte, `INV`,
    /// `SYNC`). Een LPI die niet van ons is: [`Error::Full`] niet, maar
    /// `Ok(false)`.
    pub fn enable(&mut self, lpi: u32, on: bool) -> Result<bool> {
        let Some(r) = self.routes.iter().flatten().find(|r| r.lpi == lpi).copied() else {
            return Ok(false);
        };
        self.set_prop(lpi, on);
        self.run(&[inv(r.dev, r.ev), sync(self.rd)])?;
        Ok(true)
    }

    /// De LPI van event `ev` van `dev`, als `route` hem in dit kernleven
    /// toewees (dan liepen `MAPD`, `MAPTI`, `INV` en `SYNC` zonder fout).
    #[must_use]
    pub fn routed(&self, dev: u32, ev: u32) -> Option<u32> {
        self.routes
            .iter()
            .flatten()
            .find(|r| r.dev == dev && r.ev == ev)
            .map(|r| r.lpi)
    }

    /// Vuurt een toegewezen event vanuit de ITS zelf (`INT`, `SYNC`).
    /// `Ok(false)` als het event niet van ons is.
    pub fn fire(&mut self, dev: u32, ev: u32) -> Result<bool> {
        if self.routed(dev, ev).is_none() {
            return Ok(false);
        }
        self.run(&[int(dev, ev), sync(self.rd)])?;
        Ok(true)
    }

    /// GITS_CTLR, CREADR en CWRITER, voor een diagnoseregel.
    #[must_use]
    pub fn state(&self) -> (u32, u64, u64) {
        let g = self.g();
        (g.ctlr.read(), g.creadr.read(), g.cwriter.read())
    }

    /// Is `lpi` een LPI die deze ITS uitdeelde?
    #[must_use]
    pub fn owns(&self, lpi: u32) -> bool {
        self.routes.iter().flatten().any(|r| r.lpi == lpi)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Nep-ITS: het frame (128 KB) en de regio als u64-vectoren.
    struct Fake {
        frame: Vec<u64>,
        mem: Vec<u64>,
    }

    impl Fake {
        fn new(typer: u64, basers: &[u64]) -> Self {
            let mut f = Self {
                frame: vec![0; 0x2_0000 / 8],
                // Eén extra 64 KB om te kunnen alignen.
                mem: vec![0; (MEM_LEN as usize + 0x1_0000) / 8],
            };
            let base = f.base();
            dev::write64(base.add(8), typer);
            // Quiescent staat al, zoals bij een ITS die uit staat.
            dev::write32(base, CTLR_QUIESCENT);
            for (i, b) in basers.iter().enumerate() {
                dev::write64(base.add(0x100 + 8 * i as u64), *b);
            }
            f
        }
        fn base(&mut self) -> Pa {
            Pa(self.frame.as_mut_ptr() as usize as u64)
        }
        fn mem(&mut self) -> Pa {
            Pa((self.mem.as_mut_ptr() as usize as u64).next_multiple_of(0x1_0000))
        }
    }

    /// TYPER: Physical, ITT-entry 8 bytes, 16 DeviceID-bits, PTA = 0.
    const TYPER: u64 = TYPER_PHYSICAL | (7 << 4) | (15 << 13);

    #[test]
    fn commands_are_encoded_like_the_spec() {
        assert_eq!(
            mapd(0x10, 5, 0x5004_0000),
            [0x8 | (0x10 << 32), 4, BASER_VALID | 0x5004_0000, 0]
        );
        assert_eq!(mapc(0, 3), [0x9, 0, BASER_VALID | (3 << 16), 0]);
        assert_eq!(
            mapti(0x10, 2, 8193, 0),
            [0xa | (0x10 << 32), 2 | (8193 << 32), 0, 0]
        );
        assert_eq!(inv(0x10, 2), [0xc | (0x10 << 32), 2, 0, 0]);
        assert_eq!(sync(0x80a), [0x5, 0, 0x80a << 16, 0]);
        assert_eq!(int(0x10, 2), [0x3 | (0x10 << 32), 2, 0, 0]);
        // Een ITT-adres onder 256 bytes wordt afgekapt: de spec eist die
        // alignment, dus de lage bits bestaan niet.
        assert_eq!(mapd(1, 1, 0x1234)[2], BASER_VALID | 0x1200);
    }

    #[test]
    fn baser_gets_a_flat_device_table_and_one_collection_page() {
        let dev_baser = (BASER_TYPE_DEVICES << 56) | (7 << 48);
        let coll_baser = (BASER_TYPE_COLLECTIONS << 56) | (7 << 48);
        let mut f = Fake::new(TYPER, &[dev_baser, coll_baser]);
        let (base, mem) = (f.base(), f.mem());
        // SAFETY: nep-frame en nep-regio leven de hele test.
        let mut its = unsafe { Its::new(base, mem) };
        // De nep-rij loopt nooit leeg: init komt tot MAPC en wacht dan.
        let e = its.init(Pa(0x0808_0000), 0x0100).unwrap_err();
        assert!(matches!(e, Error::Timeout { .. }), "{e:?}");
        let d = dev::read64(base.add(0x100));
        assert_ne!(d & BASER_VALID, 0);
        assert_eq!(d & BASER_INDIRECT, 0);
        // 16 bits maal 8 bytes = 512 KB = 128 pagina's van 4 KB.
        assert_eq!(d & 0xff, 127);
        assert_eq!(d & 0x0000_ffff_ffff_f000, mem.add(DEV_OFF).0);
        let c = dev::read64(base.add(0x108));
        assert_eq!(c & 0xff, 0);
        assert_eq!(c & 0x0000_ffff_ffff_f000, mem.add(COLL_OFF).0);
        // CBASER: 16 pagina's, de rij, geldig.
        let cb = dev::read64(base.add(0x80));
        assert_eq!(cb & 0xff, 15);
        assert_eq!(cb & 0x000f_ffff_ffff_f000, mem.add(CMDQ_OFF).0);
        // MAPC stond als eerste in de rij, met het processornummer (PTA 0)
        // uit GICR_TYPER[23:8] = 1.
        assert_eq!(dev::read64(mem.add(CMDQ_OFF)), 0x9);
        assert_eq!(dev::read64(mem.add(CMDQ_OFF + 16)), BASER_VALID | (1 << 16));
        assert_eq!(dev::read64(base.add(0x88)), 64);
    }

    #[test]
    fn a_wide_device_space_goes_two_level() {
        // 24 DeviceID-bits: plat zou 128 MB zijn.
        let typer = TYPER_PHYSICAL | (7 << 4) | (23 << 13) | TYPER_PTA;
        let dev_baser = (BASER_TYPE_DEVICES << 56) | (7 << 48);
        let mut f = Fake::new(typer, &[dev_baser]);
        let (base, mem) = (f.base(), f.mem());
        // SAFETY: zie hierboven.
        let mut its = unsafe { Its::new(base, mem) };
        let _ = its.init(Pa(0x0808_0000), 0);
        let d = dev::read64(base.add(0x100));
        assert_ne!(d & BASER_INDIRECT, 0);
        assert!(matches!(
            its.dev_table,
            DevTable::Indirect {
                page: 0x1000,
                es: 8,
                ..
            }
        ));
        // PTA = 1: het RDbase-veld is het adres >> 16.
        assert_eq!(its.rd, 0x0808);
        // Een DeviceID krijgt een L2-pagina in zijn L1-entry.
        its.cover(0x3_0010).unwrap();
        let idx = 0x3_0010u64 / 512;
        let l1 = dev::read64(mem.add(DEV_OFF + idx * 8));
        assert_ne!(l1 & BASER_VALID, 0);
        assert_eq!(l1 & !BASER_VALID, mem.add(DEV_OFF + L1_LEN).0);
        // Een tweede in dezelfde pagina hergebruikt hem.
        its.cover(0x3_0011).unwrap();
        assert_eq!(its.l2_next, 1);
    }

    #[test]
    fn a_route_programs_itt_prop_and_commands() {
        let dev_baser = (BASER_TYPE_DEVICES << 56) | (7 << 48);
        let mut f = Fake::new(TYPER, &[dev_baser]);
        let (base, mem) = (f.base(), f.mem());
        // SAFETY: zie hierboven.
        let mut its = unsafe { Its::new(base, mem) };
        let _ = its.init(Pa(0), 0);
        // De nep-ITS leest niets: doe alsof hij bij is, zodat `route` zijn
        // commando's kwijt kan en we ze kunnen lezen.
        let at = its.cwriter;
        dev::write64(base.add(0x90), at);
        let e = its.route(0x10, 0).unwrap_err();
        assert!(matches!(e, Error::Timeout { .. }));
        // De configuratiebyte van LPI 8192 staat aan.
        assert_eq!(dev::read8(its.prop_table()), LPI_PRIO | 1);
        // MAPD, MAPTI, INV, SYNC na de MAPC en SYNC van init.
        let q = |i: u64, w: u64| dev::read64(mem.add(CMDQ_OFF + at + 32 * i + 8 * w));
        assert_eq!(q(0, 0), 0x8 | (0x10 << 32));
        assert_eq!(q(0, 2), BASER_VALID | mem.add(ITT_OFF).0);
        assert_eq!(q(1, 0), 0xa | (0x10 << 32));
        assert_eq!(q(1, 1), 8192 << 32);
        assert_eq!(q(2, 0), 0xc | (0x10 << 32));
        assert_eq!(q(3, 0), 0x5);
        assert_eq!(dev::read64(base.add(0x88)), at + 4 * 32);
    }

    #[test]
    fn no_physical_lpis_is_refused() {
        let mut f = Fake::new(0, &[]);
        let (base, mem) = (f.base(), f.mem());
        // SAFETY: zie hierboven.
        let mut its = unsafe { Its::new(base, mem) };
        assert_eq!(its.init(Pa(0), 0).unwrap_err(), Error::NotPhysical);
        assert_eq!(its.route(1, 0).unwrap_err(), Error::Down);
    }
}
