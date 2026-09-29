//! virtio-blk over MMIO (QEMU virt): het blokapparaat onder hopfs.
//!
//! De vorm is die van de NVMe-driver uit de Go-kern
//! (`OLD/metal/driver/nvme`): één verzoek tegelijk, één DMA-buffer, en een
//! synchrone `read`/`write`/`flush` achter [`blkdev::BlockDevice`].
//! Eén in-flight verzoek is geen beperking maar de vorm: de eigenaar
//! (de hopfs-actor, `&mut self`) doet toch één ding tegelijk, en zo hoeft
//! de driver geen tags, geen rij en geen herordening te kennen.
//!
//! Het transport is virtio-mmio versie 2 (VERSION_1, virtio 1.2 §4.2.2),
//! zoals `driver-virtionet`: getypeerde registers, één split-virtqueue
//! (descriptortabel, avail- en used-ring) en een DMA-regio die het board
//! uitdeelt, buiten de kern-RAM en niet gecached gemapt. Een verzoek is een
//! keten van drie descriptors: de kop ([`ReqHdr`], het device leest), de
//! data (het device leest of schrijft) en de statusbyte (het device
//! schrijft).
//!
//! De driver pollt de used-ring: de BlockDevice-grens is synchroon, en een
//! verzoek op QEMU is klaar binnen de milliseconde (de IRQ-lijn komt met de
//! splitsing in PORT.md §3, als de I/O uit de metadata-actor gaat). Een
//! verzoek dat na [`REQUEST_TIMEOUT_NS`] niet klaar is, maakt de driver
//! dood: het device kan nog in de buffer schrijven, dus een volgend verzoek
//! zou andermans bytes zien. Dood is luid en blijvend (elke volgende call
//! faalt), nooit stil.

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
use core::mem::{offset_of, size_of};
use dev::{Pa, Reg};

/// De virtio-mmio-registers, versie 2, met de config van virtio-blk op
/// 0x100 (virtio 1.2 §5.2.4).
#[repr(C)]
struct Regs {
    magic: Reg<u32>,
    version: Reg<u32>,
    device_id: Reg<u32>,
    vendor_id: Reg<u32>,
    device_features: Reg<u32>,
    device_features_sel: Reg<u32>,
    _r0: [u32; 2],
    driver_features: Reg<u32>,
    driver_features_sel: Reg<u32>,
    _r1: [u32; 2],
    queue_sel: Reg<u32>,
    queue_num_max: Reg<u32>,
    queue_num: Reg<u32>,
    _r2: [u32; 2],
    queue_ready: Reg<u32>,
    _r3: [u32; 2],
    queue_notify: Reg<u32>,
    _r4: [u32; 3],
    interrupt_status: Reg<u32>,
    interrupt_ack: Reg<u32>,
    _r5: [u32; 2],
    status: Reg<u32>,
    _r6: [u32; 3],
    queue_desc_lo: Reg<u32>,
    queue_desc_hi: Reg<u32>,
    _r7: [u32; 2],
    queue_driver_lo: Reg<u32>,
    queue_driver_hi: Reg<u32>,
    _r8: [u32; 2],
    queue_device_lo: Reg<u32>,
    queue_device_hi: Reg<u32>,
    _r9: [u32; 21],
    config_generation: Reg<u32>,
    /// `capacity` in sectoren van 512 bytes, als twee woorden: een 64-bit
    /// lees op device-geheugen is hier gealigneerd, maar de spec noemt de
    /// config per veld en QEMU antwoordt per 32 bits.
    capacity_lo: Reg<u32>,
    capacity_hi: Reg<u32>,
    size_max: Reg<u32>,
    seg_max: Reg<u32>,
    _geometry: Reg<u32>,
    blk_size: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(Regs, magic) == 0x000);
    assert!(offset_of!(Regs, version) == 0x004);
    assert!(offset_of!(Regs, device_id) == 0x008);
    assert!(offset_of!(Regs, vendor_id) == 0x00c);
    assert!(offset_of!(Regs, device_features) == 0x010);
    assert!(offset_of!(Regs, device_features_sel) == 0x014);
    assert!(offset_of!(Regs, driver_features) == 0x020);
    assert!(offset_of!(Regs, driver_features_sel) == 0x024);
    assert!(offset_of!(Regs, queue_sel) == 0x030);
    assert!(offset_of!(Regs, queue_num_max) == 0x034);
    assert!(offset_of!(Regs, queue_num) == 0x038);
    assert!(offset_of!(Regs, queue_ready) == 0x044);
    assert!(offset_of!(Regs, queue_notify) == 0x050);
    assert!(offset_of!(Regs, interrupt_status) == 0x060);
    assert!(offset_of!(Regs, interrupt_ack) == 0x064);
    assert!(offset_of!(Regs, status) == 0x070);
    assert!(offset_of!(Regs, queue_desc_lo) == 0x080);
    assert!(offset_of!(Regs, queue_desc_hi) == 0x084);
    assert!(offset_of!(Regs, queue_driver_lo) == 0x090);
    assert!(offset_of!(Regs, queue_driver_hi) == 0x094);
    assert!(offset_of!(Regs, queue_device_lo) == 0x0a0);
    assert!(offset_of!(Regs, queue_device_hi) == 0x0a4);
    assert!(offset_of!(Regs, config_generation) == 0x0fc);
    assert!(offset_of!(Regs, capacity_lo) == 0x100);
    assert!(offset_of!(Regs, capacity_hi) == 0x104);
    assert!(offset_of!(Regs, size_max) == 0x108);
    assert!(offset_of!(Regs, seg_max) == 0x10c);
    assert!(offset_of!(Regs, blk_size) == 0x114);
};

/// Eén descriptor van de split-virtqueue (virtio 1.2 §2.7.5).
#[repr(C)]
struct Desc {
    addr: u64,
    len: u32,
    flags: u16,
    next: u16,
}

/// De kop van een blk-verzoek (`virtio_blk_req`, §5.2.6).
#[repr(C)]
struct ReqHdr {
    kind: u32,
    _reserved: u32,
    sector: u64,
}

/// Eén element van de used-ring.
#[repr(C)]
struct UsedElem {
    id: u32,
    len: u32,
}

const _: () = {
    assert!(size_of::<Desc>() == 16);
    assert!(offset_of!(Desc, addr) == 0);
    assert!(offset_of!(Desc, len) == 8);
    assert!(offset_of!(Desc, flags) == 12);
    assert!(offset_of!(Desc, next) == 14);
    assert!(size_of::<ReqHdr>() == 16);
    assert!(offset_of!(ReqHdr, kind) == 0);
    assert!(offset_of!(ReqHdr, sector) == 8);
    assert!(size_of::<UsedElem>() == 8);
    assert!(offset_of!(UsedElem, len) == 4);
};

/// "virt", little-endian.
const MAGIC: u32 = 0x7472_6976;
/// Het moderne transport.
const VERSION_2: u32 = 2;
/// DeviceID van een blokapparaat.
const DEVICE_BLK: u32 = 2;

const STATUS_ACK: u32 = 1 << 0;
const STATUS_DRIVER: u32 = 1 << 1;
const STATUS_DRIVER_OK: u32 = 1 << 2;
const STATUS_FEATURES_OK: u32 = 1 << 3;

/// VIRTIO_F_VERSION_1 (bit 32): bit 0 van het hoge feature-venster.
const FEAT_VERSION_1_HI: u32 = 1 << 0;
/// VIRTIO_BLK_F_RO: het device is alleen-lezen.
const FEAT_RO: u32 = 1 << 5;
/// VIRTIO_BLK_F_FLUSH: het device kent `VIRTIO_BLK_T_FLUSH` (een
/// write-back-cache; QEMU biedt hem standaard).
const FEAT_FLUSH: u32 = 1 << 9;

/// Descriptor-flag: de keten gaat verder op `next`.
const DESC_NEXT: u16 = 1;
/// Descriptor-flag: het device schrijft in deze buffer.
const DESC_WRITE: u16 = 2;

/// Verzoeksoorten.
const T_IN: u32 = 0;
const T_OUT: u32 = 1;
const T_FLUSH: u32 = 4;
/// De statusbyte bij succes; wat de driver er vooraf in zet, is geen
/// geldige status (`VIRTIO_BLK_S_OK`, `IOERR`, `UNSUPP` zijn 0, 1, 2).
const S_OK: u8 = 0;
const S_PENDING: u8 = 0xff;

/// De sectormaat van het protocol: `sector` telt altijd in 512 bytes.
pub const SECTOR: u64 = 512;
/// De queuegrootte: een verzoek is drie descriptors, en er is er één
/// tegelijk; vier is de kleinste macht van twee die dat draagt.
pub const QSIZE: u16 = 4;
/// De grootste transfer van één verzoek: de DMA-databuffer. 1 MiB, net als
/// de I/O-brok van de system-API (`MAX_IO_CHUNK`), zodat een app-write van
/// 1 MiB één verzoek is; hopfs bundelt aaneengesloten blokken tot deze maat.
pub const MAX_TRANSFER: usize = 1 << 20;
/// Hoe lang één verzoek mag duren. QEMU op een SSD doet 1 MiB in ruim onder
/// de 10 ms; vijf seconden is een device dat weg is, niet een trage schijf.
pub const REQUEST_TIMEOUT_NS: u64 = 5_000_000_000;

// De indeling van de DMA-regio. De ringen hebben elk hun eigen regel; de
// databuffer begint op een pagina.
const DESC_OFF: u64 = 0;
const AVAIL_OFF: u64 = 64;
const USED_OFF: u64 = 128;
const HDR_OFF: u64 = 256;
const STATUS_OFF: u64 = 272;
const DATA_OFF: u64 = 4096;
/// Wat de driver van de DMA-regio vraagt.
pub const DMA_NEED: u64 = DATA_OFF + MAX_TRANSFER as u64;

const _: () = {
    let q = QSIZE as u64;
    assert!(q.is_power_of_two() && q >= 3);
    assert!(DESC_OFF + q * size_of::<Desc>() as u64 <= AVAIL_OFF);
    // avail: flags, idx, ring[q], used_event.
    assert!(AVAIL_OFF + 6 + 2 * q <= USED_OFF);
    // used: flags, idx, ring[q], avail_event; 4-bytes gealigneerd.
    assert!(USED_OFF.is_multiple_of(4));
    assert!(USED_OFF + 6 + q * size_of::<UsedElem>() as u64 <= HDR_OFF);
    assert!(HDR_OFF.is_multiple_of(16) && HDR_OFF + size_of::<ReqHdr>() as u64 <= STATUS_OFF);
    assert!(STATUS_OFF < DATA_OFF && DATA_OFF.is_multiple_of(4096));
    assert!((MAX_TRANSFER as u64).is_multiple_of(SECTOR));
};

/// Waarom de driver weigert.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    /// Geen virtio-mmio op dit adres.
    NotVirtio,
    /// Een legacy transport (QEMU zonder `force-legacy=false`).
    Legacy,
    /// Wel virtio, maar geen blokapparaat.
    NotBlock(u32),
    /// Het device weigerde de features.
    FeaturesRefused,
    /// De queue is er niet, of kleiner dan [`QSIZE`].
    NoQueue(u32),
    /// De DMA-regio is te klein.
    DmaTooSmall {
        /// Wat nodig was.
        need: u64,
        /// Wat er was.
        have: u64,
    },
    /// Een lengte die geen veelvoud van de sector is, of buiten de schijf.
    Range {
        /// De eerste sector.
        sector: u64,
        /// Het aantal bytes.
        len: usize,
    },
    /// Het device is alleen-lezen.
    ReadOnly,
    /// Het device antwoordde niet binnen [`REQUEST_TIMEOUT_NS`]; de driver
    /// is vanaf nu dood.
    Timeout {
        /// De sector van het verzoek.
        sector: u64,
    },
    /// Het device meldde een fout (`IOERR` 1, `UNSUPP` 2).
    Status {
        /// De sector van het verzoek.
        sector: u64,
        /// De statusbyte.
        status: u8,
    },
    /// Een eerder verzoek liep af; niets gaat meer naar het device.
    Dead,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::NotVirtio => f.write_str("virtioblk: no virtio-mmio"),
            Self::Legacy => f.write_str("virtioblk: legacy transport (need version 2)"),
            Self::NotBlock(id) => write!(f, "virtioblk: device id {id} is not a block device"),
            Self::FeaturesRefused => f.write_str("virtioblk: device refused the features"),
            Self::NoQueue(n) => write!(f, "virtioblk: queue 0 offers {n} entries, need {QSIZE}"),
            Self::DmaTooSmall { need, have } => {
                write!(f, "virtioblk: DMA region too small ({need} > {have} bytes)")
            }
            Self::Range { sector, len } => {
                write!(f, "virtioblk: {len} bytes at sector {sector} out of range")
            }
            Self::ReadOnly => f.write_str("virtioblk: device is read-only"),
            Self::Timeout { sector } => write!(
                f,
                "virtioblk: request at sector {sector} not done after {} s, driver dead",
                REQUEST_TIMEOUT_NS / 1_000_000_000
            ),
            Self::Status { sector, status } => {
                write!(f, "virtioblk: status {status} at sector {sector}")
            }
            Self::Dead => f.write_str("virtioblk: driver dead after a timeout"),
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Is er op `base` een modern virtio-blokapparaat?
///
/// # Safety
///
/// `base` is een gemapt virtio-mmio-slot (minstens 0x200 bytes).
#[must_use]
pub unsafe fn is_modern_blk(base: Pa) -> bool {
    // SAFETY: de voorwaarde van deze functie.
    let r: &Regs = unsafe { dev::regs(base) };
    r.magic.read() == MAGIC && r.version.read() == VERSION_2 && r.device_id.read() == DEVICE_BLK
}

/// Eén virtio-blk met zijn queue en DMA-buffer.
pub struct VirtioBlk {
    base: Pa,
    dma: Pa,
    sectors: u64,
    flush: bool,
    read_only: bool,
    clock: fn() -> u64,
    avail_idx: u16,
    last_used: u16,
    dead: bool,
    /// Meetlat: afgehandelde verzoeken.
    pub requests: u64,
    /// Meetlat: het langste verzoek in nanoseconden.
    pub slowest_ns: u64,
}

impl VirtioBlk {
    /// Zet het device op: reset, VERSION_1 (plus FLUSH als het device hem
    /// biedt) onderhandelen, queue 0 in `dma`, DRIVER_OK. `clock` geeft
    /// monotone nanoseconden, voor de time-out van een verzoek.
    ///
    /// # Safety
    ///
    /// `base` is een gemapt virtio-mmio-blok dat voor altijd blijft, en
    /// `[dma, dma+dma_size)` is gemapt, niet gecached geheugen dat alleen
    /// deze driver en het device gebruiken, nu en zolang het programma
    /// draait.
    pub unsafe fn new(base: Pa, dma: Pa, dma_size: u64, clock: fn() -> u64) -> Result<Self> {
        if dma_size < DMA_NEED {
            return Err(Error::DmaTooSmall {
                need: DMA_NEED,
                have: dma_size,
            });
        }
        let mut d = Self {
            base,
            dma,
            sectors: 0,
            flush: false,
            read_only: false,
            clock,
            avail_idx: 0,
            last_used: 0,
            dead: false,
            requests: 0,
            slowest_ns: 0,
        };
        let r = d.regs();
        if r.magic.read() != MAGIC {
            return Err(Error::NotVirtio);
        }
        if r.version.read() != VERSION_2 {
            return Err(Error::Legacy);
        }
        let id = r.device_id.read();
        if id != DEVICE_BLK {
            return Err(Error::NotBlock(id));
        }

        r.status.write(0);
        r.status.write(STATUS_ACK);
        r.status.write(STATUS_ACK | STATUS_DRIVER);

        r.device_features_sel.write(0);
        let offered = r.device_features.read();
        d.flush = offered & FEAT_FLUSH != 0;
        d.read_only = offered & FEAT_RO != 0;
        r.driver_features_sel.write(0);
        r.driver_features.write(offered & (FEAT_FLUSH | FEAT_RO));
        r.driver_features_sel.write(1);
        r.driver_features.write(FEAT_VERSION_1_HI);
        r.status
            .write(STATUS_ACK | STATUS_DRIVER | STATUS_FEATURES_OK);
        if r.status.read() & STATUS_FEATURES_OK == 0 {
            return Err(Error::FeaturesRefused);
        }

        // De capaciteit, consistent gelezen: de config-generatie mag tussen
        // de twee helften niet wisselen.
        d.sectors = loop {
            let g = r.config_generation.read();
            let lo = u64::from(r.capacity_lo.read());
            let hi = u64::from(r.capacity_hi.read());
            if r.config_generation.read() == g {
                break (hi << 32) | lo;
            }
        };

        r.queue_sel.write(0);
        let max = r.queue_num_max.read();
        if max < u32::from(QSIZE) {
            return Err(Error::NoQueue(max));
        }
        r.queue_num.write(u32::from(QSIZE));
        dev::clear(dma, DATA_OFF as usize);
        let split = |pa: Pa| ((pa.0 & 0xffff_ffff) as u32, (pa.0 >> 32) as u32);
        let (lo, hi) = split(dma.add(DESC_OFF));
        r.queue_desc_lo.write(lo);
        r.queue_desc_hi.write(hi);
        let (lo, hi) = split(dma.add(AVAIL_OFF));
        r.queue_driver_lo.write(lo);
        r.queue_driver_hi.write(hi);
        let (lo, hi) = split(dma.add(USED_OFF));
        r.queue_device_lo.write(lo);
        r.queue_device_hi.write(hi);
        r.queue_ready.write(1);
        r.status
            .write(STATUS_ACK | STATUS_DRIVER | STATUS_FEATURES_OK | STATUS_DRIVER_OK);
        Ok(d)
    }

    fn regs(&self) -> &'static Regs {
        // SAFETY: de voorwaarde van `new`.
        unsafe { dev::regs(self.base) }
    }

    /// De capaciteit in sectoren van 512 bytes.
    #[must_use]
    pub fn sectors(&self) -> u64 {
        self.sectors
    }

    /// Kent het device FLUSH (een write-back-cache)?
    #[must_use]
    pub fn can_flush(&self) -> bool {
        self.flush
    }

    /// De naam voor de bootregel.
    #[must_use]
    pub fn model(&self) -> &'static str {
        "virtio-blk"
    }

    /// De lijn-kant voor een board dat de interrupt wil bevestigen: de
    /// driver pollt, maar een scherpe lijn moet toch los.
    pub fn ack(&self) -> u32 {
        let r = self.regs();
        let st = r.interrupt_status.read();
        if st != 0 {
            r.interrupt_ack.write(st);
        }
        st
    }

    fn set_desc(&self, i: u16, addr: Pa, len: u32, flags: u16, next: u16) {
        let d = self
            .dma
            .add(DESC_OFF + u64::from(i) * size_of::<Desc>() as u64);
        dev::write64(d.add(offset_of!(Desc, addr) as u64), addr.0);
        dev::write32(d.add(offset_of!(Desc, len) as u64), len);
        dev::write16(d.add(offset_of!(Desc, flags) as u64), flags);
        dev::write16(d.add(offset_of!(Desc, next) as u64), next);
    }

    /// Zet een verzoek klaar en luidt de doorbell: kop, (data,) status als
    /// één keten vanaf descriptor 0. De vorige keten is dan al klaar (één
    /// tegelijk), dus de descriptors zijn vrij.
    fn submit(&mut self, kind: u32, sector: u64, len: u32) {
        let hdr = self.dma.add(HDR_OFF);
        dev::write32(hdr.add(offset_of!(ReqHdr, kind) as u64), kind);
        dev::write32(hdr.add(4), 0);
        dev::write64(hdr.add(offset_of!(ReqHdr, sector) as u64), sector);
        let status = self.dma.add(STATUS_OFF);
        dev::write8(status, S_PENDING);
        if kind == T_FLUSH {
            self.set_desc(0, hdr, size_of::<ReqHdr>() as u32, DESC_NEXT, 2);
        } else {
            self.set_desc(0, hdr, size_of::<ReqHdr>() as u32, DESC_NEXT, 1);
            let w = if kind == T_IN { DESC_WRITE } else { 0 };
            self.set_desc(1, self.dma.add(DATA_OFF), len, DESC_NEXT | w, 2);
        }
        self.set_desc(2, status, 1, DESC_WRITE, 0);
        let avail = self.dma.add(AVAIL_OFF);
        dev::write16(avail.add(4 + u64::from(self.avail_idx % QSIZE) * 2), 0);
        self.avail_idx = self.avail_idx.wrapping_add(1);
        // De keten staat er vóór de index; de index vóór de doorbell.
        dev::mb();
        dev::write16(avail.add(2), self.avail_idx);
        dev::mb();
        self.regs().queue_notify.write(0);
    }

    /// Wacht tot het device het verzoek terugzet in de used-ring, of tot de
    /// time-out (dan dood).
    fn complete(&mut self, sector: u64, t0: u64) -> Result {
        let used = self.dma.add(USED_OFF);
        let deadline = t0.saturating_add(REQUEST_TIMEOUT_NS);
        while dev::read16(used.add(2)) == self.last_used {
            if (self.clock)() >= deadline {
                self.dead = true;
                return Err(Error::Timeout { sector });
            }
            core::hint::spin_loop();
        }
        // De index vóór de inhoud: pas na de barrière zijn status en data
        // van het device.
        dev::mb();
        self.last_used = self.last_used.wrapping_add(1);
        self.ack();
        let dt = (self.clock)().saturating_sub(t0);
        self.slowest_ns = self.slowest_ns.max(dt);
        self.requests += 1;
        match dev::read8(self.dma.add(STATUS_OFF)) {
            S_OK => Ok(()),
            status => Err(Error::Status { sector, status }),
        }
    }

    fn request(&mut self, kind: u32, sector: u64, len: usize) -> Result {
        if self.dead {
            return Err(Error::Dead);
        }
        let t0 = (self.clock)();
        self.submit(kind, sector, len as u32);
        self.complete(sector, t0)
    }

    /// Toetst `len` bytes vanaf `sector` tegen de schijf.
    fn check(&self, sector: u64, len: usize) -> Result {
        let n = len as u64;
        let end = n
            .is_multiple_of(SECTOR)
            .then(|| sector.checked_add(n / SECTOR))
            .flatten();
        match end {
            Some(e) if e <= self.sectors => Ok(()),
            _ => Err(Error::Range { sector, len }),
        }
    }

    /// Leest `buf.len()` bytes (een veelvoud van [`SECTOR`]) vanaf `sector`,
    /// in happen van [`MAX_TRANSFER`].
    pub fn read_at(&mut self, sector: u64, buf: &mut [u8]) -> Result {
        self.check(sector, buf.len())?;
        let mut s = sector;
        for chunk in buf.chunks_mut(MAX_TRANSFER) {
            self.request(T_IN, s, chunk.len())?;
            dev::copy_out(chunk, self.dma.add(DATA_OFF));
            s += chunk.len() as u64 / SECTOR;
        }
        Ok(())
    }

    /// Schrijft `buf` (een veelvoud van [`SECTOR`]) vanaf `sector`.
    pub fn write_at(&mut self, sector: u64, buf: &[u8]) -> Result {
        if self.read_only {
            return Err(Error::ReadOnly);
        }
        self.check(sector, buf.len())?;
        let mut s = sector;
        for chunk in buf.chunks(MAX_TRANSFER) {
            dev::copy_in(self.dma.add(DATA_OFF), chunk);
            self.request(T_OUT, s, chunk.len())?;
            s += chunk.len() as u64 / SECTOR;
        }
        Ok(())
    }

    /// Maakt alles wat geschreven is duurzaam; zonder FLUSH-feature is er
    /// geen cache en is dit niets.
    pub fn sync(&mut self) -> Result {
        if !self.flush {
            return Ok(());
        }
        self.request(T_FLUSH, 0, 0)
    }
}

impl blkdev::BlockDevice for VirtioBlk {
    fn read(&mut self, lba: u64, buf: &mut [u8]) -> blkdev::Result {
        self.read_at(lba, buf)
            .map_err(|_| blkdev::Error::Io { lba })
    }

    fn write(&mut self, lba: u64, buf: &[u8]) -> blkdev::Result {
        self.write_at(lba, buf)
            .map_err(|_| blkdev::Error::Io { lba })
    }

    fn flush(&mut self) -> blkdev::Result {
        self.sync().map_err(|_| blkdev::Error::Dead)
    }
}

#[cfg(test)]
mod tests;
