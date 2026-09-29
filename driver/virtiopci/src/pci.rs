//! virtio-pci, het moderne transport (virtio 1.2 §4.1): vijf structuren
//! achter vendor-capabilities, elk in een BAR van de functie.
//!
//! [`Pci::new`] leest de capabilities één keer, rekent de adressen uit en
//! houdt daarna alleen [`Pa`]'s: geen verwijzing naar de config-space, zodat
//! de driver hem als gewone waarde bezit. Het notify-adres van een queue
//! staat vast zodra de queue gekozen is (`queue_notify_off` maal de
//! multiplier van de notify-capability) en wordt bij [`enable_queue`]
//! bewaard; de doorbell zelf is dan één 16-bit schrijf, zonder config-lees
//! op het hete pad.
//!
//! Een transitional device (0x1000 net, 0x1001 blk) heeft naast zijn
//! legacy I/O-BAR ook de moderne capabilities; wij gebruiken alleen die.
//!
//! [`enable_queue`]: Transport::enable_queue

use crate::{Error, Result, Transport, split};
use core::mem::{offset_of, size_of};
use dev::{Pa, Reg};
use driver_pcie::{CAP_VENDOR, Config, Function};

/// De PCI-vendor van virtio (Red Hat / Qumranet).
pub const VENDOR: u16 = 0x1af4;
/// Het eerste moderne device-id: `0x1040 + type`.
pub const DEVICE_MODERN: u16 = 0x1040;
/// Het laatste device-id van de moderne reeks (§4.1.2).
pub const DEVICE_MODERN_LAST: u16 = 0x107f;

/// De structuursoorten van een virtio-capability (`cfg_type`).
pub mod cfg_type {
    /// De common configuration: features, status, queues.
    pub const COMMON: u8 = 1;
    /// De notify-regio: één doorbell per queue.
    pub const NOTIFY: u8 = 2;
    /// De ISR-status: lezen is bevestigen.
    pub const ISR: u8 = 3;
    /// De device-specifieke config (de MAC, de capaciteit).
    pub const DEVICE: u8 = 4;
    /// Het config-venster via de config-space; negeren wij (wij hebben de
    /// BAR's gemapt).
    pub const PCI: u8 = 5;
}

/// Het hoogste aantal queues dat [`Pci`] een doorbell geeft: genoeg voor
/// virtio-net (RX en TX) en virtio-blk (één), met ruimte voor een tweede
/// paar. Een queue daarboven biedt [`Transport::queue_num_max`] als 0.
pub const MAX_QUEUES: usize = 8;

/// Een `virtio_pci_cap` in de config-space (§4.1.4), plus de multiplier die
/// alleen de notify-capability heeft (`virtio_pci_notify_cap`). Alleen voor
/// de offsets: de config-space lezen we via [`Config`].
#[repr(C)]
struct VirtioPciCap {
    cap_vndr: u8,
    cap_next: u8,
    cap_len: u8,
    cfg_type: u8,
    bar: u8,
    id: u8,
    _padding: [u8; 2],
    offset: u32,
    length: u32,
    notify_off_multiplier: u32,
}

const _: () = {
    assert!(offset_of!(VirtioPciCap, cap_vndr) == 0);
    assert!(offset_of!(VirtioPciCap, cap_next) == 1);
    assert!(offset_of!(VirtioPciCap, cap_len) == 2);
    assert!(offset_of!(VirtioPciCap, cfg_type) == 3);
    assert!(offset_of!(VirtioPciCap, bar) == 4);
    assert!(offset_of!(VirtioPciCap, id) == 5);
    assert!(offset_of!(VirtioPciCap, offset) == 8);
    assert!(offset_of!(VirtioPciCap, length) == 12);
    assert!(offset_of!(VirtioPciCap, notify_off_multiplier) == 16);
};

/// De maat van een gewone capability; de notify-capability is 4 groter.
const CAP_LEN: u8 = offset_of!(VirtioPciCap, notify_off_multiplier) as u8;
const NOTIFY_CAP_LEN: u8 = size_of::<VirtioPciCap>() as u8;

/// De common configuration (`virtio_pci_common_cfg`, §4.1.4.3). Device-
/// geheugen wil natuurlijk gealigneerde toegang van de juiste breedte, dus
/// elk veld heeft zijn eigen breedte; de 64-bit adressen gaan als twee
/// woorden, wat de spec toestaat en QEMU per 32 bits implementeert.
#[repr(C)]
struct CommonCfg {
    device_feature_select: Reg<u32>,
    device_feature: Reg<u32>,
    driver_feature_select: Reg<u32>,
    driver_feature: Reg<u32>,
    /// MSI-X-vector voor config-wijzigingen; wij laten hem op NO_VECTOR.
    _config_msix_vector: Reg<u16>,
    num_queues: Reg<u16>,
    device_status: Reg<u8>,
    config_generation: Reg<u8>,
    queue_select: Reg<u16>,
    /// Leest de grootste queue die het device biedt; de driver mag er een
    /// kleinere in schrijven.
    queue_size: Reg<u16>,
    _queue_msix_vector: Reg<u16>,
    queue_enable: Reg<u16>,
    queue_notify_off: Reg<u16>,
    queue_desc_lo: Reg<u32>,
    queue_desc_hi: Reg<u32>,
    queue_driver_lo: Reg<u32>,
    queue_driver_hi: Reg<u32>,
    queue_device_lo: Reg<u32>,
    queue_device_hi: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(CommonCfg, device_feature_select) == 0x00);
    assert!(offset_of!(CommonCfg, device_feature) == 0x04);
    assert!(offset_of!(CommonCfg, driver_feature_select) == 0x08);
    assert!(offset_of!(CommonCfg, driver_feature) == 0x0c);
    assert!(offset_of!(CommonCfg, _config_msix_vector) == 0x10);
    assert!(offset_of!(CommonCfg, num_queues) == 0x12);
    assert!(offset_of!(CommonCfg, device_status) == 0x14);
    assert!(offset_of!(CommonCfg, config_generation) == 0x15);
    assert!(offset_of!(CommonCfg, queue_select) == 0x16);
    assert!(offset_of!(CommonCfg, queue_size) == 0x18);
    assert!(offset_of!(CommonCfg, _queue_msix_vector) == 0x1a);
    assert!(offset_of!(CommonCfg, queue_enable) == 0x1c);
    assert!(offset_of!(CommonCfg, queue_notify_off) == 0x1e);
    assert!(offset_of!(CommonCfg, queue_desc_lo) == 0x20);
    assert!(offset_of!(CommonCfg, queue_desc_hi) == 0x24);
    assert!(offset_of!(CommonCfg, queue_driver_lo) == 0x28);
    assert!(offset_of!(CommonCfg, queue_driver_hi) == 0x2c);
    assert!(offset_of!(CommonCfg, queue_device_lo) == 0x30);
    assert!(offset_of!(CommonCfg, queue_device_hi) == 0x34);
    assert!(size_of::<CommonCfg>() == 0x38);
};

/// Het virtio-devicetype van een PCI-functie, of `None` als het geen
/// virtio-device is: modern `0x1040 + type`, of een transitional id
/// (§4.1.2.1: 0x1000 net, 0x1001 blk, 0x1002 balloon, 0x1003 console,
/// 0x1004 scsi, 0x1005 entropy, 0x1009 9p).
#[must_use]
pub fn device_type(f: &Function) -> Option<u32> {
    if f.vendor != VENDOR {
        return None;
    }
    let ty = match f.device {
        d @ DEVICE_MODERN..=DEVICE_MODERN_LAST => d - DEVICE_MODERN,
        0x1000 => 1,
        0x1001 => 2,
        0x1002 => 5,
        0x1003 => 3,
        0x1004 => 8,
        0x1005 => 4,
        0x1009 => 9,
        _ => return None,
    };
    // Type 0 is gereserveerd: 0x1040 is geen device.
    (ty != 0).then_some(u32::from(ty))
}

/// Een gevonden virtio-structuur: waar hij staat en hoe groot hij is.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct Region {
    pa: Pa,
    len: u32,
}

/// Wat de capability-walk vond.
#[derive(Default)]
struct Found {
    common: Option<Region>,
    notify: Option<(Region, u32)>,
    isr: Option<Region>,
    device: Option<Region>,
    /// De eerste BAR die een structuur nodig had maar niet toegewezen was.
    unassigned: Option<u8>,
}

/// Leest de virtio-capabilities van `f`: per soort de eerste die bruikbaar
/// is (§4.1.4: "the driver SHOULD use the first instance"). Een capability
/// met een gereserveerde BAR, een te korte `cap_len`, een scheve offset of
/// een BAR die niet toegewezen is, wordt overgeslagen.
fn walk<C: Config + ?Sized>(cfg: &C, f: &Function) -> Found {
    let mut found = Found::default();
    let bdf = f.bdf;
    for (id, cap) in f.caps(cfg) {
        if id != CAP_VENDOR {
            continue;
        }
        let at = |field: usize| cap.saturating_add(field as u16);
        let len = cfg.read8(bdf, at(offset_of!(VirtioPciCap, cap_len)));
        let ty = cfg.read8(bdf, at(offset_of!(VirtioPciCap, cfg_type)));
        let bar = cfg.read8(bdf, at(offset_of!(VirtioPciCap, bar)));
        let off = cfg.read32(bdf, at(offset_of!(VirtioPciCap, offset)));
        let size = cfg.read32(bdf, at(offset_of!(VirtioPciCap, length)));
        let (need_len, align) = match ty {
            cfg_type::COMMON => (CAP_LEN, 4),
            cfg_type::NOTIFY => (NOTIFY_CAP_LEN, 2),
            cfg_type::ISR => (CAP_LEN, 1),
            cfg_type::DEVICE => (CAP_LEN, 4),
            // PCI-cfg, shared memory, vendor-eigen: niet voor ons.
            _ => continue,
        };
        if len < need_len || bar > 5 || !u64::from(off).is_multiple_of(align) {
            continue;
        }
        let taken = match ty {
            cfg_type::COMMON => found.common.is_some(),
            cfg_type::NOTIFY => found.notify.is_some(),
            cfg_type::ISR => found.isr.is_some(),
            _ => found.device.is_some(),
        };
        if taken {
            continue;
        }
        let base = f.bar_addr(cfg, bar);
        let Some(pa) = base.checked_add(u64::from(off)).filter(|_| base != 0) else {
            found.unassigned.get_or_insert(bar);
            continue;
        };
        let region = Region {
            pa: Pa(pa),
            len: size,
        };
        match ty {
            cfg_type::COMMON => found.common = Some(region),
            cfg_type::NOTIFY => {
                let mult = cfg.read32(bdf, at(offset_of!(VirtioPciCap, notify_off_multiplier)));
                found.notify = Some((region, mult));
            }
            cfg_type::ISR => found.isr = Some(region),
            _ => found.device = Some(region),
        }
    }
    found
}

/// Eén virtio-pci-functie, klaar voor een driver.
///
/// # Invariants
///
/// `common`, `notify`, `isr` en `device` liggen in BAR's van één
/// virtio-functie die als Device gemapt zijn en blijven, met memory-decode
/// aan; `common` heeft minstens de maat van `CommonCfg`.
pub struct Pci {
    common: Pa,
    notify: Pa,
    notify_len: u32,
    multiplier: u32,
    isr: Pa,
    /// De device-config; een device zonder heeft `device_len` 0.
    device: Pa,
    device_len: u32,
    device_id: u32,
    /// De gekozen queue.
    selected: u16,
    /// Het notify-adres van de gekozen queue, als die er een kan hebben.
    pending: Option<Pa>,
    /// Het notify-adres per aangezette queue.
    doorbells: [Option<Pa>; MAX_QUEUES],
}

impl Pci {
    /// Vindt de virtio-structuren van `f`, zet memory-decode en bus-master
    /// aan (EDK2 laat bus-master na ExitBootServices niet gegarandeerd aan,
    /// en zonder kan het device niet in de ringen), en geeft het transport.
    ///
    /// De BAR-adressen komen uit [`Function::bar_addr`]: de firmware wees ze
    /// toe, of op een kale fabric het board (`Function::assign_bars`).
    ///
    /// # Safety
    ///
    /// `cfg` is het config-venster waarin `f` gevonden is, en elke
    /// memory-BAR van `f` is als Device gemapt en blijft dat zolang het
    /// programma draait. Niemand anders drijft deze functie.
    pub unsafe fn new<C: Config + ?Sized>(cfg: &C, f: &Function) -> Result<Self> {
        let device_id = device_type(f).ok_or(Error::NotVirtio)?;
        let found = walk(cfg, f);
        let missing = |cfg_type| match found.unassigned {
            Some(bar) => Error::BarUnassigned { bar },
            None => Error::Missing { cfg_type },
        };
        let common = found
            .common
            .filter(|r| r.len as usize >= size_of::<CommonCfg>())
            .ok_or_else(|| missing(cfg_type::COMMON))?;
        let (notify, multiplier) = found
            .notify
            .filter(|(r, _)| r.len >= 2)
            .ok_or_else(|| missing(cfg_type::NOTIFY))?;
        let isr = found
            .isr
            .filter(|r| r.len >= 1)
            .ok_or_else(|| missing(cfg_type::ISR))?;
        let device = found.device.unwrap_or(Region { pa: Pa(0), len: 0 });
        f.enable(cfg);
        // INVARIANT: de adressen komen uit de BAR's van `f` (de walk), die
        // volgens de voorwaarde van deze functie gemapt zijn; decode staat
        // nu aan, en de maat van common is hierboven getoetst.
        Ok(Self {
            common: common.pa,
            notify: notify.pa,
            notify_len: notify.len,
            multiplier,
            isr: isr.pa,
            device: device.pa,
            device_len: device.len,
            device_id,
            selected: 0,
            pending: None,
            doorbells: [None; MAX_QUEUES],
        })
    }

    fn common(&self) -> &'static CommonCfg {
        // SAFETY: de invariant van `Pci`.
        unsafe { dev::regs(self.common) }
    }

    /// Het aantal queues dat het device heeft.
    #[must_use]
    pub fn num_queues(&self) -> u16 {
        self.common().num_queues.read()
    }

    /// Het notify-adres van de gekozen queue: `queue_notify_off` maal de
    /// multiplier, binnen de notify-regio en 2-gealigneerd (de doorbell is
    /// een 16-bit schrijf). `None` als het device een adres buiten zijn
    /// eigen regio opgeeft.
    fn doorbell(&self) -> Option<Pa> {
        let off = u64::from(self.common().queue_notify_off.read()) * u64::from(self.multiplier);
        let end = off.checked_add(2)?;
        let pa = self.notify.add(off);
        (end <= u64::from(self.notify_len) && pa.is_aligned(2)).then_some(pa)
    }

    /// Het adres van device-config-offset `off` als een toegang van `width`
    /// bytes daar binnen de regio past en gealigneerd is.
    fn config(&self, off: u32, width: u32) -> Option<Pa> {
        let fits = off.is_multiple_of(width) && off.checked_add(width)? <= self.device_len;
        fits.then(|| self.device.add(u64::from(off)))
    }
}

impl Transport for Pci {
    fn device_id(&self) -> u32 {
        self.device_id
    }

    fn device_features(&self, window: u32) -> u32 {
        let c = self.common();
        c.device_feature_select.write(window);
        c.device_feature.read()
    }

    fn set_driver_features(&self, window: u32, bits: u32) {
        let c = self.common();
        c.driver_feature_select.write(window);
        c.driver_feature.write(bits);
    }

    fn status(&self) -> u8 {
        self.common().device_status.read()
    }

    fn set_status(&self, status: u8) {
        self.common().device_status.write(status);
    }

    /// Kiest de queue en rekent zijn doorbell uit, nu de queue gekozen is en
    /// `queue_notify_off` dus over deze queue gaat.
    fn select_queue(&mut self, queue: u16) {
        self.common().queue_select.write(queue);
        self.selected = queue;
        let known = usize::from(queue) < MAX_QUEUES && queue < self.num_queues();
        self.pending = if known { self.doorbell() } else { None };
    }

    fn queue_num_max(&self) -> u16 {
        // Zonder bruikbare doorbell is de queue voor de driver er niet.
        if self.pending.is_none() {
            return 0;
        }
        self.common().queue_size.read()
    }

    fn set_queue_num(&self, num: u16) {
        self.common().queue_size.write(num);
    }

    fn set_queue_addrs(&self, desc: Pa, driver: Pa, device: Pa) {
        let c = self.common();
        let (lo, hi) = split(desc);
        c.queue_desc_lo.write(lo);
        c.queue_desc_hi.write(hi);
        let (lo, hi) = split(driver);
        c.queue_driver_lo.write(lo);
        c.queue_driver_hi.write(hi);
        let (lo, hi) = split(device);
        c.queue_device_lo.write(lo);
        c.queue_device_hi.write(hi);
    }

    fn enable_queue(&mut self) {
        self.common().queue_enable.write(1);
        if let Some(slot) = self.doorbells.get_mut(usize::from(self.selected)) {
            *slot = self.pending;
        }
    }

    /// De queue-index naar zijn notify-adres (zonder NOTIFICATION_DATA, dat
    /// wij niet onderhandelen). Een queue die niet aangezet is, heeft geen
    /// doorbell en er gebeurt niets.
    fn notify(&self, queue: u16) {
        if let Some(Some(pa)) = self.doorbells.get(usize::from(queue)) {
            dev::write16(*pa, queue);
        }
    }

    /// Het ISR-register lezen bevestigt hem ook (§4.1.4.5): er is geen
    /// aparte ack-schrijf zoals bij mmio.
    fn ack_interrupt(&self) -> u32 {
        u32::from(dev::read8(self.isr))
    }

    fn config_generation(&self) -> u32 {
        u32::from(self.common().config_generation.read())
    }

    fn config_read8(&self, off: u32) -> u8 {
        self.config(off, 1).map_or(u8::MAX, dev::read8)
    }

    fn config_read32(&self, off: u32) -> u32 {
        self.config(off, 4).map_or(u32::MAX, dev::read32)
    }
}
