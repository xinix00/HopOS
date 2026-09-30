//! De driver op nep-geheugen: de klok van de test IS het device. Elke keer
//! dat de driver op de used-ring wacht, leest de klok de keten die de driver
//! klaarzette en voert hem uit op een schijf in RAM, zoals QEMU dat doet.
//!
//! Alles gaat over het ene blokcontract: `start` en `poll_done`, met
//! `Paced` en een pollende `Pace` afgedraaid door `block_on`, zoals de
//! meetbank vóór de executor.

use super::*;
use blkdev::{AsyncBlockDevice, BlockIo, Paced, Spin, block_on};
use std::cell::{Cell, RefCell};
use std::vec;
use std::vec::Vec;

/// Het nep-device: waar de DMA-regio ligt, de schijf, en wat het zag.
struct Device {
    dma: Pa,
    disk: Vec<u8>,
    seen: u16,
    /// De ketens die het device uitvoerde: (soort, sector, datalengte,
    /// flags van de data-descriptor).
    log: Vec<(u32, u64, u32, u16)>,
    /// Antwoord nooit (een device dat weg is).
    mute: bool,
    now: u64,
}

thread_local! {
    static DEV: RefCell<Option<Device>> = const { RefCell::new(None) };
}

fn desc(dma: Pa, i: u16) -> (u64, u32, u16, u16) {
    let d = dma.add(DESC_OFF + u64::from(i) * 16);
    (
        dev::read64(d),
        dev::read32(d.add(8)),
        dev::read16(d.add(12)),
        dev::read16(d.add(14)),
    )
}

/// De klok: elke lees voert wat klaarstaat uit en schuift de tijd een
/// milliseconde op.
fn clock() -> u64 {
    DEV.with(|d| {
        let mut d = d.borrow_mut();
        let Some(d) = d.as_mut() else { return 0 };
        d.now += 1_000_000;
        let avail = dev::read16(d.dma.add(AVAIL_OFF + 2));
        while !d.mute && d.seen != avail {
            let head = dev::read16(d.dma.add(AVAIL_OFF + 4 + u64::from(d.seen % QSIZE) * 2));
            let (hdr, hlen, hflags, mut next) = desc(d.dma, head);
            assert_eq!(
                (hlen, hflags & DESC_WRITE),
                (16, 0),
                "header is device-readable"
            );
            let kind = dev::read32(Pa(hdr));
            let sector = dev::read64(Pa(hdr + 8));
            let (mut dlen, mut dflags) = (0, 0);
            if hflags & DESC_NEXT != 0 && next == 1 {
                let (addr, len, flags, n) = desc(d.dma, 1);
                (dlen, dflags, next) = (len, flags, n);
                let off = (sector * SECTOR) as usize;
                let span = off..off + len as usize;
                match kind {
                    T_IN => dev::copy_in(Pa(addr), &d.disk[span]),
                    T_OUT => dev::copy_out(&mut d.disk[span], Pa(addr)),
                    _ => panic!("data with kind {kind}"),
                }
            }
            let (st, slen, sflags, _) = desc(d.dma, next);
            assert_eq!(
                (slen, sflags),
                (1, DESC_WRITE),
                "status is last and writable"
            );
            dev::write8(Pa(st), S_OK);
            let used = d.dma.add(USED_OFF);
            let idx = dev::read16(used.add(2));
            dev::write32(used.add(4 + u64::from(idx % QSIZE) * 8), u32::from(head));
            dev::write16(used.add(2), idx.wrapping_add(1));
            d.seen = d.seen.wrapping_add(1);
            d.log.push((kind, sector, dlen, dflags));
        }
        d.now
    })
}

/// Zet het nep-device klaar op de DMA-regio `dma` met een schijf van
/// `sectors`.
fn install(dma: Pa, sectors: u64) {
    DEV.with(|d| {
        *d.borrow_mut() = Some(Device {
            dma,
            disk: vec![0; (sectors * SECTOR) as usize],
            seen: 0,
            log: Vec::new(),
            mute: false,
            now: 0,
        });
    });
}

/// Een driver op een nep-regio van `dma` met een schijf van `sectors`,
/// zonder `new` (geen registers om mee te onderhandelen), zoals de test van
/// virtio-net.
fn fake(sectors: u64, flush: bool) -> (VirtioBlk, Vec<u64>, Vec<u64>) {
    let mut regs = vec![0u64; 64];
    let mut mem = vec![0u64; DMA_NEED as usize / 8];
    let dma = Pa(mem.as_mut_ptr() as usize as u64);
    install(dma, sectors);
    let b = VirtioBlk {
        // SAFETY: `regs` gaat met de driver mee terug naar de test en is een
        // heel slot groot (512 bytes); alleen QueueNotify en de
        // interrupt-registers worden geraakt.
        t: unsafe { Mmio::new(Pa(regs.as_mut_ptr() as usize as u64)) },
        dma,
        sectors,
        flush,
        read_only: false,
        clock,
        avail_idx: 0,
        last_used: 0,
        dead: false,
        pending: None,
        irq: None,
        requests: 0,
        slowest_ns: 0,
    };
    (b, regs, mem)
}

/// De driver zoals hopfs hem ziet, met een `Pace` die pollt.
fn blk<T: Transport>(b: &mut VirtioBlk<T>) -> Paced<&mut VirtioBlk<T>, Spin> {
    Paced::new(b, Spin)
}

fn seen() -> Vec<(u32, u64, u32, u16)> {
    DEV.with(|d| d.borrow().as_ref().unwrap().log.clone())
}

#[test]
fn write_then_read_round_trips_through_the_chain() {
    let (mut b, _r, _m) = fake(64, true);
    let data: Vec<u8> = (0..4096u32).map(|i| (i * 7) as u8).collect();
    block_on(blk(&mut b).write(8, &data)).unwrap();
    let mut got = vec![0u8; 4096];
    block_on(blk(&mut b).read(8, &mut got)).unwrap();
    assert!(got == data, "read back differs");
    block_on(blk(&mut b).flush()).unwrap();
    assert_eq!(
        seen(),
        vec![
            (T_OUT, 8, 4096, DESC_NEXT),
            (T_IN, 8, 4096, DESC_NEXT | DESC_WRITE),
            (T_FLUSH, 0, 0, 0),
        ]
    );
    assert_eq!(b.requests, 3);
}

#[test]
fn large_transfers_split_at_the_dma_buffer() {
    let sectors = (3 * MAX_TRANSFER as u64) / SECTOR;
    let (mut b, _r, _m) = fake(sectors, false);
    let data = vec![0x5au8; 2 * MAX_TRANSFER + 512];
    block_on(blk(&mut b).write(0, &data)).unwrap();
    let per = MAX_TRANSFER as u64 / SECTOR;
    let log: Vec<(u64, u32)> = seen().iter().map(|e| (e.1, e.2)).collect();
    assert_eq!(
        log,
        vec![
            (0, MAX_TRANSFER as u32),
            (per, MAX_TRANSFER as u32),
            (2 * per, 512)
        ]
    );
    // Zonder FLUSH-feature is een flush niets.
    block_on(blk(&mut b).flush()).unwrap();
    assert_eq!(seen().len(), 3);
}

#[test]
fn out_of_range_and_unaligned_requests_never_reach_the_device() {
    let (mut b, _r, _m) = fake(16, true);
    assert_eq!(
        block_on(blk(&mut b).write(15, &[0; 1024])),
        Err(blkdev::Error::OutOfRange { lba: 15, len: 1024 })
    );
    assert_eq!(
        block_on(blk(&mut b).read(0, &mut [0; 100])),
        Err(blkdev::Error::OutOfRange { lba: 0, len: 100 })
    );
    assert_eq!(
        b.start(Op::Read {
            lba: u64::MAX,
            len: 512
        }),
        Err(blkdev::Error::OutOfRange {
            lba: u64::MAX,
            len: 512
        })
    );
    assert_eq!(
        b.start(Op::Read {
            lba: 0,
            len: MAX_TRANSFER + 512
        }),
        Err(blkdev::Error::OutOfRange {
            lba: 0,
            len: MAX_TRANSFER + 512
        })
    );
    assert!(seen().is_empty());
}

#[test]
fn a_silent_device_kills_the_driver_loudly() {
    let (mut b, _r, _m) = fake(16, true);
    DEV.with(|d| d.borrow_mut().as_mut().unwrap().mute = true);
    // Onder het contract: de driverfout, dan `Dead` voor hopfs.
    b.start_op(Op::Read { lba: 2, len: 512 }).unwrap();
    assert_eq!(
        block_on(core::future::poll_fn(|_| b.poll_op(&mut [0; 512]))),
        Err(Error::Timeout { sector: 2 })
    );
    DEV.with(|d| d.borrow_mut().as_mut().unwrap().mute = false);
    // Het verzoek kan nog lopen: niets gaat meer naar het device.
    assert_eq!(
        block_on(blk(&mut b).read(2, &mut [0; 512])),
        Err(blkdev::Error::Dead)
    );
    assert_eq!(
        block_on(blk(&mut b).write(3, &[0; 512])),
        Err(blkdev::Error::Dead)
    );
}

#[test]
fn a_request_submits_then_completes_and_copies_a_read_out() {
    let (b, _r, _m) = fake(64, true);
    let mut p = Paced::new(b, Spin);
    let data: Vec<u8> = (0..8192u32).map(|i| (i * 13) as u8).collect();
    block_on(p.write(16, &data)).unwrap();
    let mut got = vec![0u8; 8192];
    block_on(p.read(16, &mut got)).unwrap();
    assert!(got == data, "read back differs");
    block_on(p.flush()).unwrap();
    assert_eq!(
        seen(),
        vec![
            (T_OUT, 16, 8192, DESC_NEXT),
            (T_IN, 16, 8192, DESC_NEXT | DESC_WRITE),
            (T_FLUSH, 0, 0, 0),
        ]
    );
    // Eén keer kijken is niet genoeg: het device (de klok) moet eerst lopen.
    let b = p.dev_mut();
    b.start(Op::Flush).unwrap();
    assert!(b.poll_done(&mut []).is_pending());
    assert_eq!(b.poll_done(&mut []), Poll::Ready(Ok(())));
}

#[test]
fn an_abandoned_request_blocks_the_next_until_it_is_back() {
    let (mut b, _r, _m) = fake(16, true);
    DEV.with(|d| d.borrow_mut().as_mut().unwrap().mute = true);
    b.start(Op::Read { lba: 1, len: 512 }).unwrap();
    // De wachter ging weg; het device antwoordt niet: niets nieuws erbij.
    assert_eq!(
        b.start(Op::Write {
            lba: 2,
            data: &[1; 512]
        }),
        Err(blkdev::Error::Busy)
    );
    assert_eq!(b.start(Op::Flush), Err(blkdev::Error::Busy));
    DEV.with(|d| d.borrow_mut().as_mut().unwrap().mute = false);
    clock(); // Het device haalt in.
    // Nu komt het oude verzoek terug (en wordt weggegooid), dan het nieuwe.
    block_on(blk(&mut b).write(2, &[1; 512])).unwrap();
    assert_eq!(
        seen().iter().map(|e| e.0).collect::<Vec<_>>(),
        [T_IN, T_OUT]
    );
}

/// Een nep-transport: de registers als velden, zodat de init over elke
/// `Transport` getoetst wordt; het device zelf is de klok, zoals hierboven.
struct FakeT {
    id: u32,
    offered: u32,
    capacity: u64,
    max: u16,
    status: Cell<u8>,
    features: RefCell<Vec<(u32, u32)>>,
    /// De queue: (grootte, desc, avail, used, aan).
    queue: RefCell<(u16, Pa, Pa, Pa, bool)>,
    notified: Cell<u32>,
}

impl FakeT {
    fn new(id: u32, max: u16) -> Self {
        Self {
            id,
            offered: FEAT_FLUSH | 1 << 1,
            capacity: 64,
            max,
            status: Cell::new(0),
            features: RefCell::new(Vec::new()),
            queue: RefCell::new((0, Pa(0), Pa(0), Pa(0), false)),
            notified: Cell::new(0),
        }
    }
}

impl Transport for FakeT {
    fn device_id(&self) -> u32 {
        self.id
    }
    fn device_features(&self, window: u32) -> u32 {
        if window == 0 { self.offered } else { 1 }
    }
    fn set_driver_features(&self, window: u32, bits: u32) {
        self.features.borrow_mut().push((window, bits));
    }
    fn status(&self) -> u8 {
        self.status.get()
    }
    fn set_status(&self, s: u8) {
        self.status.set(s);
    }
    fn select_queue(&mut self, q: u16) {
        assert_eq!(q, 0);
    }
    fn queue_num_max(&self) -> u16 {
        self.max
    }
    fn set_queue_num(&self, n: u16) {
        self.queue.borrow_mut().0 = n;
    }
    fn set_queue_addrs(&self, d: Pa, a: Pa, u: Pa) {
        let mut q = self.queue.borrow_mut();
        (q.1, q.2, q.3) = (d, a, u);
    }
    fn enable_queue(&mut self) {
        self.queue.borrow_mut().4 = true;
    }
    fn notify(&self, q: u16) {
        assert_eq!(q, 0);
        self.notified.set(self.notified.get() + 1);
    }
    fn ack_interrupt(&self) -> u32 {
        0
    }
    fn config_generation(&self) -> u32 {
        0
    }
    fn config_read8(&self, _: u32) -> u8 {
        u8::MAX
    }
    fn config_read32(&self, off: u32) -> u32 {
        match off {
            0 => self.capacity as u32,
            4 => (self.capacity >> 32) as u32,
            _ => u32::MAX,
        }
    }
}

#[test]
fn init_over_any_transport_then_a_round_trip() {
    let mut mem = vec![0u64; DMA_NEED as usize / 8];
    let dma = Pa(mem.as_mut_ptr() as usize as u64);
    install(dma, 64);
    // SAFETY: `mem` leeft de hele test en is `DMA_NEED` groot.
    let mut b =
        unsafe { VirtioBlk::with_transport(FakeT::new(2, 8), dma, DMA_NEED, clock) }.unwrap();
    assert_eq!((b.sectors(), b.can_flush()), (64, true));
    let t = &b.t;
    // Alleen FLUSH van wat geboden werd, en VERSION_1.
    assert_eq!(
        *t.features.borrow(),
        [(0, FEAT_FLUSH), (1, FEAT_VERSION_1_HI)]
    );
    assert_eq!(
        *t.queue.borrow(),
        (
            QSIZE,
            dma.add(DESC_OFF),
            dma.add(AVAIL_OFF),
            dma.add(USED_OFF),
            true
        )
    );
    assert_eq!(t.status.get() & status::DRIVER_OK, status::DRIVER_OK);

    let data = vec![0xa5u8; 1024];
    block_on(blk(&mut b).write(4, &data)).unwrap();
    let mut got = vec![0u8; 1024];
    block_on(blk(&mut b).read(4, &mut got)).unwrap();
    assert!(got == data, "read back differs");
    assert_eq!(b.t.notified.get(), 2, "one doorbell per request");
    assert_eq!(seen().len(), 2);
    drop(mem);
}

#[test]
fn init_refuses_wrong_devices_small_queues_and_small_dma() {
    let mut mem = vec![0u64; DMA_NEED as usize / 8];
    let dma = Pa(mem.as_mut_ptr() as usize as u64);
    // SAFETY: `mem` leeft de hele test en is `DMA_NEED` groot.
    let e = unsafe { VirtioBlk::with_transport(FakeT::new(1, 8), dma, DMA_NEED, clock) }.err();
    assert_eq!(e, Some(Error::NotBlock(1)));
    // SAFETY: zie boven.
    let e = unsafe { VirtioBlk::with_transport(FakeT::new(2, 2), dma, DMA_NEED, clock) }.err();
    assert_eq!(e, Some(Error::NoQueue(2)));
    // SAFETY: zie boven.
    let e = unsafe { VirtioBlk::with_transport(FakeT::new(2, 8), dma, 4096, clock) }.err();
    assert_eq!(
        e,
        Some(Error::DmaTooSmall {
            need: DMA_NEED,
            have: 4096
        })
    );
    drop(mem);
}
