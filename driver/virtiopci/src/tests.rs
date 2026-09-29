//! Host-tests: de capability-parse en de notify-rekensom over een
//! nep-config-space met een nep-BAR in RAM, het mmio-slot in RAM, en de
//! standaardmethoden van de trait over een nep-transport.

use super::*;
use core::cell::Cell;
use driver_pcie::{Bdf, CAP_MSIX, CAP_VENDOR, CMD_MASTER, CMD_MEM, probe};
use std::cell::RefCell;
use std::vec;
use std::vec::Vec;

/// Eén functie: 4 KB config-space plus het beschrijfbare masker per dword.
struct FakeCfg {
    space: RefCell<[u32; 1024]>,
    wmask: [u32; 1024],
}

impl driver_pcie::Config for FakeCfg {
    fn read32(&self, bdf: Bdf, off: u16) -> u32 {
        if bdf != BDF {
            return u32::MAX;
        }
        self.space.borrow()[usize::from(off / 4)]
    }
    fn write32(&self, bdf: Bdf, off: u16, v: u32) {
        if bdf == BDF {
            let i = usize::from(off / 4);
            let mut s = self.space.borrow_mut();
            s[i] = (s[i] & !self.wmask[i]) | (v & self.wmask[i]);
        }
    }
    fn write16(&self, bdf: Bdf, off: u16, v: u16) {
        if bdf == BDF {
            let i = usize::from(off / 4);
            let shift = (off & 2) * 8;
            let m = self.wmask[i] & (0xffff << shift);
            let mut s = self.space.borrow_mut();
            s[i] = (s[i] & !m) | ((u32::from(v) << shift) & m);
        }
    }
}

const BDF: Bdf = Bdf {
    bus: 0,
    dev: 3,
    func: 0,
};

/// De indeling van de nep-BAR, zoals QEMU hem ongeveer legt.
const COMMON: u32 = 0x0000;
const ISR: u32 = 0x1000;
const DEVICE: u32 = 0x2000;
const NOTIFY: u32 = 0x3000;
const NOTIFY_LEN: u32 = 0x1000;
const MULT: u32 = 4;
const BAR_SIZE: usize = 0x4000;

/// Een virtio-capability als dwords: (type, bar, offset, lengte), en voor
/// notify de multiplier erachter.
fn vcap(ty: u8, bar: u8, off: u32, len: u32) -> Vec<u32> {
    let cap_len: u32 = if ty == pci::cfg_type::NOTIFY { 20 } else { 16 };
    let mut w = vec![
        (cap_len << 16) | (u32::from(ty) << 24),
        u32::from(bar),
        off,
        len,
    ];
    if ty == pci::cfg_type::NOTIFY {
        w.push(MULT);
    }
    w
}

/// Een nep-BAR in RAM, op 16 bytes gealigneerd (de laagste vier bits van
/// een BAR zijn vlaggen).
struct Bar {
    /// Houdt het geheugen levend; de test raakt het via `base`.
    _mem: Vec<u32>,
    base: u64,
}

impl Bar {
    fn new() -> Self {
        let mut mem = vec![0u32; BAR_SIZE / 4 + 4];
        let base = (mem.as_mut_ptr() as usize as u64).next_multiple_of(16);
        Self { _mem: mem, base }
    }
    fn pa(&self, off: u32) -> Pa {
        Pa(self.base + u64::from(off))
    }
}

/// Een virtio-functie met `device` als id, BAR0 (64-bit) op `bar`, en de
/// capabilities `caps` als `(id, dwords)` vanaf 0x40, elk op 0x18 bytes.
fn function(device: u16, bar: u64, caps: &[(u8, Vec<u32>)]) -> FakeCfg {
    let mut space = [0u32; 1024];
    let mut wmask = [0u32; 1024];
    space[0] = u32::from(pci::VENDOR) | (u32::from(device) << 16);
    space[2] = 0x02_0000 << 8;
    wmask[1] = 0xffff;
    space[4] = (bar as u32 & !0xf) | 0b100;
    space[5] = (bar >> 32) as u32;
    if !caps.is_empty() {
        space[1] |= 1 << 20;
        space[0x34 / 4] = 0x40;
    }
    for (i, (id, words)) in caps.iter().enumerate() {
        let at = 0x40 + i * 0x18;
        let next = if i + 1 < caps.len() { at + 0x18 } else { 0 };
        space[at / 4] = u32::from(*id) | ((next as u32) << 8) | (words[0] & 0xffff_0000);
        for (j, w) in words.iter().enumerate().skip(1) {
            space[at / 4 + j] = *w;
        }
    }
    FakeCfg {
        space: RefCell::new(space),
        wmask,
    }
}

/// Een complete virtio-net-functie: een common-cap met gereserveerde BAR
/// die overgeslagen moet worden, de vier structuren, een PCI-cfg-cap en een
/// MSI-X-cap ertussen.
fn net(bar: u64) -> FakeCfg {
    function(
        0x1041,
        bar,
        &[
            (CAP_MSIX, vec![0, 0, 0]),
            (CAP_VENDOR, vcap(pci::cfg_type::COMMON, 6, 0x100, 0x38)),
            (CAP_VENDOR, vcap(pci::cfg_type::COMMON, 0, COMMON, 0x38)),
            (CAP_VENDOR, vcap(pci::cfg_type::ISR, 0, ISR, 4)),
            (CAP_VENDOR, vcap(pci::cfg_type::DEVICE, 0, DEVICE, 0x20)),
            (
                CAP_VENDOR,
                vcap(pci::cfg_type::NOTIFY, 0, NOTIFY, NOTIFY_LEN),
            ),
            (CAP_VENDOR, vcap(pci::cfg_type::PCI, 0, 0, 0)),
            // Een tweede common: de eerste bruikbare wint.
            (CAP_VENDOR, vcap(pci::cfg_type::COMMON, 0, 0x200, 0x38)),
        ],
    )
}

fn func(vendor: u16, device: u16) -> driver_pcie::Function {
    driver_pcie::Function {
        bdf: BDF,
        vendor,
        device,
        class: 0,
        revision: 0,
        header: 0,
        multi: false,
    }
}

#[test]
fn device_type_knows_modern_and_transitional_ids() {
    assert_eq!(pci::device_type(&func(0x1af4, 0x1041)), Some(1));
    assert_eq!(pci::device_type(&func(0x1af4, 0x1042)), Some(2));
    assert_eq!(pci::device_type(&func(0x1af4, 0x1000)), Some(1));
    assert_eq!(pci::device_type(&func(0x1af4, 0x1001)), Some(2));
    assert_eq!(pci::device_type(&func(0x1af4, 0x1005)), Some(4));
    assert_eq!(pci::device_type(&func(0x1af4, 0x1040)), None);
    assert_eq!(pci::device_type(&func(0x1af4, 0x1080)), None);
    assert_eq!(pci::device_type(&func(0x1af4, 0x1006)), None);
    assert_eq!(pci::device_type(&func(0x8086, 0x1041)), None);
}

#[test]
fn pci_finds_its_structures_and_enables_the_function() {
    let bar = Bar::new();
    let cfg = net(bar.base);
    let f = probe(&cfg, BDF).unwrap();
    // SAFETY: de BAR is een levende Vec in deze test.
    let t = unsafe { Pci::new(&cfg, &f) }.unwrap();
    assert_eq!(t.device_id(), 1);
    assert_eq!(f.command(&cfg), CMD_MEM | CMD_MASTER);

    // De device-config: de MAC per byte, een woord, en buiten de regio
    // alle enen.
    dev::copy_in(bar.pa(DEVICE), &[0x52, 0x54, 0, 0x12, 0x34, 0x56]);
    let mac: Vec<u8> = (0..6).map(|i| t.config_read8(i)).collect();
    assert_eq!(mac, [0x52, 0x54, 0, 0x12, 0x34, 0x56]);
    assert_eq!(t.config_read32(0), 0x1200_5452);
    assert_eq!(t.config_read32(0x20), u32::MAX);
    assert_eq!(t.config_read32(2), u32::MAX, "unaligned");
    assert_eq!(t.config_read8(0x20), u8::MAX);

    // De common-cfg van de tweede common-cap (de eerste had BAR 6).
    dev::write8(bar.pa(COMMON + 0x14), 0x0f);
    assert_eq!(t.status(), 0x0f);
    t.set_status(status::ACKNOWLEDGE);
    assert_eq!(dev::read8(bar.pa(COMMON + 0x14)), status::ACKNOWLEDGE);
    dev::write32(bar.pa(COMMON + 0x04), 0xabcd);
    assert_eq!(t.device_features(1), 0xabcd);
    assert_eq!(dev::read32(bar.pa(COMMON)), 1, "feature select");
    t.set_driver_features(1, FEAT_VERSION_1_HI);
    assert_eq!(dev::read32(bar.pa(COMMON + 0x08)), 1);
    assert_eq!(dev::read32(bar.pa(COMMON + 0x0c)), FEAT_VERSION_1_HI);

    // ISR: lezen is bevestigen, niets schrijven.
    dev::write8(bar.pa(ISR), 1);
    assert_eq!(t.ack_interrupt(), 1);
    assert_eq!(dev::read8(bar.pa(ISR)), 1);
}

#[test]
fn notify_address_is_notify_off_times_the_multiplier() {
    let bar = Bar::new();
    let cfg = net(bar.base);
    let f = probe(&cfg, BDF).unwrap();
    // SAFETY: de BAR is een levende Vec in deze test.
    let mut t = unsafe { Pci::new(&cfg, &f) }.unwrap();
    dev::write16(bar.pa(COMMON + 0x12), 2); // num_queues
    dev::write16(bar.pa(COMMON + 0x18), 256); // queue_size
    dev::write16(bar.pa(COMMON + 0x1e), 3); // queue_notify_off

    t.select_queue(1);
    assert_eq!(dev::read16(bar.pa(COMMON + 0x16)), 1, "queue_select");
    assert_eq!(t.queue_num_max(), 256);
    t.set_queue_num(64);
    assert_eq!(dev::read16(bar.pa(COMMON + 0x18)), 64);
    t.set_queue_addrs(Pa(0x1_2345_6000), Pa(0x7000), Pa(0x8000));
    assert_eq!(dev::read32(bar.pa(COMMON + 0x20)), 0x2345_6000);
    assert_eq!(dev::read32(bar.pa(COMMON + 0x24)), 1);
    assert_eq!(dev::read32(bar.pa(COMMON + 0x28)), 0x7000);
    assert_eq!(dev::read32(bar.pa(COMMON + 0x30)), 0x8000);

    // Voor enable heeft de queue geen doorbell.
    let bell = bar.pa(NOTIFY + 3 * MULT);
    dev::write16(bell, 0xeeee);
    t.notify(1);
    assert_eq!(dev::read16(bell), 0xeeee);
    t.enable_queue();
    assert_eq!(dev::read16(bar.pa(COMMON + 0x1c)), 1, "queue_enable");
    t.notify(1);
    assert_eq!(dev::read16(bell), 1);
    // De queue_notify_off van een andere queue verandert zijn adres niet:
    // het staat vast sinds het kiezen.
    dev::write16(bar.pa(COMMON + 0x1e), 9);
    dev::write16(bell, 0);
    t.notify(1);
    assert_eq!(dev::read16(bell), 1);

    // Een notify_off buiten de notify-regio: de queue is er niet.
    dev::write16(bar.pa(COMMON + 0x1e), (NOTIFY_LEN / MULT) as u16);
    t.select_queue(0);
    assert_eq!(t.queue_num_max(), 0);
    // Net erin: wel.
    dev::write16(bar.pa(COMMON + 0x1e), (NOTIFY_LEN / MULT - 1) as u16);
    t.select_queue(0);
    // Nep-geheugen: het leest wat we eerder in queue_size schreven.
    assert_eq!(t.queue_num_max(), 64);
    // Een queue die het device niet heeft, of voorbij onze tabel.
    t.select_queue(2);
    assert_eq!(t.queue_num_max(), 0);
    dev::write16(bar.pa(COMMON + 0x12), 64);
    t.select_queue(pci::MAX_QUEUES as u16);
    assert_eq!(t.queue_num_max(), 0);
    t.notify(pci::MAX_QUEUES as u16); // geen doorbell, geen paniek
}

#[test]
fn missing_or_unassigned_structures_are_refused() {
    let bar = Bar::new();
    let no_isr = function(
        0x1042,
        bar.base,
        &[
            (CAP_VENDOR, vcap(pci::cfg_type::COMMON, 0, COMMON, 0x38)),
            (
                CAP_VENDOR,
                vcap(pci::cfg_type::NOTIFY, 0, NOTIFY, NOTIFY_LEN),
            ),
        ],
    );
    let f = probe(&no_isr, BDF).unwrap();
    // SAFETY: de BAR is een levende Vec in deze test.
    let e = unsafe { Pci::new(&no_isr, &f) }.err();
    assert_eq!(e, Some(Error::Missing { cfg_type: 3 }));

    let short_common = function(
        0x1042,
        bar.base,
        &[(CAP_VENDOR, vcap(pci::cfg_type::COMMON, 0, COMMON, 0x20))],
    );
    let f = probe(&short_common, BDF).unwrap();
    // SAFETY: zie boven.
    let e = unsafe { Pci::new(&short_common, &f) }.err();
    assert_eq!(e, Some(Error::Missing { cfg_type: 1 }));

    // BAR0 staat op 0: de firmware (of het board) wees hem niet toe.
    let bare = net(0);
    let f = probe(&bare, BDF).unwrap();
    // SAFETY: er wordt niets gelezen: de BAR is niet toegewezen.
    let e = unsafe { Pci::new(&bare, &f) }.err();
    assert_eq!(e, Some(Error::BarUnassigned { bar: 0 }));
    assert_eq!(f.command(&bare), 0, "nothing enabled on refusal");

    let other = function(0x1234, bar.base, &[]);
    let f = probe(&other, BDF).unwrap();
    // SAFETY: zie boven.
    let e = unsafe { Pci::new(&other, &f) }.err();
    assert_eq!(e, Some(Error::NotVirtio));
}

#[test]
fn mmio_checks_the_slot_and_reads_its_config() {
    let mut slot = vec![0u32; (mmio::SLOT_SIZE / 4) as usize];
    slot[0] = mmio::MAGIC;
    slot[1] = 2;
    slot[2] = 2;
    slot[0x100 / 4] = 0x0000_4000; // capacity lo
    slot[0x104 / 4] = 0x1; // capacity hi
    slot[0x60 / 4] = 1; // interrupt status
    let base = Pa(slot.as_mut_ptr() as usize as u64);
    // SAFETY: de Vec leeft de hele test en is een heel slot groot.
    let m = unsafe { Mmio::new(base) };
    assert_eq!(m.check(), Ok(()));
    // SAFETY: zie boven.
    assert!(unsafe { mmio::is_modern(base, 2) });
    // SAFETY: zie boven.
    assert!(!unsafe { mmio::is_modern(base, 1) });
    assert_eq!(m.config_read64(0), Some(0x1_0000_4000));
    assert_eq!(m.config_read8(1), 0x40);
    assert_eq!(m.config_read32(0x100), u32::MAX, "past the slot");
    assert_eq!(m.ack_interrupt(), 1);
    assert_eq!(slot[0x64 / 4], 1, "acked");

    let mut m = m;
    slot[0x34 / 4] = 1024;
    m.select_queue(1);
    assert_eq!(slot[0x30 / 4], 1);
    assert_eq!(m.queue_num_max(), 1024);
    m.set_queue_addrs(Pa(0x2_0000_1000), Pa(0x2000), Pa(0x3000));
    m.enable_queue();
    m.notify(1);
    assert_eq!(
        (
            slot[0x80 / 4],
            slot[0x84 / 4],
            slot[0x90 / 4],
            slot[0xa0 / 4]
        ),
        (0x1000, 2, 0x2000, 0x3000)
    );
    assert_eq!((slot[0x44 / 4], slot[0x50 / 4]), (1, 1));

    slot[1] = 1;
    assert_eq!(m.check(), Err(Error::Legacy { version: 1 }));
    slot[0] = 0;
    assert_eq!(m.check(), Err(Error::NotVirtio));
}

/// Een transport zonder device: alleen generatie en status, om de
/// standaardmethoden te toetsen.
struct Gen {
    /// De generatie die elke lees teruggeeft, in volgorde; daarna de laatste.
    gens: Vec<u32>,
    reads: Cell<usize>,
    status: Cell<u8>,
    /// Hoeveel statuslezen de reset duurt.
    reset_after: Cell<u32>,
}

impl Transport for Gen {
    fn device_id(&self) -> u32 {
        0
    }
    fn device_features(&self, _: u32) -> u32 {
        0
    }
    fn set_driver_features(&self, _: u32, _: u32) {}
    fn status(&self) -> u8 {
        let n = self.reset_after.get();
        if n == 0 {
            return self.status.get();
        }
        self.reset_after.set(n - 1);
        0xff
    }
    fn set_status(&self, s: u8) {
        self.status.set(s);
    }
    fn select_queue(&mut self, _: u16) {}
    fn queue_num_max(&self) -> u16 {
        0
    }
    fn set_queue_num(&self, _: u16) {}
    fn set_queue_addrs(&self, _: Pa, _: Pa, _: Pa) {}
    fn enable_queue(&mut self) {}
    fn notify(&self, _: u16) {}
    fn ack_interrupt(&self) -> u32 {
        0
    }
    fn config_generation(&self) -> u32 {
        let i = self.reads.get();
        self.reads.set(i + 1);
        *self.gens.get(i).or(self.gens.last()).unwrap()
    }
    fn config_read8(&self, _: u32) -> u8 {
        0
    }
    fn config_read32(&self, off: u32) -> u32 {
        off + self.reads.get() as u32
    }
}

fn gen_of(gens: Vec<u32>) -> Gen {
    Gen {
        gens,
        reads: Cell::new(0),
        status: Cell::new(0),
        reset_after: Cell::new(0),
    }
}

#[test]
fn config_read64_rereads_when_the_generation_changes() {
    // Eerste poging: 0 ervoor, 1 erna; tweede: 1 en 1.
    let t = gen_of(vec![0, 1, 1, 1]);
    assert_eq!(t.config_read64(8), Some((15u64 << 32) | 11));
    assert_eq!(t.reads.get(), 4);
    // Een device dat blijft wisselen geeft geen waarde.
    let t = gen_of((0..100).collect());
    assert_eq!(t.config_read64(0), None);
    assert_eq!(t.reads.get(), 2 * CONFIG_RETRIES as usize);
}

#[test]
fn reset_waits_for_the_status_to_read_zero() {
    let t = gen_of(vec![0]);
    t.status.set(status::DRIVER_OK);
    t.reset_after.set(10);
    assert!(t.reset());
    assert_eq!(t.reset_after.get(), 0);
    t.reset_after.set(RESET_POLLS);
    assert!(!t.reset(), "a device that never comes back");
}
