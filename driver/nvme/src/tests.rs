//! De driver tegen een nep-controller: registers en DMA in RAM, en de klok
//! van de test IS de controller. Elke keer dat de driver op de klok kijkt,
//! spiegelt de controller CC.EN in CSTS.RDY, voert wat er achter de
//! doorbells staat uit op een schijf in RAM, en zet completions met de
//! juiste phase terug. Zo draait `new` van reset tot I/O-queue, en elke
//! read, write en flush over echte SQ's, CQ's en PRP-lijsten, via het ene
//! blokcontract (`start` en `poll_done`, afgedraaid met `block_on`).

use super::*;
use blkdev::{AsyncBlockDevice, BlockIo, Paced, Spin, block_on};
use std::cell::RefCell;
use std::vec;
use std::vec::Vec;

const BS: u64 = 512;
const NBLOCKS: u64 = 4096;

/// Eén nep-queue: SQ-basis, CQ-basis, wat we al zagen, onze CQ-tail en
/// de phase.
type FakeQueue = (Pa, Pa, u16, u16, bool);

#[derive(Default)]
struct Ctl {
    regs: Pa,
    disk: Vec<u8>,
    /// Per queue de staat van de controller.
    q: [Option<FakeQueue>; 2],
    /// Voert niets uit (een controller die weg is).
    mute: bool,
    /// Zet een verkeerde CID terug.
    wrong_cid: bool,
    /// De blokmaat die de namespace meldt (LBADS).
    lbads: u8,
    mdts: u8,
    /// Wat er uitgevoerd werd: (opcode, lba, nlb, prp2).
    log: Vec<(u8, u64, u32, u64)>,
    now: u64,
}

thread_local! {
    static CTL: RefCell<Ctl> = RefCell::new(Ctl::default());
}

fn db(regs: Pa, n: u64) -> Pa {
    regs.add(DB + (n << 2))
}

/// Leest de data van een opdracht uit zijn PRP's (of schrijft hem erheen).
fn pages(prp1: u64, prp2: u64, len: u64) -> Vec<u64> {
    let n = len.div_ceil(PAGE);
    let mut v = vec![prp1];
    if n == 2 {
        v.push(prp2);
    } else if n > 2 {
        for i in 0..n - 1 {
            v.push(dev::read64(Pa(prp2 + i * 8)));
        }
    }
    v
}

fn exec(c: &mut Ctl, qi: usize, sqe: Pa) -> u16 {
    let cdw0 = dev::read32(sqe);
    let opc = (cdw0 & 0xff) as u8;
    let prp1 = dev::read64(sqe.add(24));
    let prp2 = dev::read64(sqe.add(32));
    let cdw10 = dev::read32(sqe.add(40));
    let cdw11 = dev::read32(sqe.add(44));
    let cdw12 = dev::read32(sqe.add(48));
    if qi == 0 {
        match opc {
            ADM_IDENTIFY if cdw10 == 1 => {
                dev::clear(Pa(prp1), 4096);
                dev::copy_in(Pa(prp1 + 24), b"HopOS Fake NVMe                         ");
                dev::write8(Pa(prp1 + 77), c.mdts);
            }
            ADM_IDENTIFY => {
                dev::clear(Pa(prp1), 4096);
                dev::write64(Pa(prp1), NBLOCKS);
                dev::write8(Pa(prp1 + 26), 1); // LBAF 1 actief
                dev::write32(Pa(prp1 + 132), u32::from(c.lbads) << 16);
            }
            ADM_CREATE_CQ => {
                c.q[1] = Some((Pa(0), Pa(prp1), 0, 0, true));
            }
            ADM_CREATE_SQ => {
                if let Some(q) = c.q[1].as_mut() {
                    q.0 = Pa(prp1);
                }
                assert_eq!(cdw11 >> 16, 1, "SQ bound to CQ 1");
            }
            _ => return 1 << 1, // invalid opcode
        }
        c.log.push((opc, 0, 0, 0));
        return 0;
    }
    let lba = u64::from(cdw10) | (u64::from(cdw11) << 32);
    let nlb = cdw12 + 1;
    let bs = 1u64 << c.lbads;
    c.log.push((opc, lba, nlb, prp2));
    let len = u64::from(nlb) * bs;
    if opc == IO_FLUSH {
        return 0;
    }
    let mut off = (lba * bs) as usize;
    let mut left = len;
    for p in pages(prp1, prp2, len) {
        let n = left.min(PAGE) as usize;
        match opc {
            IO_READ => dev::copy_in(Pa(p), &c.disk[off..off + n]),
            IO_WRITE => dev::copy_out(&mut c.disk[off..off + n], Pa(p)),
            _ => return 1 << 1,
        }
        off += n;
        left -= n as u64;
    }
    0
}

fn clock() -> u64 {
    CTL.with(|c| {
        let mut c = c.borrow_mut();
        c.now += 1_000_000;
        let regs = c.regs;
        if regs.0 == 0 {
            return c.now;
        }
        // CSTS.RDY volgt CC.EN; bij het aanzetten komen de admin-queues.
        let en = dev::read32(regs.add(0x14)) & CC_EN;
        if en != 0 && dev::read32(regs.add(0x1c)) & CSTS_RDY == 0 {
            c.q[0] = Some((
                Pa(dev::read64(regs.add(0x28))),
                Pa(dev::read64(regs.add(0x30))),
                0,
                0,
                true,
            ));
        }
        dev::write32(regs.add(0x1c), en);
        if c.mute {
            return c.now;
        }
        for qi in 0..2 {
            let Some((sq, cq, mut seen, mut tail, mut phase)) = c.q[qi] else {
                continue;
            };
            let new = dev::read32(db(regs, 2 * qi as u64)) as u16;
            while seen != new {
                let sqe = sq.add(u64::from(seen) * 64);
                let cid = (dev::read32(sqe) >> 16) as u16;
                let st = exec(&mut c, qi, sqe);
                let e = cq.add(u64::from(tail) * 16);
                let cid = if c.wrong_cid { cid ^ 7 } else { cid };
                dev::write16(e.add(12), cid);
                dev::write16(e.add(14), (st << 1) | u16::from(phase));
                tail = (tail + 1) % Q_ENTRIES;
                if tail == 0 {
                    phase = !phase;
                }
                seen = (seen + 1) % Q_ENTRIES;
            }
            // Wat `exec` aan deze queue veranderde (de SQ-basis van queue 1)
            // blijft staan; alleen de voortgang is van hier.
            if let Some(q) = c.q[qi].as_mut() {
                (q.2, q.3, q.4) = (seen, tail, phase);
            }
        }
        c.now
    })
}

struct Mem {
    _regs: Vec<u64>,
    _dma: Vec<u64>,
    base: Pa,
    dma: Pa,
}

fn mem(cap: u64, lbads: u8, mdts: u8) -> Mem {
    let mut regs = vec![0u64; MMIO_LEN as usize / 8];
    let mut dma = vec![0u64; DMA_NEED as usize / 8 + 512];
    let base = Pa(regs.as_mut_ptr() as usize as u64);
    let dma_pa = Pa((dma.as_mut_ptr() as usize as u64).next_multiple_of(PAGE));
    dev::write64(base, cap);
    CTL.with(|c| {
        *c.borrow_mut() = Ctl {
            regs: base,
            disk: vec![0; (NBLOCKS << lbads) as usize],
            lbads,
            mdts,
            ..Ctl::default()
        };
    });
    Mem {
        _regs: regs,
        _dma: dma,
        base,
        dma: dma_pa,
    }
}

/// MQES 1023, DSTRD 0, MPSMIN 0.
const CAP: u64 = 1023;

fn up(m: &Mem) -> Nvme {
    // SAFETY: registers en DMA liggen in `m`, dat de test overleeft.
    unsafe { Nvme::new(m.base, m.dma, DMA_NEED, clock) }.unwrap()
}

/// De driver zoals hopfs hem ziet, met een `Pace` die pollt.
fn blk(n: &mut Nvme) -> Paced<&mut Nvme, Spin> {
    Paced::new(n, Spin)
}

/// Eén opdracht over hetzelfde pad (`start_op`, `poll_op`), met de fout van
/// de driver in plaats van die van het contract; `lba` in blokken van de
/// namespace.
fn raw(n: &mut Nvme, op: Op<'_>, lba: u64) -> Result {
    n.start_op(op, lba)?;
    block_on(core::future::poll_fn(|_| n.poll_op(&mut [])))
}

fn log() -> Vec<(u8, u64, u32, u64)> {
    CTL.with(|c| c.borrow().log.clone())
}

#[test]
fn new_identifies_and_creates_the_io_queues() {
    let m = mem(CAP, 9, 0);
    let n = up(&m);
    assert_eq!((n.block_size(), n.blocks()), (BS, NBLOCKS));
    assert_eq!(n.max_transfer(), MAX_TRANSFER);
    assert_eq!(n.model(), "HopOS Fake NVMe");
    let ops: Vec<u8> = log().iter().map(|e| e.0).collect();
    assert_eq!(
        ops,
        vec![ADM_IDENTIFY, ADM_IDENTIFY, ADM_CREATE_CQ, ADM_CREATE_SQ]
    );
    assert_eq!(dev::read32(m.base.add(0x24)), (63 << 16) | 63);
    assert_eq!(dev::read64(m.base.add(0x28)), m.dma.0);
    assert_eq!(dev::read32(m.base.add(0x14)), CC_EN | CC_IOSQES | CC_IOCQES);
    // De admin-doorbells: SQ-tail 4, CQ-head 4.
    assert_eq!(dev::read32(m.base.add(0x1000)), 4);
    assert_eq!(dev::read32(m.base.add(0x1004)), 4);
}

#[test]
fn write_read_flush_round_trip_over_prp_lists() {
    let m = mem(CAP, 9, 0);
    let mut n = up(&m);
    for len in [512usize, 4096, 8192, 12288, MAX_TRANSFER as usize] {
        let data: Vec<u8> = (0..len).map(|i| (i * 13 + len) as u8).collect();
        block_on(blk(&mut n).write(16, &data)).unwrap();
        let mut got = vec![0u8; len];
        block_on(blk(&mut n).read(16, &mut got)).unwrap();
        assert!(got == data, "{len} bytes differ");
    }
    block_on(blk(&mut n).flush()).unwrap();
    let io: Vec<(u8, u64, u32)> = log().iter().skip(4).map(|e| (e.0, e.1, e.2)).collect();
    assert_eq!(io.first(), Some(&(IO_WRITE, 16, 1)));
    assert_eq!(io.last(), Some(&(IO_FLUSH, 0, 1)));
    // PRP2: nul voor één pagina, de tweede pagina voor twee, de lijst
    // daarboven.
    let prp2: Vec<u64> = log()
        .iter()
        .skip(4)
        .step_by(2)
        .take(5)
        .map(|e| e.3)
        .collect();
    let data = m.dma.0 + DATA_OFF;
    let list = m.dma.0 + PRP_OFF;
    assert_eq!(prp2, vec![0, 0, data + PAGE, list, list]);
    assert_eq!(n.commands, 4 + 11);
}

#[test]
fn the_contract_round_trips_4k_blocks() {
    // Een namespace met 4096-byte-blokken en MDTS 2 (16 KiB per opdracht):
    // het contract rekent in 512 bytes, de driver om, en de brokken van
    // `Paced` volgen de transfergrens van de controller.
    let m = mem(CAP, 12, 2);
    let mut p = Paced::new(up(&m), Spin);
    let data: Vec<u8> = (0..40960u32).map(|i| (i * 7) as u8).collect();
    block_on(p.write(24, &data)).unwrap();
    let mut got = vec![0u8; data.len()];
    block_on(p.read(24, &mut got)).unwrap();
    assert!(got == data, "read back differs");
    block_on(p.flush()).unwrap();
    let io: Vec<(u8, u64, u32)> = log().iter().skip(4).map(|e| (e.0, e.1, e.2)).collect();
    assert_eq!(
        io,
        vec![
            (IO_WRITE, 3, 4),
            (IO_WRITE, 7, 4),
            (IO_WRITE, 11, 2),
            (IO_READ, 3, 4),
            (IO_READ, 7, 4),
            (IO_READ, 11, 2),
            (IO_FLUSH, 0, 1),
        ]
    );
    // Een LBA die niet op een blok valt, gaat niet naar de controller.
    assert_eq!(
        block_on(p.read(25, &mut [0; 4096])),
        Err(blkdev::Error::OutOfRange { lba: 25, len: 4096 })
    );
}

#[test]
fn an_abandoned_command_blocks_the_next_until_it_is_back() {
    let m = mem(CAP, 9, 0);
    let mut n = up(&m);
    CTL.with(|c| c.borrow_mut().mute = true);
    n.start(Op::Read { lba: 1, len: 512 }).unwrap();
    assert_eq!(n.start(Op::Flush), Err(blkdev::Error::Busy));
    assert_eq!(
        n.start(Op::Write {
            lba: 2,
            data: &[1; 512]
        }),
        Err(blkdev::Error::Busy)
    );
    CTL.with(|c| c.borrow_mut().mute = false);
    clock(); // De controller haalt in.
    block_on(blk(&mut n).write(2, &[1; 512])).unwrap();
    let ops: Vec<u8> = log().iter().skip(4).map(|e| e.0).collect();
    assert_eq!(ops, vec![IO_READ, IO_WRITE]);
}

#[test]
fn big_requests_split_at_the_transfer_limit() {
    // MDTS 2: vier pagina's van 4 KB per opdracht.
    let m = mem(CAP, 12, 2);
    let mut n = up(&m);
    assert_eq!((n.block_size(), n.max_transfer()), (4096, 16384));
    let data = vec![0xa5u8; 40960];
    block_on(blk(&mut n).write(3 * 8, &data)).unwrap(); // blok 3
    let io: Vec<(u64, u32)> = log().iter().skip(4).map(|e| (e.1, e.2)).collect();
    assert_eq!(io, vec![(3, 4), (7, 4), (11, 2)]);
}

#[test]
fn invalid_transfers_never_reach_the_controller() {
    let m = mem(CAP, 9, 0);
    let mut n = up(&m);
    let before = log().len();
    for (lba, len) in [
        (0u64, 0usize),
        (0, 100),
        (NBLOCKS, 512),
        (NBLOCKS - 1, 1024),
        (u64::MAX, 512),
    ] {
        assert_eq!(
            n.start(Op::Write {
                lba,
                data: &vec![0; len]
            }),
            Err(blkdev::Error::OutOfRange { lba, len }),
            "lba {lba} len {len}"
        );
    }
    assert_eq!(
        block_on(blk(&mut n).read(NBLOCKS, &mut [0; 512])),
        Err(blkdev::Error::OutOfRange {
            lba: NBLOCKS,
            len: 512
        })
    );
    assert_eq!(log().len(), before);
}

/// `correctness_test.go`: een ontbrekende completion maakt de controller
/// dood, en een late completion geeft de oude DMA-buffer niet vrij.
#[test]
fn a_timeout_retains_the_dma_buffer() {
    let m = mem(CAP, 9, 0);
    let mut n = up(&m);
    CTL.with(|c| c.borrow_mut().mute = true);
    assert_eq!(
        raw(&mut n, Op::Read { lba: 0, len: 512 }, 0),
        Err(Error::Timeout { opc: IO_READ })
    );
    CTL.with(|c| c.borrow_mut().mute = false);
    let data = m.dma.add(DATA_OFF);
    dev::write8(data, 0x11);
    let mut payload = [0u8; 512];
    payload[0] = 99;
    assert_eq!(
        raw(
            &mut n,
            Op::Write {
                lba: 1,
                data: &payload
            },
            1
        ),
        Err(Error::Dead)
    );
    assert_eq!(dev::read8(data), 0x11, "DMA buffer reused after a timeout");
    assert_eq!(n.io.tail, 1, "queue reused after a timeout");
    assert_eq!(block_on(blk(&mut n).flush()), Err(blkdev::Error::Dead));
}

#[test]
fn a_foreign_completion_kills_the_driver() {
    let m = mem(CAP, 9, 0);
    let mut n = up(&m);
    CTL.with(|c| c.borrow_mut().wrong_cid = true);
    assert!(matches!(raw(&mut n, Op::Flush, 0), Err(Error::Cid { .. })));
    assert_eq!(block_on(blk(&mut n).flush()), Err(blkdev::Error::Dead));
}

#[test]
fn the_controller_is_checked_before_it_is_trusted() {
    // `dma_layout_test.go`: een onvolledige reservering valt af vóór de
    // eerste MMIO (basis 0 is ongemapt).
    for (dma, size) in [
        (0x20_0000u64, DMA_NEED - 1),
        (0x20_0100, DMA_NEED),
        (0, DMA_NEED),
    ] {
        // SAFETY: de toets faalt vóór er een register wordt aangeraakt.
        let e = unsafe { Nvme::new(Pa(0), Pa(dma), size, clock) }.err();
        assert_eq!(e, Some(Error::Dma { base: dma, size }));
    }
    let m = mem(u64::MAX, 9, 0);
    // SAFETY: registers en DMA liggen in `m`.
    let e = unsafe { Nvme::new(m.base, m.dma, DMA_NEED, clock) }.err();
    assert_eq!(e, Some(Error::OffBus));
    let big_pages = CAP | (1 << 48);
    let m = mem(big_pages, 9, 0);
    // SAFETY: zie hierboven.
    let e = unsafe { Nvme::new(m.base, m.dma, DMA_NEED, clock) }.err();
    assert_eq!(e, Some(Error::Unsupported { cap: big_pages }));
    let m = mem(CAP, 8, 0); // blokken van 256 bytes
    // SAFETY: zie hierboven.
    let e = unsafe { Nvme::new(m.base, m.dma, DMA_NEED, clock) }.err();
    assert_eq!(
        e,
        Some(Error::Namespace {
            blocks: NBLOCKS,
            lbads: 8
        })
    );
}

#[test]
fn the_doorbell_stride_follows_cap() {
    let m = mem(CAP, 9, 0);
    let mut n = Nvme::at(m.base, m.dma, clock);
    n.dstrd = 2;
    assert_eq!(n.doorbell(&n.io, false), m.base.add(0x1000 + (2 << 4)));
    assert_eq!(n.doorbell(&n.io, true), m.base.add(0x1000 + (3 << 4)));
    // De grootste stride past in het gemapte venster.
    let last = DB + (3u64 << (2 + MAX_DSTRD));
    assert!(last + 4 <= MMIO_LEN);
}

#[test]
fn the_dma_layout_keeps_the_cached_block_apart() {
    // `dma_layout_test.go`: de queues en de PRP-lijst liggen vóór het
    // datablok, dat hele blokken van 2 MB beslaat.
    for (off, len) in [
        (ASQ_OFF, 64 * 64),
        (ACQ_OFF, 64 * 16),
        (IOSQ_OFF, 64 * 64),
        (IOCQ_OFF, 64 * 16),
        (PRP_OFF, PAGE),
    ] {
        assert!(off + len <= DATA_OFF);
    }
    let m = mem(CAP, 9, 0);
    let n = up(&m);
    assert_eq!(n.data_region(), (m.dma.add(DATA_OFF), 2 << 20));
}

#[test]
fn the_block_contract_speaks_512_byte_sectors_on_4k_namespaces() {
    let m = mem(CAP, 12, 0);
    let mut n = up(&m);
    assert_eq!(
        (n.block_size(), n.blocks(), n.sectors()),
        (4096, NBLOCKS, NBLOCKS * 8)
    );
    let data = vec![0x3cu8; 8192];
    block_on(blk(&mut n).write(16, &data)).unwrap(); // sector 16 = blok 2
    let io = log().last().copied().unwrap();
    assert_eq!((io.0, io.1, io.2), (IO_WRITE, 2, 2));
    let mut got = vec![0u8; 8192];
    block_on(blk(&mut n).read(16, &mut got)).unwrap();
    assert!(got == data);
    // Niet op een blok: geweigerd vóór de controller.
    for (lba, len) in [(3u64, 4096usize), (8, 512)] {
        assert_eq!(
            block_on(blk(&mut n).write(lba, &vec![0; len])),
            Err(blkdev::Error::OutOfRange { lba, len })
        );
    }
}
