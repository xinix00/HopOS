//! De core tegen een nep-controller met een nagebootst transport
//! ([`Sim`]): registers en DMA in RAM, en de klok van de test IS de
//! controller. Bij elke blik op de klok spiegelt hij CC in CSTS (EN in RDY,
//! SHN in SHST), voert hij uit wat er klaarstaat (van de gezien-plek tot de
//! tail-deurbel van een ring, of de slots die een lineair transport
//! aanwees) op een schijf in RAM via de echte PRP's, en zet hij de
//! completions met de juiste phase terug, tenzij hij ze vasthoudt
//! (`hold`): dan is de DMA gebeurd maar niets bevestigd. De transports
//! (`pci`, `apple`) gebruiken dezelfde controller voor hun eigen toetsen.

use super::*;
use blkdev::{AsyncBlockDevice, BlockIo, Spin, block_on};
use std::cell::RefCell;
use std::vec;
use std::vec::Vec;

/// Eén queue van de nep-controller.
#[derive(Clone, Copy, Debug)]
pub(crate) struct FakeQ {
    pub(crate) sq: Pa,
    cq: Pa,
    /// Tot hier zag hij de SQ-ring.
    seen: u16,
    tail: u16,
    phase: bool,
}

/// Wat de controller uitvoerde.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) struct Exec {
    pub(crate) q: usize,
    pub(crate) opc: u8,
    /// CDW10 en CDW11: de LBA van een I/O-opdracht, de CNS van een identify.
    pub(crate) at: u64,
    /// Het aantal blokken (CDW12 + 1).
    pub(crate) nlb: u32,
    pub(crate) prp1: u64,
    pub(crate) prp2: u64,
    pub(crate) slot: u16,
    pub(crate) cid: u16,
}

#[derive(Default)]
pub(crate) struct Ctl {
    pub(crate) regs: Pa,
    pub(crate) disk: Vec<u8>,
    /// De blokmaat van de schijf (LBADS) en de MDTS van de identify.
    pub(crate) lbads: u8,
    pub(crate) mdts: u8,
    pub(crate) q: [Option<FakeQ>; 2],
    /// De slots die een lineair transport aanwees: (queue, slot).
    pub(crate) rung: Vec<(usize, u16)>,
    /// Vóór elke ronde (de ANS: zijn eigen registers).
    pub(crate) hook: Option<fn(&mut Ctl)>,
    /// Wordt hij ready bij deze CC? Zonder: altijd.
    pub(crate) ready_if: Option<fn(&Ctl, u32) -> bool>,
    pub(crate) booted: bool,
    /// Voert niets uit (een controller die weg is).
    pub(crate) mute: bool,
    /// Zet een verkeerde CID terug.
    pub(crate) wrong_cid: bool,
    /// Houdt de completions van de I/O-queue vast tot [`release`].
    pub(crate) hold: bool,
    /// Vastgehouden completions: (CID, SCT/SC, opcode).
    pub(crate) held: Vec<(u16, u16, u8)>,
    /// Bevestigde I/O-opdrachten, in volgorde: (opcode, CID).
    pub(crate) acked: Vec<(u8, u16)>,
    pub(crate) log: Vec<Exec>,
    pub(crate) events: Vec<&'static str>,
    /// Het transport: zo vaak bijgehouden, en of het omviel.
    pub(crate) services: u32,
    pub(crate) crashed: bool,
    now: u64,
}

thread_local! {
    static CTL: RefCell<Ctl> = RefCell::new(Ctl::default());
}

pub(crate) fn with<R>(f: impl FnOnce(&mut Ctl) -> R) -> R {
    CTL.with(|c| f(&mut c.borrow_mut()))
}

fn lo_hi(p: Pa) -> u64 {
    u64::from(dev::read32(p)) | (u64::from(dev::read32(p.add(4))) << 32)
}

/// De klok, en bij elke blik een ronde van de controller.
pub(crate) fn now() -> u64 {
    with(|c| {
        c.now += 1_000_000;
        tick(c);
        c.now
    })
}

fn tick(c: &mut Ctl) {
    let r = c.regs;
    if r.0 == 0 {
        return;
    }
    if let Some(h) = c.hook {
        h(c);
    }
    let cc = dev::read32(r.add(0x14));
    let rdy = cc & CC_EN != 0 && c.ready_if.is_none_or(|f| f(c, cc));
    let mut csts = dev::read32(r.add(0x1c));
    if rdy && csts & CSTS_RDY == 0 {
        // Bij het aanzetten komen de admin-queues.
        c.q[0] = Some(FakeQ {
            sq: Pa(lo_hi(r.add(0x28))),
            cq: Pa(lo_hi(r.add(0x30))),
            seen: 0,
            tail: 0,
            phase: true,
        });
    }
    csts = (csts & !CSTS_RDY) | u32::from(rdy);
    if cc & CC_SHN_MASK == 1 << 14 && cc & CC_EN != 0 {
        csts |= 2 << 2; // SHST: klaar
    }
    dev::write32(r.add(0x1c), csts);
    if c.mute || !rdy {
        return;
    }
    let dstrd = u64::from(dev::read32(r.add(4)) & 0xf);
    for qi in 0..2 {
        let Some(mut q) = c.q[qi] else {
            continue;
        };
        let mut slots: Vec<u16> = c.rung.iter().filter(|s| s.0 == qi).map(|s| s.1).collect();
        c.rung.retain(|s| s.0 != qi);
        let tail = dev::read32(r.add(DB + ((2 * qi as u64) << (2 + dstrd)))) as u16;
        while q.seen != tail {
            slots.push(q.seen);
            q.seen = (q.seen + 1) % Q_ENTRIES;
        }
        c.q[qi] = Some(q);
        for slot in slots {
            exec(c, qi, slot);
        }
    }
}

/// De pagina's van een opdracht uit zijn PRP's.
fn pages(prp1: u64, prp2: u64, len: u64) -> Vec<u64> {
    let n = len.div_ceil(PAGE);
    let mut v = vec![prp1];
    if n == 2 {
        v.push(prp2);
    } else if n > 2 {
        v.extend((0..n - 1).map(|i| dev::read64(Pa(prp2 + i * 8))));
    }
    v
}

fn exec(c: &mut Ctl, qi: usize, slot: u16) {
    let sqe = c.q[qi].unwrap().sq.add(u64::from(slot) * SQE);
    let cdw0 = dev::read32(sqe);
    let e = Exec {
        q: qi,
        opc: (cdw0 & 0xff) as u8,
        at: u64::from(dev::read32(sqe.add(40))) | (u64::from(dev::read32(sqe.add(44))) << 32),
        nlb: (dev::read32(sqe.add(48)) & 0xffff) + 1,
        prp1: dev::read64(sqe.add(24)),
        prp2: dev::read64(sqe.add(32)),
        slot,
        cid: (cdw0 >> 16) as u16,
    };
    c.log.push(e);
    let sc = if qi == 0 { admin(c, &e) } else { io(c, &e) };
    if qi == 1 && c.hold {
        c.held.push((e.cid, sc, e.opc));
    } else {
        complete(c, qi, e.cid, sc, e.opc);
    }
}

/// Zet de completion van `cid` in de CQ van queue `qi`.
fn complete(c: &mut Ctl, qi: usize, cid: u16, sc: u16, opc: u8) {
    if qi == 1 {
        c.acked.push((opc, cid));
    }
    let q = c.q[qi].as_mut().unwrap();
    let e = q.cq.add(u64::from(q.tail) * CQE);
    dev::write16(e.add(12), if c.wrong_cid { cid ^ 7 } else { cid });
    dev::write16(e.add(14), (sc << 1) | u16::from(q.phase));
    q.tail = (q.tail + 1) % Q_ENTRIES;
    if q.tail == 0 {
        q.phase = !q.phase;
    }
}

/// Bevestigt de vastgehouden opdracht met `cid`.
pub(crate) fn release(cid: u16) {
    with(|c| {
        let i = c.held.iter().position(|h| h.0 == cid).unwrap();
        let (cid, sc, opc) = c.held.remove(i);
        complete(c, 1, cid, sc, opc);
    });
}

fn admin(c: &mut Ctl, e: &Exec) -> u16 {
    let buf = Pa(e.prp1);
    match e.opc {
        ADM_IDENTIFY if e.at == 1 => {
            dev::clear(buf, 4096);
            dev::copy_in(buf.add(24), b"HopOS Fake NVMe                         ");
            dev::write8(buf.add(77), c.mdts);
        }
        ADM_IDENTIFY => {
            dev::clear(buf, 4096);
            dev::write64(buf, (c.disk.len() >> c.lbads) as u64);
            dev::write8(buf.add(26), 1); // LBAF 1 actief
            dev::write32(buf.add(132), u32::from(c.lbads) << 16);
        }
        ADM_CREATE_CQ => {
            c.q[1] = Some(FakeQ {
                sq: Pa(0),
                cq: buf,
                seen: 0,
                tail: 0,
                phase: true,
            });
        }
        ADM_CREATE_SQ => c.q[1].as_mut().unwrap().sq = buf,
        ADM_GET_FEATURES => {}
        0x00 | 0x04 => c.events.push("delete queue"),
        _ => return 1, // invalid opcode
    }
    0
}

fn io(c: &mut Ctl, e: &Exec) -> u16 {
    if e.opc == IO_FLUSH {
        c.events.push("flush");
        return 0;
    }
    let bs = 1u64 << c.lbads;
    if (e.at + u64::from(e.nlb)) * bs > c.disk.len() as u64 {
        return 0x80; // LBA out of range
    }
    let (mut off, mut left) = ((e.at * bs) as usize, u64::from(e.nlb) * bs);
    for p in pages(e.prp1, e.prp2, left) {
        let n = left.min(PAGE) as usize;
        match e.opc {
            IO_READ => dev::copy_in(Pa(p), &c.disk[off..off + n]),
            IO_WRITE => dev::copy_out(&mut c.disk[off..off + n], Pa(p)),
            _ => return 1,
        }
        off += n;
        left -= n as u64;
    }
    0
}

/// Registers en DMA in RAM; de controller zonder schijf.
pub(crate) struct Mem {
    _regs: Vec<u64>,
    _dma: Vec<u64>,
    pub(crate) base: Pa,
    pub(crate) dma: Pa,
}

pub(crate) fn machine(regs_len: u64, align: u64) -> Mem {
    let mut regs = vec![0u64; regs_len as usize / 8 + 1];
    let mut dma = vec![0u64; ((DMA_NEED + align) / 8) as usize];
    let base = Pa(regs.as_mut_ptr() as usize as u64);
    let dma_pa = Pa((dma.as_mut_ptr() as usize as u64).next_multiple_of(align));
    with(|c| {
        *c = Ctl {
            regs: base,
            ..Ctl::default()
        }
    });
    Mem {
        _regs: regs,
        _dma: dma,
        base,
        dma: dma_pa,
    }
}

/// Het nagebootste transport: een ring, of lineair (`L`) zoals de ANS.
pub(crate) struct Sim<const L: bool>;

impl<const L: bool> Transport for Sim<L> {
    type Error = u32;
    const LINEAR: bool = L;

    fn submit(&mut self, q: &Queue, slot: u16, _m: &Cmd) -> Result<(), u32> {
        if L {
            with(|c| c.rung.push((usize::from(q.id), slot)));
        }
        Ok(())
    }

    fn service(&mut self) -> Result<(), u32> {
        with(|c| {
            c.services += 1;
            if c.crashed {
                Err(Error::Transport(7))
            } else {
                Ok(())
            }
        })
    }
}

const NBLOCKS: u64 = 4096;

/// Een schijf van [`NBLOCKS`] blokken van 2^`lbads` bytes, elk blok met zijn
/// eigen nummer vooraan, en de core erop zoals PCI hem opbrengt.
fn sim<const L: bool>(lbads: u8, mdts: u8) -> (Mem, Nvme<Sim<L>>) {
    let m = machine(0x2000, PAGE);
    dev::write32(m.base, 1023); // MQES; DSTRD 0
    with(|c| {
        c.disk = vec![0; (NBLOCKS << lbads) as usize];
        for (i, b) in c.disk.chunks_mut(1 << lbads).enumerate() {
            b[..8].copy_from_slice(&(i as u64).to_le_bytes());
        }
        (c.lbads, c.mdts) = (lbads, mdts);
    });
    let mut n = Nvme::at(Sim::<L>, m.base, m.dma, DMA_NEED, now).unwrap();
    n.regs().cc.write(0);
    n.wait_ready(false).unwrap();
    n.enable(CC_EN | CC_IOSQES | CC_IOCQES).unwrap();
    n.identify_namespace().unwrap();
    n.create_io_queues().unwrap();
    n.started = true;
    with(|c| c.log.clear());
    (m, n)
}

/// De driver zoals hopfs hem ziet: achter de wachtrij, met een `Pace` die
/// pollt.
fn blk<T: Transport>(n: &mut Nvme<T>) -> blkdev::Queue<&mut Nvme<T>, Spin> {
    blkdev::Queue::new(n, Spin)
}

/// `read` door de wachtrij, afgedraaid.
fn rd<T: Transport>(n: &mut Nvme<T>, lba: u64, b: &mut [u8]) -> blkdev::Result {
    let q = blk(n);
    let mut io = &q;
    block_on(io.read(lba, b))
}

/// `write` door de wachtrij, afgedraaid.
fn wr<T: Transport>(n: &mut Nvme<T>, lba: u64, b: &[u8]) -> blkdev::Result {
    let q = blk(n);
    let mut io = &q;
    block_on(io.write(lba, b))
}

/// `flush` door de wachtrij, afgedraaid.
fn fl<T: Transport>(n: &mut Nvme<T>) -> blkdev::Result {
    let q = blk(n);
    let mut io = &q;
    block_on(io.flush())
}

/// De I/O-opdrachten die de controller uitvoerde: (opcode, LBA, blokken).
fn io_log() -> Vec<(u8, u64, u32)> {
    with(|c| {
        c.log
            .iter()
            .filter(|e| e.q == 1)
            .map(|e| (e.opc, e.at, e.nlb))
            .collect()
    })
}

/// Wacht op ticket `t` zoals de wachtrij het doet: ophalen, dan kijken (de
/// klok loopt mee). Een fout bij het ophalen is een dood device: dan geeft
/// `poll_tag` de fout.
fn wait_tag<T: Transport>(n: &mut Nvme<T>, t: usize, into: &mut [u8]) -> Poll<blkdev::Result> {
    for _ in 0..3 {
        let _ = n.reap();
        if let Poll::Ready(r) = n.poll_tag(t, into) {
            return Poll::Ready(r);
        }
    }
    Poll::Pending
}

/// Draagt `b` de blokken van 4 KiB vanaf `first`?
fn is_blocks(b: &[u8], first: u64) -> bool {
    b.chunks(4096)
        .zip(first..)
        .all(|(c, n)| c[..8] == n.to_le_bytes())
}

#[test]
fn every_size_round_trips_over_the_prp_lists() {
    for (lbads, mdts) in [(9u8, 0u8), (12, 2)] {
        let (m, mut n) = sim::<false>(lbads, mdts);
        for len in [1 << lbads, 4096, 8192, 12288, 40960, n.step()] {
            let data: Vec<u8> = (0..len).map(|i| (i * 13 + len) as u8).collect();
            wr(&mut n, 16, &data).unwrap();
            let mut got = vec![0u8; len];
            rd(&mut n, 16, &mut got).unwrap();
            assert!(got == data, "{len} bytes differ on 2^{lbads}");
        }
        fl(&mut n).unwrap();
        assert_eq!(io_log().last(), Some(&(IO_FLUSH, 0, 1)));
        assert_eq!(n.free_pages(), PAGES, "every page back");
        if lbads == 9 {
            // PRP2: nul voor één pagina, de tweede pagina voor twee, de
            // lijst van het ticket daarboven.
            let prp2: Vec<u64> = with(|c| {
                let w = c.log.iter().filter(|e| e.opc == IO_WRITE);
                w.map(|e| e.prp2).take(5).collect()
            });
            let (data, list) = (m.dma.0 + DATA_OFF, m.dma.0 + PRP_OFF);
            assert_eq!(prp2, [0, 0, data + PAGE, list, list]);
        }
    }
}

/// Een ticket boven de MDTS gaat als meer opdrachten de lucht in, elk met
/// een eigen CID en zijn stuk van de ene PRP-lijst, en is pas klaar met de
/// laatste completion; ook lineair, elke opdracht op zijn eigen slot.
#[test]
fn a_ticket_above_the_mdts_is_several_commands() {
    fn run<const L: bool>() {
        let (m, mut n) = sim::<L>(12, 2);
        assert_eq!((n.block_size(), n.max_transfer()), (4096, 16384));
        assert_eq!(AsyncBlockDevice::max_transfer(&n), 16 * 16384);
        let data: Vec<u8> = (0..40960u32).map(|i| (i * 7) as u8).collect();
        with(|c| c.hold = true);
        let t = n
            .start_tag(Op::Write {
                lba: 24,
                data: &data,
            })
            .unwrap();
        assert!(wait_tag(&mut n, t, &mut []).is_pending());
        assert_eq!(
            io_log(),
            [(IO_WRITE, 3, 4), (IO_WRITE, 7, 4), (IO_WRITE, 11, 2)]
        );
        let cids: Vec<u16> = with(|c| {
            assert!(
                !L || c.log.iter().all(|e| e.slot == e.cid),
                "linear: slot = CID"
            );
            // De tweede wijst midden in de lijst van het ticket.
            assert_eq!(c.log[1].prp2, m.dma.0 + PRP_OFF + 4 * 8);
            c.held.iter().map(|h| h.0).collect()
        });
        assert_eq!(cids, [0, 1, 2]);
        release(2);
        release(0);
        assert!(
            wait_tag(&mut n, t, &mut []).is_pending(),
            "one command still out"
        );
        release(1);
        assert_eq!(wait_tag(&mut n, t, &mut []), Poll::Ready(Ok(())));
        with(|c| c.hold = false);
        let mut got = vec![0u8; data.len()];
        rd(&mut n, 24, &mut got).unwrap();
        assert!(got == data);
        // Het contract rekent in 512 bytes: een LBA die niet op een blok
        // valt, gaat niet naar de controller.
        assert_eq!(n.sectors(), NBLOCKS * 8);
        assert_eq!(
            rd(&mut n, 25, &mut [0; 4096]),
            Err(blkdev::Error::OutOfRange { lba: 25, len: 4096 })
        );
    }
    run::<false>();
    run::<true>();
}

/// Een opdracht met een foutstatus faalt zijn ticket (pas na de laatste
/// completion van dat ticket), niet de driver.
#[test]
fn a_failed_command_fails_its_ticket_not_the_driver() {
    let (_m, mut n) = sim::<false>(12, 2);
    // De schijf is korter dan de namespace zegt: vanaf blok 8 status 0x80.
    with(|c| c.disk.truncate(8 * 4096));
    let mut b = vec![0u8; 40960];
    let i = n.start_read(4, b.len(), 0).unwrap();
    let r = n.wait_ticket(i, &mut b);
    assert_eq!(
        r,
        Err(Error::Status {
            opc: IO_READ,
            status: 0x80
        })
    );
    assert_eq!(io_log().len(), 3);
    let r = rd(&mut n, 32, &mut b);
    assert_eq!(r, Err(blkdev::Error::Io { lba: 32 }));
    rd(&mut n, 0, &mut b[..4096]).unwrap();
    assert!(is_blocks(&b[..4096], 0));
    assert_eq!(n.free_pages(), PAGES);
}

#[test]
fn invalid_transfers_never_reach_the_controller() {
    let (_m, mut n) = sim::<false>(9, 0);
    let big = n.step() + 512;
    for (lba, len) in [
        (0u64, 0usize),
        (0, 100),
        (NBLOCKS, 512),
        (NBLOCKS - 1, 1024),
        (u64::MAX, 512),
        (0, big),
    ] {
        assert_eq!(
            n.start_tag(Op::Write {
                lba,
                data: &vec![0; len]
            }),
            Err(blkdev::Error::OutOfRange { lba, len }),
            "lba {lba} len {len}"
        );
    }
    assert_eq!(
        rd(&mut n, NBLOCKS, &mut [0; 512]),
        Err(blkdev::Error::OutOfRange {
            lba: NBLOCKS,
            len: 512
        })
    );
    assert_eq!(io_log(), []);
    assert_eq!(n.free_pages(), PAGES);
}

/// `correctness_test.go`: een ontbrekende completion maakt de controller
/// dood, en een late completion geeft de oude pagina's niet vrij.
#[test]
fn a_timeout_kills_the_driver_and_keeps_its_pages() {
    let (m, mut n) = sim::<false>(9, 0);
    with(|c| c.mute = true);
    let i = n.start_read(0, 512, 0).unwrap();
    assert_eq!(
        n.wait_ticket(i, &mut []),
        Err(Error::Timeout { opc: IO_READ })
    );
    with(|c| c.mute = false);
    let page = m.dma.add(DATA_OFF);
    dev::write8(page, 0x11);
    let w = Op::Write {
        lba: 1,
        data: &[99; 512],
    };
    assert_eq!(n.start_tag(w), Err(blkdev::Error::Dead));
    assert_eq!(dev::read8(page), 0x11, "page reused after a timeout");
    assert_eq!(n.free_pages(), PAGES - 1, "the page stays the controller's");
    assert_eq!(n.io.tail, 1, "queue reused after a timeout");
    assert_eq!(fl(&mut n), Err(blkdev::Error::Dead));
}

#[test]
fn a_foreign_completion_or_a_dead_transport_kills_the_driver() {
    let (_m, mut n) = sim::<false>(9, 0);
    with(|c| c.wrong_cid = true);
    let i = n.start_flush().unwrap();
    assert_eq!(n.wait_ticket(i, &mut []), Err(Error::Cid { got: 7 }));
    assert_eq!(fl(&mut n), Err(blkdev::Error::Dead));

    let (_m, mut n) = sim::<true>(9, 0);
    with(|c| (c.hold, c.crashed) = (true, true));
    let t = n.start_tag(Op::Read { lba: 0, len: 512 }).unwrap();
    assert_eq!(
        wait_tag(&mut n, t, &mut []),
        Poll::Ready(Err(blkdev::Error::Dead))
    );
    assert_eq!(n.start_tag(Op::Flush), Err(blkdev::Error::Dead));
}

#[test]
fn the_dma_region_is_checked_before_one_access() {
    for (dma, size) in [
        (0x20_0000u64, DMA_NEED - 1),
        (0x20_0100, DMA_NEED),
        (0, DMA_NEED),
        (u64::MAX - PAGE + 1, DMA_NEED),
    ] {
        let e = Nvme::at(Sim::<false>, Pa(0), Pa(dma), size, now).err();
        assert_eq!(e, Some(Error::Dma { base: dma, size }));
    }
    let e: Error<u32> = Error::Dma { base: 1, size: 2 };
    assert!(e.to_string().starts_with("nvme: DMA region 0x2 at 0x1"));
}

/// Blokken van 4 KiB zonder MDTS: een volle hap is één opdracht van
/// [`MAX_TRANSFER`]. `hold`: de controller bevestigt pas na [`release`].
fn steps<const L: bool>(hold: bool) -> (Mem, Nvme<Sim<L>>) {
    let (m, n) = sim::<L>(12, 0);
    with(|c| c.hold = hold);
    (m, n)
}

const HAP: usize = MAX_TRANSFER as usize;
const LBAS: u64 = HAP as u64 / SECTOR;
/// Blokken van 4 KiB per hap.
const HB: u64 = HAP as u64 / 4096;

/// Leest één hap op `lba` met de controller vastgehouden: geeft de
/// opdracht met `cid` vrij en wacht.
fn read_held<T: Transport>(n: &mut Nvme<T>, lba: u64, cid: u16, b: &mut [u8]) {
    let t = n.start_tag(Op::Read { lba, len: HAP }).unwrap();
    assert!(
        wait_tag(n, t, b).is_pending(),
        "nothing back before the completion"
    );
    release(cid);
    assert_eq!(wait_tag(n, t, b), Poll::Ready(Ok(())));
}

#[test]
fn a_write_comes_back_only_after_its_completion_and_a_flush_after_it() {
    let (_m, mut n) = steps::<false>(true);
    let data = [0x77u8; 4096];
    let t = n
        .start_tag(Op::Write {
            lba: 8,
            data: &data,
        })
        .unwrap();
    // De controller deed de DMA al, maar bevestigde nog niet: de schrijf
    // komt niet terug, hoe vaak de wachter ook kijkt.
    with(|c| assert!(c.disk[4096..8192].iter().all(|&x| x == 0x77)));
    assert!(wait_tag(&mut n, t, &mut []).is_pending());
    release(0);
    assert_eq!(wait_tag(&mut n, t, &mut []), Poll::Ready(Ok(())));
    // Nu de flush (hopfs zet hem pas na de schrijfs die hij dekt), en ook
    // die alleen met zijn eigen completion.
    let t = n.start_tag(Op::Flush).unwrap();
    assert!(wait_tag(&mut n, t, &mut []).is_pending());
    release(0);
    assert_eq!(wait_tag(&mut n, t, &mut []), Poll::Ready(Ok(())));
    with(|c| assert_eq!(c.acked, [(IO_WRITE, 0), (IO_FLUSH, 0)]));
}

#[test]
fn sequential_reads_keep_the_next_hap_in_flight_and_a_flush_waits_for_it() {
    let (_m, mut n) = steps::<false>(true);
    let mut b = vec![0u8; HAP];
    read_held(&mut n, 0, 0, &mut b);
    assert!(is_blocks(&b, 0));
    read_held(&mut n, LBAS, 0, &mut b);
    // Twee happen op rij: de derde staat al op de controller, op het
    // ticket dat de tweede net vrijgaf.
    with(|c| assert_eq!(c.held.iter().map(|h| h.0).collect::<Vec<_>>(), [0]));
    assert_eq!(io_log().last(), Some(&(IO_READ, 2 * HB, HB as u32)));
    // Een flush is pas klaar als er geen read-ahead meer loopt.
    let t = n.start_tag(Op::Flush).unwrap();
    release(1);
    assert!(wait_tag(&mut n, t, &mut []).is_pending());
    release(0);
    assert_eq!(wait_tag(&mut n, t, &mut []), Poll::Ready(Ok(())));
    // De derde hap komt van de read-ahead: geen nieuwe lees, maar de
    // vierde staat alweer op de controller.
    // De vierde gaat al bij het pakken van de derde, niet pas na zijn
    // drain: twee happen tegelijk voor een lezer die alleen is.
    let before = io_log().len();
    let t = n
        .start_tag(Op::Read {
            lba: 2 * LBAS,
            len: HAP,
        })
        .unwrap();
    assert_eq!(io_log().len(), before + 1);
    assert_eq!(io_log().last(), Some(&(IO_READ, 3 * HB, HB as u32)));
    assert_eq!(wait_tag(&mut n, t, &mut b), Poll::Ready(Ok(())));
    assert!(is_blocks(&b, 2 * HB));
    assert_eq!(n.ahead_hits, 1);
    assert_eq!(
        io_log().len(),
        before + 1,
        "no second read-ahead for the same hap"
    );
    // Hij komt terug en wordt gelezen zonder nieuwe lees.
    release(0);
    let t = n
        .start_tag(Op::Read {
            lba: 3 * LBAS,
            len: HAP,
        })
        .unwrap();
    assert_eq!(wait_tag(&mut n, t, &mut b), Poll::Ready(Ok(())));
    assert!(is_blocks(&b, 3 * HB));
    assert_eq!((n.ahead_hits, n.ahead_waste), (2, 0));
}

/// De vroege read-ahead neemt de laatste pagina's: niet als een ander op
/// de controller staat. Dan komt hij pas na de drain, met de reserve. Met
/// MDTS 16 KiB is een hap 256 KiB, dan zijn er pagina's genoeg en telt
/// alleen of de lezer alleen is.
#[test]
fn the_early_read_ahead_waits_for_a_reader_that_is_not_alone() {
    let (_m, mut n) = sim::<false>(12, 2);
    let hap = n.step();
    let lbas = hap as u64 / SECTOR;
    let mut b = vec![0u8; hap];
    for k in [0, 1] {
        rd(&mut n, k * lbas, &mut b).unwrap();
    }
    // De read-ahead van hap 2 is terug; een ander leest 4 KiB en wacht nog.
    with(|c| c.hold = true);
    n.start_tag(Op::Read {
        lba: 999 * 8,
        len: 4096,
    })
    .unwrap();
    let before = io_log().len();
    let t = n
        .start_tag(Op::Read {
            lba: 2 * lbas,
            len: hap,
        })
        .unwrap();
    assert_eq!(io_log().len(), before, "not while another ticket is out");
    assert_eq!(wait_tag(&mut n, t, &mut b), Poll::Ready(Ok(())));
    assert!(is_blocks(&b, 2 * hap as u64 / 4096));
    let next = 3 * hap as u64 / 4096;
    assert_eq!(
        io_log().get(before).map(|e| e.1),
        Some(next),
        "after the drain"
    );
}

#[test]
fn a_write_over_the_read_ahead_makes_it_stale() {
    let (_m, mut n) = steps::<false>(true);
    let mut b = vec![0u8; HAP];
    read_held(&mut n, 0, 0, &mut b);
    read_held(&mut n, LBAS, 0, &mut b);
    // De read-ahead op CID 0 loopt nog; een schrijf over zijn blokken gaat
    // ernaast op CID 1, en komt eerder terug dan hij.
    let data = [0x5au8; 4096];
    let t = n
        .start_tag(Op::Write {
            lba: 2 * LBAS,
            data: &data,
        })
        .unwrap();
    release(1);
    assert_eq!(wait_tag(&mut n, t, &mut []), Poll::Ready(Ok(())));
    release(0);
    // De lees daarna ziet de schrijf, niet de oude bytes van de read-ahead
    // (die op CID 0 terugkwam en weg is).
    read_held(&mut n, 2 * LBAS, 1, &mut b);
    assert!(b[..4096].iter().all(|&x| x == 0x5a));
    assert!(is_blocks(&b[4096..], 2 * HB + 1));
    assert_eq!((n.ahead_hits, n.ahead_waste), (0, 1));
    // Alle pagina's terug, behalve die van de volgende read-ahead.
    assert_eq!(n.free_pages(), PAGES - HAP / PAGE as usize);
    with(|c| {
        let cids: Vec<u16> = c.acked.iter().map(|x| x.1).collect();
        assert_eq!(cids, [0, 0, 1, 0, 1]);
    });
}

#[test]
fn an_unclaimed_read_ahead_makes_room_for_the_next_stream() {
    let (_m, mut n) = steps::<false>(false);
    let mut b = vec![0u8; HAP];
    // Een stroom die stopt: zijn laatste read-ahead (hap 2) leest niemand.
    for k in [0u64, 1] {
        rd(&mut n, k * LBAS, &mut b).unwrap();
    }
    // Een volgende stroom elders krijgt toch weer een read-ahead.
    for k in [5u64, 6, 7] {
        rd(&mut n, k * LBAS, &mut b).unwrap();
        assert!(is_blocks(&b, HB * k));
    }
    assert_eq!((n.ahead_hits, n.ahead_waste), (1, 1));
}

#[test]
fn sixteen_tickets_at_once_and_back_in_any_order() {
    fn run<const L: bool>() {
        let (_m, mut n) = steps::<L>(true);
        // Zestien losse blokken van 4 KiB tegelijk op de controller.
        let tags: Vec<usize> = (0..DEPTH as u64)
            .map(|k| {
                let read = Op::Read {
                    lba: k * 8,
                    len: 4096,
                };
                n.start_tag(read).unwrap()
            })
            .collect();
        assert_eq!(tags, (0..DEPTH).collect::<Vec<_>>());
        let one = Op::Read { lba: 0, len: 4096 };
        assert_eq!(n.start_tag(one), Err(blkdev::Error::Busy), "the 17th waits");
        with(|c| assert_eq!(c.held.len(), DEPTH));
        // Terug in omgekeerde volgorde: elk ticket krijgt zijn eigen bytes.
        let mut b = vec![0u8; 4096];
        for t in (0..DEPTH).rev() {
            assert!(wait_tag(&mut n, t, &mut b).is_pending());
            release(t as u16);
            assert_eq!(wait_tag(&mut n, t, &mut b), Poll::Ready(Ok(())));
            assert!(is_blocks(&b, t as u64), "ticket {t}");
        }
        with(|c| assert!(!L || c.log.iter().all(|e| e.slot == e.cid)));
        assert_eq!(n.free_pages(), PAGES);
    }
    run::<false>();
    run::<true>();
}

#[test]
fn a_ticket_takes_free_pages_wherever_they_lie() {
    let (_m, mut n) = steps::<false>(true);
    // Drie, twee en vier pagina's: de eerste gaat terug, en de derde
    // schrijf krijgt dan 0, 1, 2 en 5 (de PRP-lijst wijst ze aan).
    let w = |k: usize, v: u8| vec![v; k * 4096];
    let (d1, d2, d3) = (w(3, 0x11), w(2, 0x22), w(4, 0x33));
    let t1 = n.start_tag(Op::Write { lba: 0, data: &d1 }).unwrap();
    let t2 = n.start_tag(Op::Write { lba: 24, data: &d2 }).unwrap();
    release(0);
    assert_eq!(wait_tag(&mut n, t1, &mut []), Poll::Ready(Ok(())));
    let t3 = n.start_tag(Op::Write { lba: 64, data: &d3 }).unwrap();
    assert_eq!(&n.pages[t3][..4], &[0, 1, 2, 5]);
    for (t, cid) in [(t2, 1), (t3, 0)] {
        release(cid);
        assert_eq!(wait_tag(&mut n, t, &mut []), Poll::Ready(Ok(())));
    }
    with(|c| c.hold = false);
    let mut b = vec![0u8; d3.len()];
    rd(&mut n, 64, &mut b).unwrap();
    assert_eq!(b, d3);
    assert_eq!(n.free_pages(), PAGES);
}

/// De CID's raken op vóór de ring overloopt: met MDTS 1 (8 KiB) is een
/// ticket van 64 KiB acht opdrachten, en na zeven tickets zijn er nog
/// zeven CID's.
#[test]
fn cids_run_out_before_the_ring_overflows() {
    let (_m, mut n) = sim::<false>(12, 1);
    with(|c| c.hold = true);
    let data = vec![0x42u8; 65536];
    let mut lba = 0;
    let mut write = |n: &mut Nvme<Sim<false>>| {
        lba += 128;
        n.start_tag(Op::Write { lba, data: &data })
    };
    for t in 0..7 {
        assert_eq!(write(&mut n), Ok(t));
    }
    assert_eq!(write(&mut n), Err(blkdev::Error::Busy));
    assert_eq!(
        n.free_pages(),
        PAGES - 7 * 16,
        "a refused ticket keeps no pages"
    );
    with(|c| assert_eq!(c.held.len(), 56));
    for cid in 0..8 {
        release(cid);
    }
    assert_eq!(wait_tag(&mut n, 0, &mut []), Poll::Ready(Ok(())));
    assert_eq!(write(&mut n), Ok(0));
}
