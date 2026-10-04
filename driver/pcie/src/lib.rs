//! PCIe via ECAM: enumeratie, BAR's lezen en toewijzen, bus-master,
//! capabilities en MSI-X.
//!
//! De Rust-vorm van `OLD/metal/driver/pcie`: een [`Config`]-venster (op
//! ijzer [`Ecam`], in de tests een nep-config-space), een [`Function`] per
//! gevonden (bus, device, functie), en de handelingen erop. Wat deze crate
//! niet bezit: het adres van het venster (dat levert het board, uit de ACPI
//! MCFG of een board-constante) en de mapping ervan (de identity map van
//! het board).
//!
//! Twee soorten fabrics, en de Go-kern leerde het verschil op ijzer:
//!
//! - **Door de firmware geconfigureerd** (UEFI/ACPI: EDK2 op QEMU, de
//!   Ampere Altra, de O6N onder zijn UEFI): de busnummers staan in de
//!   bridges en de BAR's zijn toegewezen. [`walk`] leest alleen;
//!   [`Function::bar`] geeft wat de firmware koos.
//! - **Kaal** (wij booten zonder firmware-hulp, de Pi 5): niemand wees iets
//!   toe. [`Function::assign_bars`] deelt BAR's uit een [`MmioWindow`] van
//!   het board. Een bus-walk die zelf busnummers programmeert bestaat bewust
//!   nog niet (net als in Go): die komt met silicium om hem op te bewijzen.
//!
//! Config-space is onvertrouwde invoer: een capability-lijst die in een
//! kring wijst of een bridge die naar een al geziene bus verwijst, eindigt
//! de lus in plaats van hem te laten draaien.

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

use bounded::BoundedVec;
use core::fmt;
use dev::Pa;

#[cfg(test)]
mod tests;

/// De registers van de config-header (type 0 en type 1), als byte-offsets.
pub mod reg {
    /// Vendor-ID (15:0) en Device-ID (31:16).
    pub const ID: u16 = 0x00;
    /// Command (15:0) en Status (31:16).
    pub const COMMAND: u16 = 0x04;
    /// Revisie (7:0) en klassecode (31:8).
    pub const CLASS: u16 = 0x08;
    /// Cacheline, latency, headertype (23:16), BIST.
    pub const HEADER: u16 = 0x0c;
    /// De eerste BAR; BAR `i` staat op `BAR0 + 4 * i`.
    pub const BAR0: u16 = 0x10;
    /// Type 1: primary (7:0), secondary (15:8), subordinate (23:16).
    pub const BUS_NUMBERS: u16 = 0x18;
    /// De kop van de capability-lijst (7:0).
    pub const CAP_PTR: u16 = 0x34;
}

/// Command: I/O-decode.
pub const CMD_IO: u16 = 1 << 0;
/// Command: memory-decode.
pub const CMD_MEM: u16 = 1 << 1;
/// Command: bus-master (DMA).
pub const CMD_MASTER: u16 = 1 << 2;
/// Command: INTx uit (verplicht zodra MSI of MSI-X de lijn is).
pub const CMD_INTX_DISABLE: u16 = 1 << 10;
/// Status (in het hoge half van [`reg::COMMAND`]): er is een capability-lijst.
const STATUS_CAP_LIST: u32 = 1 << (16 + 4);

/// Capability-ID: vendor-specifiek (virtio-pci zet er zijn structuren in).
pub const CAP_VENDOR: u8 = 0x09;
/// Capability-ID: PCI Express.
pub const CAP_PCIE: u8 = 0x10;
/// Capability-ID: MSI-X.
pub const CAP_MSIX: u8 = 0x11;

/// Het meeste aantal capabilities dat een lijst mag hebben voor we hem een
/// kring noemen: 192 bytes vanaf 0x40, elk minstens 4 bytes.
const CAP_WALK_MAX: usize = 48;

/// Waarom een PCIe-handeling weigert.
#[derive(Debug, PartialEq, Eq)]
pub enum Error {
    /// Het MMIO-venster heeft geen plek voor een BAR van deze maat.
    NoSpace {
        /// De gevraagde maat.
        size: u64,
    },
    /// Een 64-bit BAR op de laatste plek, of een BAR-index buiten de header.
    BadBar {
        /// Het adres van de functie.
        bdf: Bdf,
        /// De BAR-index.
        idx: u8,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::NoSpace { size } => write!(f, "pcie: no window space for a {size:#x}-byte BAR"),
            Self::BadBar { bdf, idx } => write!(f, "pcie: {bdf} has no usable BAR {idx}"),
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Een (bus, device, functie)-adres.
#[derive(Copy, Clone, PartialEq, Eq, PartialOrd, Ord, Hash, Debug, Default)]
pub struct Bdf {
    /// De bus, 0 tot en met 255.
    pub bus: u8,
    /// Het device op de bus, 0 tot en met 31.
    pub dev: u8,
    /// De functie, 0 tot en met 7.
    pub func: u8,
}

impl Bdf {
    /// Een adres, of `None` als device of functie buiten het bereik valt.
    #[must_use]
    pub const fn new(bus: u8, dev: u8, func: u8) -> Option<Self> {
        if dev >= 32 || func >= 8 {
            return None;
        }
        Some(Self { bus, dev, func })
    }

    /// De offset van deze functie in een ECAM-venster dat op bus 0 begint:
    /// `bus << 20 | dev << 15 | func << 12`.
    #[must_use]
    pub const fn ecam_offset(self) -> u64 {
        ((self.bus as u64) << 20)
            | (((self.dev & 31) as u64) << 15)
            | (((self.func & 7) as u64) << 12)
    }
}

impl fmt::Display for Bdf {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{:02x}:{:02x}.{:x}", self.bus, self.dev, self.func)
    }
}

/// Een config-space-venster: wie het heeft, kan elke functie erin lezen en
/// schrijven.
///
/// `off` is een byte-offset binnen de 4 KB van de functie, gealigneerd op
/// de maat van de toegang. Een functie die er niet is, leest als alle enen
/// (zoals op de bus) en slikt schrijven.
pub trait Config {
    /// Leest een dword.
    fn read32(&self, bdf: Bdf, off: u16) -> u32;
    /// Schrijft een dword.
    fn write32(&self, bdf: Bdf, off: u16, v: u32);
    /// Schrijft een woord. Nodig voor Command en de message-control van
    /// MSI-X: een dword-schrijf zou de RW1C-bits van Status wissen.
    fn write16(&self, bdf: Bdf, off: u16, v: u16);

    /// Leest een woord (via het dword eromheen).
    fn read16(&self, bdf: Bdf, off: u16) -> u16 {
        (self.read32(bdf, off & !3) >> ((off & 2) * 8)) as u16
    }

    /// Leest een byte (via het dword eromheen).
    fn read8(&self, bdf: Bdf, off: u16) -> u8 {
        (self.read32(bdf, off & !3) >> ((off & 3) * 8)) as u8
    }
}

/// Het ECAM-venster van een host-bridge: 4 KB per functie, 1 MB per bus,
/// de basis is die van bus 0 (zo staat hij in de ACPI MCFG, en zo rekent
/// Linux' `pci_mcfg` ermee).
#[derive(Copy, Clone, Debug)]
pub struct Ecam {
    base: Pa,
    bus_start: u8,
    bus_end: u8,
}

impl Ecam {
    /// Het venster op `base` voor de bussen `bus_start..=bus_end`.
    ///
    /// # Safety
    ///
    /// `[base + (bus_start << 20), base + ((bus_end + 1) << 20))` is het
    /// ECAM-venster van een host-bridge, gemapt als Device, en blijft dat
    /// zolang het programma draait.
    #[must_use]
    pub const unsafe fn new(base: Pa, bus_start: u8, bus_end: u8) -> Self {
        Self {
            base,
            bus_start,
            bus_end,
        }
    }

    /// De basis (bus 0).
    #[must_use]
    pub const fn base(&self) -> Pa {
        self.base
    }

    /// De eerste en de laatste bus.
    #[must_use]
    pub const fn buses(&self) -> (u8, u8) {
        (self.bus_start, self.bus_end)
    }

    /// Het adres van `off` in `bdf`, of `None` buiten het venster.
    fn addr(&self, bdf: Bdf, off: u16) -> Option<Pa> {
        if bdf.bus < self.bus_start || bdf.bus > self.bus_end || off >= 4096 {
            return None;
        }
        Some(self.base.add(bdf.ecam_offset() + u64::from(off)))
    }
}

impl Config for Ecam {
    fn read32(&self, bdf: Bdf, off: u16) -> u32 {
        self.addr(bdf, off & !3).map_or(u32::MAX, dev::read32)
    }

    fn write32(&self, bdf: Bdf, off: u16, v: u32) {
        if let Some(pa) = self.addr(bdf, off & !3) {
            dev::write32(pa, v);
        }
    }

    fn write16(&self, bdf: Bdf, off: u16, v: u16) {
        if let Some(pa) = self.addr(bdf, off & !1) {
            dev::write16(pa, v);
        }
    }
}

/// Eén PCIe-functie zoals de enumeratie hem vond.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct Function {
    /// Het adres.
    pub bdf: Bdf,
    /// Vendor-ID.
    pub vendor: u16,
    /// Device-ID.
    pub device: u16,
    /// De 24-bit klassecode: basis (23:16), sub (15:8), prog-if (7:0).
    pub class: u32,
    /// De revisie.
    pub revision: u8,
    /// Headertype (6:0): 0 = endpoint, 1 = PCI-PCI-bridge.
    pub header: u8,
    /// Bit 7 van het headertype: het device heeft meer functies.
    pub multi: bool,
}

impl fmt::Display for Function {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "{} {:04x}:{:04x} class {:06x}",
            self.bdf, self.vendor, self.device, self.class
        )
    }
}

/// Wat een BAR decodeert.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Bar {
    /// Niet geïmplementeerd (maat 0), of de hoge helft van een 64-bit BAR.
    Unused,
    /// Een I/O-poortbereik.
    Io {
        /// De poortbasis.
        port: u32,
        /// De maat in bytes.
        size: u32,
    },
    /// Een memory-bereik.
    Mem {
        /// Het adres op de bus (op ARM gelijk aan het fysieke adres).
        addr: u64,
        /// De maat in bytes.
        size: u64,
        /// Een 64-bit BAR (beslaat twee plekken).
        is64: bool,
        /// Prefetchable.
        prefetch: bool,
    },
}

impl Bar {
    /// Het memory-adres, als dit een toegewezen memory-BAR is.
    #[must_use]
    pub const fn mem(&self) -> Option<(u64, u64)> {
        match *self {
            Self::Mem { addr, size, .. } if addr != 0 => Some((addr, size)),
            _ => None,
        }
    }
}

/// Een bereik waaruit het board BAR's uitdeelt op een kale fabric: elke
/// BAR op zijn eigen maat gealigneerd (zo eist de spec het).
#[derive(Copy, Clone, Debug)]
pub struct MmioWindow(dev::Bump);

impl MmioWindow {
    /// Het venster `[base, base + size)`.
    #[must_use]
    pub const fn new(base: u64, size: u64) -> Self {
        Self(dev::Bump::new(base, size))
    }

    /// Een naturel gealigneerd stuk van `size` bytes (een macht van twee).
    pub fn alloc(&mut self, size: u64) -> Result<u64> {
        let size = size.max(16);
        self.0.take(size, size).ok_or(Error::NoSpace { size })
    }
}

/// Plek in een BAR: de BAR-index en de offset erin (de MSI-X-tabel, de
/// virtio-structuren).
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct BarOffset {
    /// De BAR-index (0 tot en met 5).
    pub bar: u8,
    /// De offset in die BAR.
    pub off: u32,
}

/// De MSI-X-capability van een functie.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct Msix {
    /// De config-offset van de capability.
    pub cap: u16,
    /// Het aantal vectoren in de tabel.
    pub size: u16,
    /// Waar de tabel staat (16 bytes per vector).
    pub table: BarOffset,
}

/// De link van een PCIe-functie, voor de bootlog: een NIC op x1 Gen1 terwijl
/// het slot x4 Gen3 kan, is een meting waard.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct Link {
    /// De generatie (1 = 2,5 GT/s, 2 = 5, 3 = 8, 4 = 16, 5 = 32).
    pub speed: u8,
    /// Het aantal lanes.
    pub width: u8,
}

/// Leest één (bus, device, functie); `None` als er niets is.
pub fn probe<C: Config + ?Sized>(c: &C, bdf: Bdf) -> Option<Function> {
    let id = c.read32(bdf, reg::ID);
    if id == u32::MAX || id & 0xffff == 0 || id & 0xffff == 0xffff {
        return None;
    }
    let class = c.read32(bdf, reg::CLASS);
    let hdr = (c.read32(bdf, reg::HEADER) >> 16) as u8;
    Some(Function {
        bdf,
        vendor: id as u16,
        device: (id >> 16) as u16,
        class: class >> 8,
        revision: class as u8,
        header: hdr & 0x7f,
        multi: hdr & 0x80 != 0,
    })
}

/// Loopt een door de firmware geconfigureerde hiërarchie af vanaf
/// `start_bus`, en geeft elke functie (bridges en alle functies van een
/// multifunctie-device meegeteld) aan `visit`. `visit` geeft `false` om te
/// stoppen.
///
/// Puur lezen: de secondary-busnummers komen uit de bridges zelf. Een bus
/// wordt hooguit één keer bezocht, dus een kromme of dubbele
/// bridge-config maakt geen lus. Volgorde: eerst een hele bus, dan de
/// bussen achter zijn bridges.
pub fn walk<C: Config + ?Sized>(c: &C, start_bus: u8, mut visit: impl FnMut(&Function) -> bool) {
    let mut seen = [0u64; 4];
    let mut todo: BoundedVec<u8, 256> = BoundedVec::new();
    let _ = todo.push(start_bus);
    while let Some(bus) = todo.pop() {
        let (word, bit) = (usize::from(bus / 64), bus % 64);
        let Some(w) = seen.get_mut(word) else {
            continue;
        };
        if *w & (1 << bit) != 0 {
            continue;
        }
        *w |= 1 << bit;
        for f in functions_on(c, bus) {
            if f.header == 1 {
                let sec = (c.read32(f.bdf, reg::BUS_NUMBERS) >> 8) as u8;
                // Secondary 0 is een bridge die niemand configureerde.
                if sec != 0 {
                    // Vol kan niet: er zijn maar 256 bussen en elke bus
                    // komt hooguit eens per bridge; een dubbele valt bij
                    // de `seen`-toets af.
                    let _ = todo.push(sec);
                }
            }
            if !visit(&f) {
                return;
            }
        }
    }
}

/// Alle functies op één bus, in (device, functie)-volgorde.
fn functions_on<C: Config + ?Sized>(c: &C, bus: u8) -> impl Iterator<Item = Function> + '_ {
    (0u8..32).flat_map(move |dev| {
        let first = Bdf::new(bus, dev, 0).and_then(|b| probe(c, b));
        let more = first.is_some_and(|f| f.multi);
        let rest = (1u8..8)
            .filter(move |_| more)
            .filter_map(move |func| Bdf::new(bus, dev, func).and_then(|b| probe(c, b)));
        first.into_iter().chain(rest)
    })
}

/// De eerste functie waarop `is` ja zegt.
pub fn find<C: Config + ?Sized>(
    c: &C,
    start_bus: u8,
    mut is: impl FnMut(&Function) -> bool,
) -> Option<Function> {
    let mut hit = None;
    walk(c, start_bus, |f| {
        if is(f) {
            hit = Some(*f);
        }
        hit.is_none()
    });
    hit
}

impl Function {
    /// Een type-1 PCI-PCI-bridge (root-poort, switch-poort).
    #[must_use]
    pub const fn is_bridge(&self) -> bool {
        self.header == 1
    }

    /// Het aantal BAR's van deze header: 6 voor een endpoint, 2 voor een
    /// bridge.
    #[must_use]
    pub const fn bar_count(&self) -> u8 {
        if self.is_bridge() { 2 } else { 6 }
    }

    /// Het Command-register.
    pub fn command<C: Config + ?Sized>(&self, c: &C) -> u16 {
        c.read16(self.bdf, reg::COMMAND)
    }

    /// Zet bits in Command.
    pub fn set_command<C: Config + ?Sized>(&self, c: &C, bits: u16) {
        let cmd = self.command(c);
        c.write16(self.bdf, reg::COMMAND, cmd | bits);
    }

    /// Memory-decode en bus-mastering (DMA) aan. EDK2 laat bus-master bij
    /// ExitBootServices vaak uit staan: een driver die DMA doet, zet dit
    /// altijd zelf.
    pub fn enable<C: Config + ?Sized>(&self, c: &C) {
        self.set_command(c, CMD_MEM | CMD_MASTER);
        dev::mb();
    }

    /// Het ruwe adres van BAR `idx` zoals toegewezen (de lees-weg van
    /// Go's `BAR`), zonder maat en zonder iets te schrijven. 0 = niet
    /// toegewezen of geen memory-BAR.
    pub fn bar_addr<C: Config + ?Sized>(&self, c: &C, idx: u8) -> u64 {
        if idx >= self.bar_count() {
            return 0;
        }
        let off = reg::BAR0 + u16::from(idx) * 4;
        let lo = c.read32(self.bdf, off);
        if lo & 1 != 0 {
            return 0;
        }
        let mut addr = u64::from(lo & !0xf);
        if (lo >> 1) & 3 == 2 && idx + 1 < self.bar_count() {
            addr |= u64::from(c.read32(self.bdf, off + 4)) << 32;
        }
        addr
    }

    /// BAR `idx` met type en maat. De maat meten is de klassieke dans:
    /// alle enen schrijven, teruglezen, het oude adres terugzetten; met
    /// decode tijdelijk uit, zodat het device niet even op een onzinadres
    /// antwoordt.
    pub fn bar<C: Config + ?Sized>(&self, c: &C, idx: u8) -> Result<Bar> {
        if idx >= self.bar_count() {
            return Err(Error::BadBar { bdf: self.bdf, idx });
        }
        if self.is_upper_half(c, idx) {
            return Ok(Bar::Unused);
        }
        let off = reg::BAR0 + u16::from(idx) * 4;
        let lo = c.read32(self.bdf, off);
        let cmd = self.command(c);
        c.write16(self.bdf, reg::COMMAND, cmd & !(CMD_MEM | CMD_IO));
        let bar = self.decode_bar(c, idx, off, lo);
        c.write16(self.bdf, reg::COMMAND, cmd);
        bar
    }

    /// Is BAR `idx` de hoge helft van een 64-bit BAR ervoor? Dat volgt
    /// alleen uit de BAR's vanaf 0: de hoge helft zelf ziet eruit als een
    /// 32-bit BAR met alle adresbits beschrijfbaar.
    fn is_upper_half<C: Config + ?Sized>(&self, c: &C, idx: u8) -> bool {
        let mut i = 0u8;
        while i < idx {
            let lo = c.read32(self.bdf, reg::BAR0 + u16::from(i) * 4);
            if lo & 1 == 0 && (lo >> 1) & 3 == 2 {
                if i + 1 == idx {
                    return true;
                }
                i += 2;
            } else {
                i += 1;
            }
        }
        false
    }

    /// Het meetwerk van [`bar`](Self::bar), met decode al uit.
    fn decode_bar<C: Config + ?Sized>(&self, c: &C, idx: u8, off: u16, lo: u32) -> Result<Bar> {
        let bdf = self.bdf;
        c.write32(bdf, off, u32::MAX);
        let mask_lo = c.read32(bdf, off);
        c.write32(bdf, off, lo);
        if lo & 1 != 0 {
            let mask = mask_lo & !3;
            if mask == 0 {
                return Ok(Bar::Unused);
            }
            let size = (!mask).wrapping_add(1) & 0xffff;
            return Ok(Bar::Io {
                port: lo & !3,
                size,
            });
        }
        let is64 = (lo >> 1) & 3 == 2;
        let prefetch = lo & 8 != 0;
        let (addr, mask) = if is64 {
            if idx + 1 >= self.bar_count() {
                return Err(Error::BadBar { bdf, idx });
            }
            let hi = c.read32(bdf, off + 4);
            c.write32(bdf, off + 4, u32::MAX);
            let mask_hi = c.read32(bdf, off + 4);
            c.write32(bdf, off + 4, hi);
            (
                (u64::from(hi) << 32) | u64::from(lo & !0xf),
                (u64::from(mask_hi) << 32) | u64::from(mask_lo & !0xf),
            )
        } else {
            // Een 32-bit BAR: de hoge helft telt als "alle enen", zodat de
            // maat-rekensom dezelfde is.
            (
                u64::from(lo & !0xf),
                0xffff_ffff_0000_0000 | u64::from(mask_lo & !0xf),
            )
        };
        // Geen beschrijfbaar adresbit: niet geïmplementeerd.
        if mask & !0xffff_ffff_0000_0000 == 0 && (!is64 || mask == 0) {
            return Ok(Bar::Unused);
        }
        Ok(Bar::Mem {
            addr,
            size: (!mask).wrapping_add(1),
            is64,
            prefetch,
        })
    }

    /// Schrijft `addr` in memory-BAR `idx` (en in de hoge helft als het een
    /// 64-bit BAR is). Decode blijft zoals hij stond.
    pub fn set_bar<C: Config + ?Sized>(&self, c: &C, idx: u8, addr: u64) -> Result {
        if idx >= self.bar_count() {
            return Err(Error::BadBar { bdf: self.bdf, idx });
        }
        let off = reg::BAR0 + u16::from(idx) * 4;
        let lo = c.read32(self.bdf, off);
        if lo & 1 != 0 {
            return Err(Error::BadBar { bdf: self.bdf, idx });
        }
        c.write32(self.bdf, off, (addr as u32 & !0xf) | (lo & 0xf));
        if (lo >> 1) & 3 == 2 {
            if idx + 1 >= self.bar_count() {
                return Err(Error::BadBar { bdf: self.bdf, idx });
            }
            c.write32(self.bdf, off + 4, (addr >> 32) as u32);
        } else if addr >> 32 != 0 {
            return Err(Error::BadBar { bdf: self.bdf, idx });
        }
        Ok(())
    }

    /// Kale fabric: meet elke memory-BAR, wijst hem toe uit `win` en zet
    /// memory-decode aan. I/O-BAR's blijven leeg (ARM heeft geen
    /// I/O-ruimte). Geeft de BAR's zoals ze nu staan.
    pub fn assign_bars<C: Config + ?Sized>(&self, c: &C, win: &mut MmioWindow) -> Result<[Bar; 6]> {
        let mut out = [Bar::Unused; 6];
        let mut idx = 0u8;
        while idx < self.bar_count() {
            let bar = self.bar(c, idx)?;
            let step = match bar {
                Bar::Mem {
                    size,
                    is64,
                    prefetch,
                    ..
                } => {
                    let addr = win.alloc(size)?;
                    self.set_bar(c, idx, addr)?;
                    if let Some(slot) = out.get_mut(usize::from(idx)) {
                        *slot = Bar::Mem {
                            addr,
                            size,
                            is64,
                            prefetch,
                        };
                    }
                    if is64 { 2 } else { 1 }
                }
                _ => 1,
            };
            idx += step;
        }
        self.set_command(c, CMD_MEM);
        Ok(out)
    }

    /// De capability-lijst: `(id, offset)` per capability. Stopt na
    /// [`CAP_WALK_MAX`] stappen, zodat een kring in de lijst eindigt.
    pub fn caps<'a, C: Config + ?Sized>(&self, c: &'a C) -> Caps<'a, C> {
        let has = c.read32(self.bdf, reg::COMMAND) & STATUS_CAP_LIST != 0;
        let next = if has {
            u16::from(c.read8(self.bdf, reg::CAP_PTR) & 0xfc)
        } else {
            0
        };
        Caps {
            c,
            bdf: self.bdf,
            next,
            left: CAP_WALK_MAX,
        }
    }

    /// De offset van de eerste capability met `id`.
    pub fn find_cap<C: Config + ?Sized>(&self, c: &C, id: u8) -> Option<u16> {
        self.caps(c).find(|&(i, _)| i == id).map(|(_, off)| off)
    }

    /// De MSI-X-capability, als de functie er een heeft.
    pub fn msix<C: Config + ?Sized>(&self, c: &C) -> Option<Msix> {
        let cap = self.find_cap(c, CAP_MSIX)?;
        let ctrl = (c.read32(self.bdf, cap) >> 16) as u16;
        let table = c.read32(self.bdf, cap + 4);
        Some(Msix {
            cap,
            size: (ctrl & 0x7ff) + 1,
            table: BarOffset {
                bar: (table & 7) as u8,
                off: table & !7,
            },
        })
    }

    /// MSI-X aan (en de functiemaskering eraf) of uit. Aan zet ook INTx
    /// uit: een functie mag niet op twee manieren tegelijk melden.
    pub fn msix_enable<C: Config + ?Sized>(&self, c: &C, m: &Msix, on: bool) {
        const ENABLE: u16 = 1 << 15;
        const FUNC_MASK: u16 = 1 << 14;
        let ctrl = (c.read32(self.bdf, m.cap) >> 16) as u16;
        let ctrl = if on {
            (ctrl | ENABLE) & !FUNC_MASK
        } else {
            ctrl & !ENABLE
        };
        c.write16(self.bdf, m.cap + 2, ctrl);
        if on {
            self.set_command(c, CMD_INTX_DISABLE);
        }
    }

    /// Het fysieke adres van de MSI-X-tabel van `m`: het adres van zijn
    /// BAR plus de offset. `None` als die BAR niet toegewezen is.
    pub fn msix_table_addr<C: Config + ?Sized>(&self, c: &C, m: &Msix) -> Option<u64> {
        let bar = self.bar_addr(c, m.table.bar);
        (bar != 0).then(|| bar + u64::from(m.table.off))
    }

    /// De message-control van `m` zoals hij nu staat: bit 15 MSI-X aan, bit
    /// 14 de functiemaskering, [10:0] de tabelmaat min één.
    pub fn msix_control<C: Config + ?Sized>(&self, c: &C, m: &Msix) -> u16 {
        c.read16(self.bdf, m.cap + 2)
    }

    /// Het fysieke adres van de pending-bit-array van `m` (BIR en offset op
    /// cap + 8). `None` als die BAR niet toegewezen is.
    pub fn msix_pba_addr<C: Config + ?Sized>(&self, c: &C, m: &Msix) -> Option<u64> {
        let pba = c.read32(self.bdf, m.cap + 8);
        let bar = self.bar_addr(c, (pba & 7) as u8);
        (bar != 0).then(|| bar + u64::from(pba & !7))
    }

    /// De INTx-pin van de functie: 0 = INTA tot 3 = INTD, `None` als hij
    /// geen INTx heeft (register 0x3d is 0) of een onzinwaarde meldt.
    pub fn intx_pin<C: Config + ?Sized>(&self, c: &C) -> Option<u8> {
        match c.read8(self.bdf, 0x3d) {
            p @ 1..=4 => Some(p - 1),
            _ => None,
        }
    }

    /// De onderhandelde link, als dit een PCIe-functie is.
    pub fn link<C: Config + ?Sized>(&self, c: &C) -> Option<Link> {
        let cap = self.find_cap(c, CAP_PCIE)?;
        let status = (c.read32(self.bdf, cap + 0x10) >> 16) as u16;
        Some(Link {
            speed: (status & 0xf) as u8,
            width: ((status >> 4) & 0x3f) as u8,
        })
    }
}

/// De INTx-swizzle van een PCI-PCI-bridge: pin `pin` (0 = INTA) van
/// device `dev` achter de bridge komt op pin `(pin + dev) % 4` van de
/// bridge zelf (PCI-to-PCI Bridge Architecture, tabel 9-1; Linux'
/// `pci_swizzle_interrupt_pin`).
#[must_use]
pub const fn swizzle(pin: u8, dev: u8) -> u8 {
    (pin.wrapping_add(dev)) % 4
}

/// Hoe diep [`intx_at_root`] door bridges loopt: een root-poort en een
/// switch (upstream plus downstream) is drie; meer hebben onze boards niet.
const INTX_DEPTH: usize = 6;

/// De INTx-pin van `f` zoals hij op de root-bus `root` aankomt: `(device,
/// pin)` op die bus, na de swizzle door elke bridge op de weg. Dat paar
/// zoekt het board op in de `_PRT` van de host-bridge. `None` als de
/// functie geen INTx heeft of de weg niet te vinden is.
pub fn intx_at_root<C: Config + ?Sized>(c: &C, root: u8, f: &Function) -> Option<(u8, u8)> {
    let mut pin = f.intx_pin(c)?;
    let mut here = f.bdf;
    for _ in 0..INTX_DEPTH {
        if here.bus == root {
            return Some((here.dev, pin));
        }
        // De bridge waarachter `here` hangt: secondary = zijn bus. Alleen
        // bridges op of onder de root-bus tellen.
        let bridge = find(c, root, |b| {
            b.is_bridge() && (c.read32(b.bdf, reg::BUS_NUMBERS) >> 8) as u8 == here.bus
        })?;
        pin = swizzle(pin, here.dev);
        here = bridge.bdf;
    }
    None
}

/// De capability-lijst van één functie, zie [`Function::caps`].
pub struct Caps<'a, C: Config + ?Sized> {
    c: &'a C,
    bdf: Bdf,
    next: u16,
    left: usize,
}

impl<C: Config + ?Sized> Iterator for Caps<'_, C> {
    type Item = (u8, u16);

    fn next(&mut self) -> Option<(u8, u16)> {
        // Een capability ligt na de header (0x40) en binnen de 256 bytes
        // van de klassieke config-space.
        if self.left == 0 || self.next < 0x40 || self.next > 0xfc {
            return None;
        }
        self.left -= 1;
        let off = self.next;
        let w = self.c.read32(self.bdf, off);
        self.next = u16::from(((w >> 8) as u8) & 0xfc);
        Some((w as u8, off))
    }
}

/// De MSI-X-tabel van een functie in zijn BAR: 16 bytes per vector (adres
/// laag, adres hoog, data, vector-control).
#[derive(Copy, Clone, Debug)]
pub struct MsixTable {
    base: Pa,
    size: u16,
}

impl MsixTable {
    /// De tabel op `base` met `size` vectoren.
    ///
    /// # Safety
    ///
    /// `[base, base + 16 * size)` is de MSI-X-tabel van een functie
    /// (BAR-adres plus [`Msix::table`]), gemapt als Device, met
    /// memory-decode aan.
    #[must_use]
    pub const unsafe fn new(base: Pa, size: u16) -> Self {
        Self { base, size }
    }

    /// Zet vector `idx` op `addr`/`data` en haalt zijn masker eraf. Een
    /// index buiten de tabel is een no-op met `false`.
    pub fn set(&self, idx: u16, addr: u64, data: u32) -> bool {
        if idx >= self.size {
            return false;
        }
        let e = self.base.add(u64::from(idx) * 16);
        dev::write32(e.add(12), 1);
        dev::write32(e, addr as u32);
        dev::write32(e.add(4), (addr >> 32) as u32);
        dev::write32(e.add(8), data);
        dev::write32(e.add(12), 0);
        true
    }

    /// Vector `idx` zoals de functie hem teruggeeft: adres laag, adres
    /// hoog, data, vector-control. `None` buiten de tabel.
    #[must_use]
    pub fn get(&self, idx: u16) -> Option<[u32; 4]> {
        if idx >= self.size {
            return None;
        }
        let e = self.base.add(u64::from(idx) * 16);
        Some([0, 4, 8, 12].map(|o| dev::read32(e.add(o))))
    }
}
