//! NVMe over PCIe: het blokapparaat onder hopfs op de O6N en de Altra.
//!
//! De Rust-vorm van `OLD/metal/driver/nvme` (zonder de Apple-ANS, die bij het
//! M4-board komt): een admin-queue plus één I/O-queue-paar, gepold, één
//! verzoek tegelijk in één DMA-databuffer. Dat is geen beperking maar de
//! vorm: de eigenaar (de hopfs-actor, `&mut self`) doet één ding tegelijk,
//! dus de driver kent geen tags, geen rij en geen herordening. De Go-`mu`
//! die de transfers serialiseerde, verdwijnt daarmee (PORT.md §3: "de
//! NVMe-actor: zijn lus ís één tegelijk").
//!
//! De driver kent geen PCI: het board vindt de controller (klasse 01:08:02),
//! leest BAR0 die de firmware toewees, zet memory-decode en bus-mastering
//! aan, en geeft het adres. Het registerblok is getypeerd ([`Regs`]); de
//! doorbells liggen op een stride die de controller zelf meldt (CAP.DSTRD)
//! en worden daarom berekend.
//!
//! De DMA-regio: vier queue-pagina's en een PRP-lijstpagina in het eerste
//! blok van 2 MB, de databuffer in een eigen blok van 2 MB. Een board mag
//! dat tweede blok cacheable mappen (op de M4 gemeten 03-09: ongecachete
//! loads haalden ~100 MB/s); de driver doet dan `push` vóór een write en
//! `pull` na een read, en op ongecachet geheugen zijn die gratis.
//!
//! Eén pad voor de I/O: [`blkdev::AsyncBlockDevice`] zet de opdracht klaar
//! (`post`) en kijkt of de completion er is (`reap`). De hopfs-actor wacht
//! erop met `.await` en geeft intussen de executor terug; wat vóór de
//! executor draait (de mount, de meetbank) draait dezelfde futures af met
//! `blkdev::block_on` (waarom: de crate-doc van `blkdev`). Les van 30-09:
//! de synchrone commit van hopfs hield op QEMU de OS-core tot 7 s stil; op
//! NVMe zou een FLUSH van een consumer-SSD zonder PLP hetzelfde doen. De
//! controller heeft geen lijn in deze driver: de wachter pollt
//! (`blkdev::InFlight::done`), per ronde en dan op een timer, zodat Hop
//! tijdens een lange FLUSH zijn beurten houdt. Alleen de admin-opdrachten
//! van [`Nvme::new`] (identify, de queues aanmelden) wachten ter plekke,
//! met `block_on` over dezelfde `reap`: dat is de init, vóór er een
//! blokcontract bestaat.
//!
//! Een verzoek dat niet binnen [`COMMAND_TIMEOUT_NS`] terugkomt, maakt de
//! driver dood: de controller kan nog in de buffer schrijven, dus een
//! volgend verzoek zou andermans bytes zien of overschrijven. Dood is luid
//! en blijvend, zoals in Go (`c.failed`) en in virtio-blk.

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

use blkdev::Op;
use core::fmt;
use core::mem::{offset_of, size_of};
use core::task::Poll;
use dev::{Pa, Reg};

pub mod apple;

/// De controller-registers (NVMe 1.4 §3.1) op BAR0.
#[repr(C)]
struct Regs {
    /// Capabilities: MQES [15:0], TO [31:24] (in 500 ms), DSTRD [35:32],
    /// MPSMIN [51:48].
    cap: Reg<u64>,
    vs: Reg<u32>,
    _intms: Reg<u32>,
    _intmc: Reg<u32>,
    cc: Reg<u32>,
    _r0: u32,
    csts: Reg<u32>,
    _nssr: Reg<u32>,
    aqa: Reg<u32>,
    asq: Reg<u64>,
    acq: Reg<u64>,
}

const _: () = {
    assert!(offset_of!(Regs, cap) == 0x00);
    assert!(offset_of!(Regs, vs) == 0x08);
    assert!(offset_of!(Regs, cc) == 0x14);
    assert!(offset_of!(Regs, csts) == 0x1c);
    assert!(offset_of!(Regs, aqa) == 0x24);
    assert!(offset_of!(Regs, asq) == 0x28);
    assert!(offset_of!(Regs, acq) == 0x30);
};

/// Het begin van de doorbells: SQ y tail op `DB + (2y) << (2 + DSTRD)`, CQ y
/// head op `DB + (2y + 1) << (2 + DSTRD)`.
const DB: u64 = 0x1000;
/// Hoeveel van BAR0 het board moet mappen: registers en de doorbells van
/// queue 0 en 1, bij de grootste stride die we accepteren.
pub const MMIO_LEN: u64 = 0x2000;
/// De grootste doorbell-stride die in [`MMIO_LEN`] past (4 << 4 = 64 bytes).
const MAX_DSTRD: u32 = 4;

/// Eén submission-entry (NVMe §4.2).
#[repr(C)]
struct Sqe {
    /// Opcode [7:0], CID [31:16].
    cdw0: u32,
    nsid: u32,
    _cdw2: u32,
    _cdw3: u32,
    _mptr: u64,
    prp1: u64,
    prp2: u64,
    cdw10: u32,
    cdw11: u32,
    cdw12: u32,
    _cdw13: u32,
    _cdw14: u32,
    _cdw15: u32,
}

/// Eén completion-entry (NVMe §4.6).
#[repr(C)]
struct Cqe {
    _dw0: u32,
    _dw1: u32,
    _sq_head: u16,
    _sq_id: u16,
    cid: u16,
    /// Phase [0], status [15:1].
    status: u16,
}

const _: () = {
    assert!(size_of::<Sqe>() == 64);
    assert!(offset_of!(Sqe, cdw0) == 0);
    assert!(offset_of!(Sqe, nsid) == 4);
    assert!(offset_of!(Sqe, prp1) == 24);
    assert!(offset_of!(Sqe, prp2) == 32);
    assert!(offset_of!(Sqe, cdw10) == 40);
    assert!(offset_of!(Sqe, cdw11) == 44);
    assert!(offset_of!(Sqe, cdw12) == 48);
    assert!(size_of::<Cqe>() == 16);
    assert!(offset_of!(Cqe, cid) == 12);
    assert!(offset_of!(Cqe, status) == 14);
};
const SQE: u64 = size_of::<Sqe>() as u64;
const CQE: u64 = size_of::<Cqe>() as u64;

const CC_EN: u32 = 1 << 0;
/// 64-byte submission-entries (2^6).
const CC_IOSQES: u32 = 6 << 16;
/// 16-byte completion-entries (2^4).
const CC_IOCQES: u32 = 4 << 20;
const CSTS_RDY: u32 = 1 << 0;
/// Controller Fatal Status.
const CSTS_CFS: u32 = 1 << 1;

// Opcodes.
const ADM_CREATE_SQ: u8 = 0x01;
const ADM_CREATE_CQ: u8 = 0x05;
const ADM_IDENTIFY: u8 = 0x06;
const IO_FLUSH: u8 = 0x00;
const IO_WRITE: u8 = 0x01;
const IO_READ: u8 = 0x02;

/// Entries per queue: ruim voor één consument (de Go-waarde).
pub const Q_ENTRIES: u16 = 64;
/// De namespace die we gebruiken.
const NSID: u32 = 1;
/// De controllerpagina (CC.MPS = 0).
pub const PAGE: u64 = 4096;
/// De grootste transfer van één verzoek: één frame van de system-API
/// (`MAX_IO_CHUNK`), één NVMe-opdracht.
pub const MAX_TRANSFER: u64 = 1 << 20;
/// Hoe lang één opdracht mag duren, tenzij de controller in CAP.TO langer
/// vraagt. Een gezonde completion is er binnen microseconden.
pub const COMMAND_TIMEOUT_NS: u64 = 5_000_000_000;

// De indeling van de DMA-regio.
const ASQ_OFF: u64 = 0;
const ACQ_OFF: u64 = PAGE;
const IOSQ_OFF: u64 = 2 * PAGE;
const IOCQ_OFF: u64 = 3 * PAGE;
const PRP_OFF: u64 = 4 * PAGE;
/// De databuffer: een eigen blok van 2 MB dat het board cacheable mag
/// mappen; de queues en de PRP-lijst blijven erbuiten.
pub const DATA_OFF: u64 = 2 << 20;
/// De maat van het datablok.
pub const DATA_SIZE: u64 = 2 << 20;
/// Wat de driver van de DMA-regio vraagt.
pub const DMA_NEED: u64 = DATA_OFF + DATA_SIZE;

const _: () = {
    assert!(Q_ENTRIES as u64 * SQE <= PAGE && Q_ENTRIES as u64 * CQE <= PAGE);
    assert!(PRP_OFF + PAGE <= DATA_OFF);
    assert!(MAX_TRANSFER <= DATA_SIZE);
    // Eén PRP-lijstpagina draagt 512 adressen; 1 MiB vraagt er 255.
    assert!((MAX_TRANSFER / PAGE - 1) * 8 <= PAGE);
    assert!(DATA_OFF.is_multiple_of(2 << 20) && DATA_SIZE.is_multiple_of(2 << 20));
};

/// Waarom de driver weigert.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    /// De DMA-regio is te klein, niet op een pagina, of loopt om.
    Dma {
        /// De basis.
        base: u64,
        /// De maat.
        size: u64,
    },
    /// Het registerblok leest alles-enen: de controller is niet op de bus.
    OffBus,
    /// De controller eist pagina's groter dan 4 KB, of een stride die niet
    /// in [`MMIO_LEN`] past.
    Unsupported {
        /// CAP.
        cap: u64,
    },
    /// CSTS.RDY werd niet wat gevraagd, of de controller meldt een fatale
    /// fout.
    NotReady {
        /// CSTS na de grens.
        csts: u32,
        /// De gevraagde RDY.
        want: bool,
    },
    /// Namespace 1 is onbruikbaar (geen blokken, metadata, bescherming, of
    /// een blokmaat buiten 512..4096).
    Namespace {
        /// Aantal blokken.
        blocks: u64,
        /// LBADS.
        lbads: u8,
    },
    /// Een lengte die geen blokveelvoud is, nul, of buiten de namespace.
    Range {
        /// De eerste LBA.
        lba: u64,
        /// Het aantal bytes.
        len: usize,
    },
    /// De controller meldde een fout op een opdracht.
    Status {
        /// De opcode.
        opc: u8,
        /// De statusvelden (SCT/SC, zonder de phase).
        status: u16,
    },
    /// Geen completion binnen de grens; de driver is vanaf nu dood.
    Timeout {
        /// De opcode.
        opc: u8,
    },
    /// Een completion voor een andere opdracht; de driver is vanaf nu dood.
    Cid {
        /// Wat de controller terugzette.
        got: u16,
        /// Wat we verwachtten.
        want: u16,
    },
    /// Een eerder verzoek liep af; niets gaat meer naar de controller.
    Dead,
    /// Er loopt nog een opdracht (de wachter ging weg vóór de completion);
    /// de DMA-buffer is nog van de controller.
    Busy,
    /// Er is geen opdracht om op te wachten.
    Idle,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::Dma { base, size } => {
                write!(
                    f,
                    "nvme: DMA region {size:#x} at {base:#x} invalid (need {DMA_NEED:#x}, page aligned)"
                )
            }
            Self::OffBus => f.write_str("nvme: registers read all-ones (device off the bus?)"),
            Self::Unsupported { cap } => write!(f, "nvme: unsupported controller (CAP={cap:#x})"),
            Self::NotReady { csts, want } => {
                write!(
                    f,
                    "nvme: CSTS.RDY never became {} (CSTS={csts:#x})",
                    u8::from(want)
                )
            }
            Self::Namespace { blocks, lbads } => {
                write!(
                    f,
                    "nvme: namespace unusable (blocks={blocks}, block size 2^{lbads})"
                )
            }
            Self::Range { lba, len } => write!(f, "nvme: {len} bytes at LBA {lba} out of range"),
            Self::Status { opc, status } => {
                write!(f, "nvme: command {opc:#x} status {status:#x}")
            }
            Self::Timeout { opc } => {
                write!(
                    f,
                    "nvme: timeout on command {opc:#x}, DMA retained, driver dead"
                )
            }
            Self::Cid { got, want } => {
                write!(
                    f,
                    "nvme: completion CID {got}, expected {want}, driver dead"
                )
            }
            Self::Dead => f.write_str("nvme: driver dead after an unfinished command"),
            Self::Busy => f.write_str("nvme: a command is still in flight"),
            Self::Idle => f.write_str("nvme: no command in flight"),
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Eén SQ/CQ-paar met de poll-staat.
#[derive(Clone, Copy, Debug)]
struct Queue {
    sq: Pa,
    cq: Pa,
    id: u16,
    /// SQ-tail: wij produceren.
    tail: u16,
    /// CQ-head: wij consumeren.
    head: u16,
    /// De phase die de volgende geldige CQ-entry draagt.
    phase: bool,
}

impl Queue {
    fn new(dma: Pa, sq: u64, cq: u64, id: u16) -> Self {
        Self {
            sq: dma.add(sq),
            cq: dma.add(cq),
            id,
            tail: 0,
            head: 0,
            phase: true,
        }
    }
}

/// De opdracht die op de controller staat.
#[derive(Clone, Copy, Debug)]
struct Pending {
    admin: bool,
    opc: u8,
    cid: u16,
    t0: u64,
    /// Bij een lees: zoveel bytes komen uit de databuffer.
    read: usize,
}

/// Een opdracht in opbouw.
#[derive(Clone, Copy, Default)]
struct Cmd {
    opc: u8,
    nsid: u32,
    prp1: u64,
    prp2: u64,
    cdw10: u32,
    cdw11: u32,
    cdw12: u32,
}

/// Eén NVMe-controller met namespace 1.
pub struct Nvme {
    base: Pa,
    dma: Pa,
    clock: fn() -> u64,
    timeout_ns: u64,
    dstrd: u32,
    admin: Queue,
    io: Queue,
    block_size: u64,
    blocks: u64,
    max_transfer: u64,
    model: [u8; 40],
    dead: bool,
    pending: Option<Pending>,
    /// Meetlat: afgehandelde opdrachten.
    pub commands: u64,
    /// Meetlat: de langste opdracht in nanoseconden.
    pub slowest_ns: u64,
}

impl Nvme {
    /// Reset de controller, zet de admin-queue op, identificeert
    /// controller en namespace 1, en meldt het I/O-queue-paar aan. `clock`
    /// geeft monotone nanoseconden.
    ///
    /// # Safety
    ///
    /// `base` is BAR0 van een NVMe-controller, gemapt als Device voor
    /// minstens [`MMIO_LEN`] bytes en voor altijd; memory-decode en
    /// bus-mastering staan aan. `[dma, dma+dma_size)` is gemapt geheugen dat
    /// alleen deze driver en de controller gebruiken, nu en zolang het
    /// programma draait, op een adres dat de controller ziet zoals de CPU.
    pub unsafe fn new(base: Pa, dma: Pa, dma_size: u64, clock: fn() -> u64) -> Result<Self> {
        if dma.0 == 0
            || !dma.is_aligned(PAGE)
            || dma_size < DMA_NEED
            || dma.0.checked_add(dma_size).is_none()
        {
            return Err(Error::Dma {
                base: dma.0,
                size: dma_size,
            });
        }
        let mut c = Self::at(base, dma, clock);
        c.enable()?;
        c.identify()?;
        c.create_io_queues()?;
        Ok(c)
    }

    /// De staat zonder één registertoegang (ook voor de tests).
    fn at(base: Pa, dma: Pa, clock: fn() -> u64) -> Self {
        Self {
            base,
            dma,
            clock,
            timeout_ns: COMMAND_TIMEOUT_NS,
            dstrd: 0,
            admin: Queue::new(dma, ASQ_OFF, ACQ_OFF, 0),
            io: Queue::new(dma, IOSQ_OFF, IOCQ_OFF, 1),
            block_size: 0,
            blocks: 0,
            max_transfer: MAX_TRANSFER,
            model: [0; 40],
            dead: false,
            pending: None,
            commands: 0,
            slowest_ns: 0,
        }
    }

    fn regs(&self) -> &'static Regs {
        // SAFETY: de voorwaarde van `new`.
        unsafe { dev::regs(self.base) }
    }

    fn data(&self) -> Pa {
        self.dma.add(DATA_OFF)
    }

    /// Wacht tot CSTS.RDY `want` is.
    fn wait_ready(&self, want: bool) -> Result {
        let r = self.regs();
        let deadline = (self.clock)().saturating_add(self.timeout_ns);
        loop {
            let csts = r.csts.read();
            if csts & CSTS_CFS == 0 && (csts & CSTS_RDY != 0) == want {
                return Ok(());
            }
            if csts == u32::MAX || csts & CSTS_CFS != 0 || (self.clock)() >= deadline {
                return Err(Error::NotReady { csts, want });
            }
            core::hint::spin_loop();
        }
    }

    /// Reset, admin-queue registreren, enable.
    fn enable(&mut self) -> Result {
        let r = self.regs();
        let cap = r.cap.read();
        if cap == u64::MAX {
            return Err(Error::OffBus);
        }
        let mpsmin = (cap >> 48) & 0xf;
        let dstrd = ((cap >> 32) & 0xf) as u32;
        let mqes = (cap & 0xffff) + 1;
        if mpsmin != 0 || dstrd > MAX_DSTRD || mqes < u64::from(Q_ENTRIES) {
            return Err(Error::Unsupported { cap });
        }
        self.dstrd = dstrd;
        // CAP.TO is de langste tijd die de controller voor RDY vraagt, in
        // halve seconden; een trage controller krijgt die ruimte.
        let to = ((cap >> 24) & 0xff) * 500_000_000;
        self.timeout_ns = COMMAND_TIMEOUT_NS.max(to);

        r.cc.write(0);
        self.wait_ready(false)?;
        dev::clear(self.dma, DATA_OFF as usize);
        self.admin = Queue::new(self.dma, ASQ_OFF, ACQ_OFF, 0);
        self.io = Queue::new(self.dma, IOSQ_OFF, IOCQ_OFF, 1);
        let q = u32::from(Q_ENTRIES - 1);
        r.aqa.write((q << 16) | q);
        r.asq.write(self.admin.sq.0);
        r.acq.write(self.admin.cq.0);
        dev::mb();
        r.cc.write(CC_EN | CC_IOSQES | CC_IOCQES);
        self.wait_ready(true)
    }

    fn doorbell(&self, q: &Queue, cq: bool) -> Pa {
        let n = 2 * u64::from(q.id) + u64::from(cq);
        self.base.add(DB + (n << (2 + self.dstrd)))
    }

    /// Eén admin-opdracht van de init ([`new`](Self::new)): zetten, en met
    /// `block_on` over [`reap`](Self::reap) wachten tot hij terug is. Er is
    /// dan nog geen executor en geen blokcontract; de I/O loopt nooit hier.
    fn admin(&mut self, m: Cmd) -> Result {
        self.post(true, m, 0)?;
        blkdev::block_on(core::future::poll_fn(|_| self.reap()))
    }

    /// Zet `m` in de SQ en luidt de doorbell; keert meteen terug. Eén
    /// opdracht tegelijk: de CID is de tail, en de CQ-entry die terugkomt
    /// hoort bij deze opdracht of de driver is dood. Een opdracht waarvan de
    /// wachter wegging, wordt eerst opgehaald als hij klaar is; loopt hij
    /// nog, dan [`Error::Busy`].
    fn post(&mut self, admin: bool, m: Cmd, read: usize) -> Result {
        if self.dead {
            return Err(Error::Dead);
        }
        if self.pending.is_some() && self.reap().is_pending() {
            return Err(Error::Busy);
        }
        let mut q = if admin { self.admin } else { self.io };
        let cid = q.tail;
        let sqe = q.sq.add(u64::from(cid) * SQE);
        dev::clear(sqe, SQE as usize);
        dev::write32(
            sqe.add(offset_of!(Sqe, cdw0) as u64),
            u32::from(m.opc) | (u32::from(cid) << 16),
        );
        dev::write32(sqe.add(offset_of!(Sqe, nsid) as u64), m.nsid);
        dev::write64(sqe.add(offset_of!(Sqe, prp1) as u64), m.prp1);
        dev::write64(sqe.add(offset_of!(Sqe, prp2) as u64), m.prp2);
        dev::write32(sqe.add(offset_of!(Sqe, cdw10) as u64), m.cdw10);
        dev::write32(sqe.add(offset_of!(Sqe, cdw11) as u64), m.cdw11);
        dev::write32(sqe.add(offset_of!(Sqe, cdw12) as u64), m.cdw12);
        dev::mb();
        q.tail = (q.tail + 1) % Q_ENTRIES;
        dev::write32(self.doorbell(&q, false), u32::from(q.tail));
        self.store(admin, q);
        self.pending = Some(Pending {
            admin,
            opc: m.opc,
            cid,
            t0: (self.clock)(),
            read,
        });
        Ok(())
    }

    /// Kijkt of de completion van de opdracht van [`post`](Self::post) er
    /// is; keert meteen terug. Na de time-out is de driver dood.
    fn reap(&mut self) -> Poll<Result> {
        let Some(p) = self.pending else {
            return Poll::Ready(Err(Error::Idle));
        };
        let mut q = if p.admin { self.admin } else { self.io };
        let cqe = q.cq.add(u64::from(q.head) * CQE);
        let status = dev::read16(cqe.add(offset_of!(Cqe, status) as u64));
        if (status & 1 != 0) != q.phase {
            if (self.clock)() >= p.t0.saturating_add(self.timeout_ns) {
                self.dead = true;
                self.pending = None;
                return Poll::Ready(Err(Error::Timeout { opc: p.opc }));
            }
            return Poll::Pending;
        }
        self.pending = None;
        // De phase vóór de inhoud: pas na de barrière is de rest van de
        // entry van de controller.
        dev::mb();
        let got = dev::read16(cqe.add(offset_of!(Cqe, cid) as u64));
        q.head = (q.head + 1) % Q_ENTRIES;
        if q.head == 0 {
            q.phase = !q.phase;
        }
        dev::write32(self.doorbell(&q, true), u32::from(q.head));
        self.store(p.admin, q);
        if got != p.cid {
            self.dead = true;
            return Poll::Ready(Err(Error::Cid { got, want: p.cid }));
        }
        let dt = (self.clock)().saturating_sub(p.t0);
        self.slowest_ns = self.slowest_ns.max(dt);
        self.commands += 1;
        Poll::Ready(match status >> 1 {
            0 => Ok(()),
            s => Err(Error::Status {
                opc: p.opc,
                status: s,
            }),
        })
    }

    fn store(&mut self, admin: bool, q: Queue) {
        if admin {
            self.admin = q;
        } else {
            self.io = q;
        }
    }

    /// Controller (CNS 1): model en MDTS; namespace 1 (CNS 0): grootte en
    /// blokmaat uit de actieve LBA-indeling.
    fn identify(&mut self) -> Result {
        let buf = self.data();
        self.admin(Cmd {
            opc: ADM_IDENTIFY,
            prp1: buf.0,
            cdw10: 1,
            ..Cmd::default()
        })?;
        dev::pull(buf, PAGE as usize);
        dev::copy_out(&mut self.model, buf.add(24));
        // MDTS = 0 is "geen limiet"; anders 2^MDTS controllerpagina's. Onze
        // eigen buffer blijft de harde bovengrens.
        let mdts = dev::read8(buf.add(77));
        if mdts != 0 && mdts < 52 {
            self.max_transfer = self.max_transfer.min(PAGE << mdts);
        }

        self.admin(Cmd {
            opc: ADM_IDENTIFY,
            nsid: NSID,
            prp1: buf.0,
            ..Cmd::default()
        })?;
        dev::pull(buf, PAGE as usize);
        let blocks = dev::read64(buf);
        let flbas = u64::from(dev::read8(buf.add(26)) & 0xf);
        let lbaf = dev::read32(buf.add(128 + flbas * 4));
        let lbads = ((lbaf >> 16) & 0xff) as u8;
        let metadata = lbaf & 0xffff;
        let protection = dev::read8(buf.add(29)) & 7;
        if blocks == 0 || !(9..=12).contains(&lbads) || metadata != 0 || protection != 0 {
            return Err(Error::Namespace { blocks, lbads });
        }
        self.blocks = blocks;
        self.block_size = 1 << lbads;
        Ok(())
    }

    /// Meldt het I/O-queue-paar aan: CQ eerst, fysiek aaneengesloten, zonder
    /// interrupts (de driver pollt).
    fn create_io_queues(&mut self) -> Result {
        let q = u32::from(Q_ENTRIES - 1) << 16;
        let id = u32::from(self.io.id);
        self.admin(Cmd {
            opc: ADM_CREATE_CQ,
            prp1: self.io.cq.0,
            cdw10: q | id,
            cdw11: 1, // PC
            ..Cmd::default()
        })?;
        self.admin(Cmd {
            opc: ADM_CREATE_SQ,
            prp1: self.io.sq.0,
            cdw10: q | id,
            cdw11: (id << 16) | 1, // CQID, PC
            ..Cmd::default()
        })
    }

    /// De paginawijzers voor `n` bytes in de databuffer: PRP1 naar de eerste
    /// pagina; PRP2 voor twee pagina's direct naar de tweede, en daarboven
    /// naar een lijst met alle vervolgpagina's.
    fn prps(&self, n: u64) -> (u64, u64) {
        let data = self.data();
        if n <= PAGE {
            return (data.0, 0);
        }
        if n <= 2 * PAGE {
            return (data.0, data.0 + PAGE);
        }
        let list = self.dma.add(PRP_OFF);
        dev::clear(list, PAGE as usize);
        let pages = n.div_ceil(PAGE);
        for p in 1..pages {
            dev::write64(list.add((p - 1) * 8), data.0 + p * PAGE);
        }
        (data.0, list.0)
    }

    /// Toetst een transfer tegen de namespace en de buffer: nul bytes is
    /// een fout van de aanroeper (NLB is 0-based, een lege transfer werd
    /// 0xffff blokken, ver buiten onze buffer).
    fn check(&self, lba: u64, len: usize) -> Result<u32> {
        let n = len as u64;
        let bad = Error::Range { lba, len };
        if len == 0 || self.block_size == 0 || !n.is_multiple_of(self.block_size) {
            return Err(bad);
        }
        let nlb = n / self.block_size;
        match lba.checked_add(nlb) {
            Some(end) if end <= self.blocks && n <= self.max_transfer => {
                u32::try_from(nlb - 1).map_err(|_| bad)
            }
            _ => Err(bad),
        }
    }

    fn io_cmd(&self, opc: u8, lba: u64, len: usize, nlb0: u32) -> Cmd {
        let (prp1, prp2) = self.prps(len as u64);
        Cmd {
            opc,
            nsid: NSID,
            prp1,
            prp2,
            cdw10: (lba & 0xffff_ffff) as u32,
            cdw11: (lba >> 32) as u32,
            cdw12: nlb0,
        }
    }

    /// De hapgrootte: de grootste transfer, en minstens één blok.
    fn step(&self) -> usize {
        self.max_transfer.max(self.block_size.max(1)) as usize
    }

    /// De blokmaat van namespace 1 in bytes: de eenheid van een LBA.
    #[must_use]
    pub fn block_size(&self) -> u64 {
        self.block_size
    }

    /// De grootte van namespace 1 in blokken.
    #[must_use]
    pub fn blocks(&self) -> u64 {
        self.blocks
    }

    /// De grootste transfer van één opdracht in bytes.
    #[must_use]
    pub fn max_transfer(&self) -> u64 {
        self.max_transfer
    }

    /// De modelnaam uit de identify, zonder de opvulling.
    #[must_use]
    pub fn model(&self) -> &str {
        let end = self
            .model
            .iter()
            .rposition(|&b| b != 0 && b != b' ')
            .map_or(0, |i| i + 1);
        self.model
            .get(..end)
            .and_then(|m| core::str::from_utf8(m).ok())
            .unwrap_or("?")
    }

    /// De NVMe-versie (VS: major [31:16], minor [15:8]).
    #[must_use]
    pub fn version(&self) -> (u16, u8) {
        let vs = self.regs().vs.read();
        ((vs >> 16) as u16, (vs >> 8) as u8)
    }

    /// Het datablok (basis, maat) dat het board cacheable mag mappen.
    #[must_use]
    pub fn data_region(&self) -> (Pa, u64) {
        (self.data(), DATA_SIZE)
    }
}

/// De sector van het blokcontract.
pub const SECTOR: u64 = 512;

impl Nvme {
    /// De capaciteit in sectoren van [`SECTOR`] bytes.
    #[must_use]
    pub fn sectors(&self) -> u64 {
        self.blocks.saturating_mul(self.block_size / SECTOR)
    }

    /// Een LBA van 512 bytes als LBA van de namespace.
    fn native(&self, lba: u64, len: usize) -> blkdev::Result<u64> {
        let per = self.block_size / SECTOR;
        if per == 0 || !lba.is_multiple_of(per) || !(len as u64).is_multiple_of(self.block_size) {
            return Err(blkdev::Error::OutOfRange { lba, len });
        }
        Ok(lba / per)
    }
}

fn blk_err(e: Error, lba: u64, len: usize) -> blkdev::Error {
    match e {
        Error::Range { .. } => blkdev::Error::OutOfRange { lba, len },
        Error::Dead | Error::Timeout { .. } | Error::Cid { .. } => blkdev::Error::Dead,
        Error::Busy => blkdev::Error::Busy,
        _ => blkdev::Error::Io { lba },
    }
}

impl Nvme {
    /// Zet één opdracht van hoogstens [`max_transfer`](Self::max_transfer)
    /// bytes op de I/O-queue; `lba` in LBA's van de namespace. Een write gaat nu de databuffer in, dus pas als er niets
    /// meer loopt.
    fn start_op(&mut self, op: Op<'_>, lba: u64) -> Result {
        match op {
            Op::Read { len, .. } => {
                let nlb0 = self.check(lba, len)?;
                let m = self.io_cmd(IO_READ, lba, len, nlb0);
                self.post(false, m, len)
            }
            Op::Write { data, .. } => {
                let nlb0 = self.check(lba, data.len())?;
                if self.dead {
                    return Err(Error::Dead);
                }
                if self.pending.is_some() && self.reap().is_pending() {
                    return Err(Error::Busy);
                }
                dev::copy_in(self.data(), data);
                dev::push(self.data(), data.len());
                let m = self.io_cmd(IO_WRITE, lba, data.len(), nlb0);
                self.post(false, m, 0)
            }
            Op::Flush => self.post(
                false,
                Cmd {
                    opc: IO_FLUSH,
                    nsid: NSID,
                    ..Cmd::default()
                },
                0,
            ),
        }
    }

    /// De completion van [`start_op`](Self::start_op): bij een lees de
    /// bytes uit de databuffer naar `into`.
    fn poll_op(&mut self, into: &mut [u8]) -> Poll<Result> {
        let read = self.pending.map_or(0, |p| p.read);
        let r = self.reap();
        if matches!(r, Poll::Ready(Ok(()))) && read > 0 {
            let n = read.min(into.len());
            dev::pull(self.data(), n);
            if let Some(d) = into.get_mut(..n) {
                dev::copy_out(d, self.data());
            }
        }
        r
    }
}

/// Het blokcontract van `blkdev`: LBA's van 512 bytes, ook als de
/// namespace 4096-byte-blokken heeft. Dan moet een verzoek op een blok
/// beginnen en eindigen; hopfs schrijft in blokken van 4 KB vanaf LBA 0, dus
/// dat doet hij altijd. Zo is de NVMe voor de binary dezelfde schijf als
/// virtio-blk ([`Nvme::sectors`], [`SECTOR`]). Zonder lijn: de wachter
/// pollt.
impl blkdev::AsyncBlockDevice for Nvme {
    fn max_transfer(&self) -> usize {
        self.step()
    }

    fn start(&mut self, op: Op<'_>) -> blkdev::Result {
        let (lba, len) = match op {
            Op::Read { lba, len } => (lba, len),
            Op::Write { lba, data } => (lba, data.len()),
            Op::Flush => (0, 0),
        };
        let native = if matches!(op, Op::Flush) {
            0
        } else {
            self.native(lba, len)?
        };
        self.start_op(op, native).map_err(|e| blk_err(e, lba, len))
    }

    fn poll_done(&mut self, into: &mut [u8]) -> Poll<blkdev::Result> {
        self.poll_op(into).map_err(|e| blk_err(e, 0, 0))
    }
}

#[cfg(test)]
mod tests;
