//! De driver op nep-geheugen: de klok van de test IS het device. Elke keer
//! dat de driver op de used-ring wacht, leest de klok de keten die de driver
//! klaarzette en voert hem uit op een schijf in RAM, zoals QEMU dat doet.

use super::*;
use kern::hopfs::BlockDevice;
use std::cell::RefCell;
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

/// Een driver op een nep-regio van `dma` met een schijf van `sectors`,
/// zonder `new` (geen registers om mee te onderhandelen), zoals de test van
/// virtio-net.
fn fake(sectors: u64, flush: bool) -> (VirtioBlk, Vec<u64>, Vec<u64>) {
    let mut regs = vec![0u64; 64];
    let mut mem = vec![0u64; DMA_NEED as usize / 8];
    let dma = Pa(mem.as_mut_ptr() as usize as u64);
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
    let b = VirtioBlk {
        base: Pa(regs.as_mut_ptr() as usize as u64),
        dma,
        sectors,
        flush,
        read_only: false,
        clock,
        avail_idx: 0,
        last_used: 0,
        dead: false,
        requests: 0,
        slowest_ns: 0,
    };
    (b, regs, mem)
}

fn seen() -> Vec<(u32, u64, u32, u16)> {
    DEV.with(|d| d.borrow().as_ref().unwrap().log.clone())
}

#[test]
fn write_then_read_round_trips_through_the_chain() {
    let (mut b, _r, _m) = fake(64, true);
    let data: Vec<u8> = (0..4096u32).map(|i| (i * 7) as u8).collect();
    BlockDevice::write(&mut b, 8, &data).unwrap();
    let mut got = vec![0u8; 4096];
    BlockDevice::read(&mut b, 8, &mut got).unwrap();
    assert!(got == data, "read back differs");
    BlockDevice::flush(&mut b).unwrap();
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
    b.write_at(0, &data).unwrap();
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
    b.sync().unwrap();
    assert_eq!(seen().len(), 3);
}

#[test]
fn out_of_range_and_unaligned_requests_never_reach_the_device() {
    let (mut b, _r, _m) = fake(16, true);
    assert!(matches!(
        b.write_at(15, &[0; 1024]),
        Err(Error::Range { .. })
    ));
    assert!(matches!(
        b.read_at(0, &mut [0; 100]),
        Err(Error::Range { .. })
    ));
    assert!(matches!(
        b.read_at(u64::MAX, &mut [0; 512]),
        Err(Error::Range { .. })
    ));
    assert!(seen().is_empty());
}

#[test]
fn a_silent_device_kills_the_driver_loudly() {
    let (mut b, _r, _m) = fake(16, true);
    DEV.with(|d| d.borrow_mut().as_mut().unwrap().mute = true);
    assert_eq!(
        b.read_at(2, &mut [0; 512]),
        Err(Error::Timeout { sector: 2 })
    );
    DEV.with(|d| d.borrow_mut().as_mut().unwrap().mute = false);
    // Het verzoek kan nog lopen: niets gaat meer naar het device.
    assert_eq!(b.read_at(2, &mut [0; 512]), Err(Error::Dead));
    assert_eq!(
        BlockDevice::write(&mut b, 3, &[0; 512]),
        Err(kern::Error::Io { lba: 3 })
    );
}
