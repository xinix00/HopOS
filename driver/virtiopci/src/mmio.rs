//! virtio-mmio versie 2 (virtio 1.2 §4.2.2): één registerblok per slot,
//! zoals QEMU virt ze vanaf 0x0a00_0000 legt.
//!
//! Bezit alleen de registertabel; welke slots er zijn, weet het board (uit
//! de FDT of door de 32 slots af te lopen).

use crate::{Error, Result, Transport, split};
use core::mem::{offset_of, size_of};
use dev::{Pa, Reg};

/// De virtio-mmio-registers, versie 2 (virtio 1.2 §4.2.2). De device-config
/// begint direct erna, op [`CONFIG_OFF`].
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
    /// Bit 0 = used-ring bijgewerkt, bit 1 = config veranderd.
    interrupt_status: Reg<u32>,
    /// Dezelfde bits terugschrijven laat de (level-)lijn los.
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
    assert!(size_of::<Regs>() == CONFIG_OFF as usize);
};

/// "virt", little-endian.
pub const MAGIC: u32 = 0x7472_6976;
/// Het moderne transport.
pub const VERSION_2: u32 = 2;
/// Waar de device-config in het slot begint.
pub const CONFIG_OFF: u64 = 0x100;
/// De maat van één slot op QEMU virt; de config loopt tot het eind ervan.
pub const SLOT_SIZE: u64 = 0x200;

/// Is er op `base` een modern virtio-mmio-device van type `device_id`?
///
/// # Safety
///
/// `base` is een gemapt virtio-mmio-slot (minstens [`SLOT_SIZE`] bytes).
#[must_use]
pub unsafe fn is_modern(base: Pa, device_id: u32) -> bool {
    // SAFETY: de voorwaarde van deze functie.
    let m = unsafe { Mmio::new(base) };
    m.check().is_ok() && m.device_id() == device_id
}

/// Eén virtio-mmio-slot. `Copy`: het is een adres, en het interrupt-pad van
/// een board mag er een kopie van houden (alleen InterruptStatus en
/// InterruptACK, die niets met de ringen delen).
///
/// # Invariants
///
/// `base` is een gemapt virtio-mmio-slot dat zolang het programma draait
/// blijft bestaan.
#[derive(Clone, Copy, Debug)]
pub struct Mmio {
    base: Pa,
}

impl Mmio {
    /// Het slot op `base`, zonder iets te lezen; [`check`](Self::check)
    /// zegt of er een modern virtio-device zit.
    ///
    /// # Safety
    ///
    /// `base` is een gemapt virtio-mmio-slot (minstens [`SLOT_SIZE`] bytes,
    /// Device-geheugen) dat zolang het programma draait blijft bestaan.
    #[must_use]
    pub const unsafe fn new(base: Pa) -> Self {
        // INVARIANT: de voorwaarde van deze functie.
        Self { base }
    }

    /// Het adres van het slot.
    #[must_use]
    pub const fn base(&self) -> Pa {
        self.base
    }

    /// Staat er virtio in het slot, en het moderne transport?
    pub fn check(&self) -> Result {
        let r = self.regs();
        if r.magic.read() != MAGIC {
            return Err(Error::NotVirtio);
        }
        let version = r.version.read();
        if version != VERSION_2 {
            return Err(Error::Legacy { version });
        }
        Ok(())
    }

    fn regs(&self) -> &'static Regs {
        // SAFETY: de invariant van `Mmio`.
        unsafe { dev::regs(self.base) }
    }

    /// Het adres van config-offset `off` als een toegang van `width` bytes
    /// daar binnen het slot past en gealigneerd is.
    fn config(&self, off: u32, width: u64) -> Option<Pa> {
        let off = u64::from(off);
        let fits = off.is_multiple_of(width) && off + width <= SLOT_SIZE - CONFIG_OFF;
        fits.then(|| self.base.add(CONFIG_OFF + off))
    }
}

impl Transport for Mmio {
    fn device_id(&self) -> u32 {
        self.regs().device_id.read()
    }

    fn device_features(&self, window: u32) -> u32 {
        let r = self.regs();
        r.device_features_sel.write(window);
        r.device_features.read()
    }

    fn set_driver_features(&self, window: u32, bits: u32) {
        let r = self.regs();
        r.driver_features_sel.write(window);
        r.driver_features.write(bits);
    }

    fn status(&self) -> u8 {
        self.regs().status.read() as u8
    }

    fn set_status(&self, status: u8) {
        self.regs().status.write(u32::from(status));
    }

    fn select_queue(&mut self, queue: u16) {
        self.regs().queue_sel.write(u32::from(queue));
    }

    fn queue_num_max(&self) -> u16 {
        // Een slot dat meer biedt dan een u16, biedt in elk geval de grootste.
        u16::try_from(self.regs().queue_num_max.read()).unwrap_or(u16::MAX)
    }

    fn set_queue_num(&self, num: u16) {
        self.regs().queue_num.write(u32::from(num));
    }

    fn set_queue_addrs(&self, desc: Pa, driver: Pa, device: Pa) {
        let r = self.regs();
        let (lo, hi) = split(desc);
        r.queue_desc_lo.write(lo);
        r.queue_desc_hi.write(hi);
        let (lo, hi) = split(driver);
        r.queue_driver_lo.write(lo);
        r.queue_driver_hi.write(hi);
        let (lo, hi) = split(device);
        r.queue_device_lo.write(lo);
        r.queue_device_hi.write(hi);
    }

    fn enable_queue(&mut self) {
        self.regs().queue_ready.write(1);
    }

    fn notify(&self, queue: u16) {
        self.regs().queue_notify.write(u32::from(queue));
    }

    /// Zonder de schrijf naar InterruptACK vuurt de lijn na de EOI meteen
    /// weer, hoe leeg de ring ook is.
    fn ack_interrupt(&self) -> u32 {
        let r = self.regs();
        let st = r.interrupt_status.read();
        if st != 0 {
            r.interrupt_ack.write(st);
        }
        st
    }

    fn config_generation(&self) -> u32 {
        self.regs().config_generation.read()
    }

    fn config_read8(&self, off: u32) -> u8 {
        self.config(off, 1).map_or(u8::MAX, dev::read8)
    }

    fn config_read32(&self, off: u32) -> u32 {
        self.config(off, 4).map_or(u32::MAX, dev::read32)
    }
}
