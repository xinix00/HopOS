//! De ANS: dezelfde NVMe, een andere aanlanding (Apple silicon, de interne
//! SSD van de Mac mini M4).
//!
//! Op Apple silicon zit de SSD niet als PCIe-device op een bus maar achter
//! een coprocessor (ANS, Apple NVMe Storage) met zijn eigen RTKit-firmware.
//! Het NVMe daarboven is gewoon NVMe: dezelfde queues, opcodes en
//! completions. Vier dingen zijn anders, en die staan in dit bestand:
//!
//! 1. De submission-deurbel is "lineair": je schrijft niet de nieuwe tail
//!    maar het SLOT waar de opdracht staat. De completion-deurbellen zijn de
//!    gewone (0x1004 en 0x100c, DSTRD = 0).
//! 2. Elke opdracht heeft naast zijn SQE een TCB van 128 bytes in een tweede
//!    tabel: de NVMMU leest daaruit welke kant de DMA op gaat en welke
//!    buffers erbij horen. Na elke completion moet dat slot ongeldig
//!    verklaard worden, anders loopt de tabel vol.
//! 3. De coprocessor ervoor moet gewekt worden, zijn mailbox moet tijdens
//!    elke wachtlus leeggetrokken worden, en bij afsluiten moet hij in
//!    slaap. Dat gesprek (RTKit) woont niet hier maar achter het trait
//!    [`Coprocessor`], dat het board met `driver-rtkit` invult: deze crate
//!    kent geen RTKit.
//! 4. De schijf is niet van ons. macOS staat erop (iBootSystemContainer,
//!    APFS, RecoveryOS), dus lezen mag overal (de GPT staat op blok 1), maar
//!    schrijven alleen binnen een venster dat de aanroeper expliciet zet
//!    ([`Ans::set_window`], typisch het grootste gat uit `fw::gpt` na de
//!    macOS-partities). Zonder venster weigert elke schrijf, luid.
//!
//! De vier lessen van 29-08 (`OLD/docs/v1/archief/apple-m4.md`, "De
//! leeskant werkt") staan op de plek waar ze gelden: het wekbericht
//! ([`Ans::start`]), CC bijstellen in plaats van overschrijven
//! ([`Ans::start`]), de opdracht op het slot dat de deurbel aanwijst
//! (`submit`), en netjes afsluiten ([`Ans::shutdown`]).
//!
//! Schrijven is dezelfde submit-weg als lezen met opcode 0x01: m1n1 en
//! Linux zetten de DMA-richting van de TCB op `opcode & 1`. De Go-versie
//! (`OLD/metal/driver/nvme/apple.go`) kon alleen lezen. Referentie: m1n1
//! `src/nvme.c`, Linux `drivers/nvme/host/apple.c`.
//!
//! Eén eigenaar met `&mut self`. De opstart, de GPT en het afsluiten wachten
//! ter plekke op tag 0 (er draait dan nog niets anders). Het blokcontract
//! wacht niet: `start` zet de opdracht op de controller en keert terug,
//! `poll_done` haalt de completions op die er zijn (zie de impl van
//! `blkdev::AsyncBlockDevice`). Daarnaast mag er één read-ahead in de lucht
//! zijn: [`DEPTH`] tags, elk met een eigen helft van het datablok.

use super::{COMMAND_TIMEOUT_NS, DATA_OFF, DATA_SIZE, MAX_TRANSFER, PAGE, Q_ENTRIES, SECTOR};
use core::fmt;
use core::mem::{offset_of, size_of};
use dev::{Pa, Reg};

/// Wat het ANS-pad van de coprocessor ervoor nodig heeft. Het board vult
/// dit in met `driver-rtkit` (de ASC-basis, de bufferregio en de klok zijn
/// van die kant); deze crate kent geen RTKit.
pub trait Coprocessor {
    /// Wat er mis kan gaan in het gesprek.
    type Error: fmt::Display + fmt::Debug + Copy;

    /// Het opstartgesprek: wekken, HELLO, de endpointkaart, wachten tot hij
    /// "aan" meldt. Ook op een coprocessor die al draait.
    fn boot(&mut self) -> core::result::Result<(), Self::Error>;

    /// De mailbox leegtrekken: syslogregels bevestigen, geheugenverzoeken
    /// beantwoorden. Hoort in elke wachtlus; een fout hier is meestal een
    /// crashmelding.
    fn service(&mut self) -> core::result::Result<(), Self::Error>;

    /// In slaap: AP-kant stil, coprocessor slapen, zijn kern stil.
    fn sleep(&mut self) -> core::result::Result<(), Self::Error>;

    /// Schrijft zijn crashlog naar `out`, als hij er een heeft.
    fn crashlog(&self, out: &mut dyn fmt::Write) -> fmt::Result {
        let _ = out;
        Ok(())
    }
}

/// De NVMe-registers met Apples toevoegingen (m1n1 `src/nvme.c`), op het
/// NVMe-venster uit de ADT. Alle 64-bit registers gaan in twee helften: ze
/// nemen geen enkele 64-bit toegang aan.
#[repr(C)]
struct Regs {
    _cap: [u32; 2],
    _vs: u32,
    _intms: u32,
    _intmc: u32,
    cc: Reg<u32>,
    _r0: u32,
    csts: Reg<u32>,
    _nssr: u32,
    aqa: Reg<u32>,
    asq: [Reg<u32>; 2],
    acq: [Reg<u32>; 2],
    _r1: [u8; 0x1000 - 0x38],
    /// SQ0-tail, CQ0-head, SQ1-tail, CQ1-head (DSTRD = 0). De SQ-tails
    /// gebruikt het lineaire pad niet.
    db: [Reg<u32>; 4],
    _r2: [u8; 0x1200 - 0x1010],
    /// M4 (`nvme-secure-bar`): de I/O-SQ-basis nog eens, hier.
    ioq_cmds: [Reg<u32>; 2],
    /// M4 (`nvme-secure-bar`): de I/O-CQ-basis nog eens, hier.
    ioq_cqes: [Reg<u32>; 2],
    max_pend: Reg<u32>,
    _r3: [u8; 0x1300 - 0x1214],
    boot_status: Reg<u32>,
    _r4: [u8; 0x24908 - 0x1304],
    linear_sq_ctrl: Reg<u32>,
    db_linear_asq: Reg<u32>,
    db_linear_iosq: Reg<u32>,
}

/// De NVMMU op zijn eigen venster (M4: `reg[3]`; M1-M3: hetzelfde als
/// NVMe), met dezelfde offsets.
#[repr(C)]
struct Nvmmu {
    _r0: [u8; 0x28100],
    num: Reg<u32>,
    _r1: u32,
    asq_base: [Reg<u32>; 2],
    iosq_base: [Reg<u32>; 2],
    tcb_inval: Reg<u32>,
    _r2: [u8; 0x29120 - 0x2811c],
    tcb_stat: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(Regs, cc) == 0x14);
    assert!(offset_of!(Regs, csts) == 0x1c);
    assert!(offset_of!(Regs, aqa) == 0x24);
    assert!(offset_of!(Regs, asq) == 0x28);
    assert!(offset_of!(Regs, acq) == 0x30);
    assert!(offset_of!(Regs, db) == 0x1000);
    assert!(offset_of!(Regs, ioq_cmds) == 0x1200);
    assert!(offset_of!(Regs, ioq_cqes) == 0x1208);
    assert!(offset_of!(Regs, max_pend) == 0x1210);
    assert!(offset_of!(Regs, boot_status) == 0x1300);
    assert!(offset_of!(Regs, linear_sq_ctrl) == 0x24908);
    assert!(offset_of!(Regs, db_linear_asq) == 0x2490c);
    assert!(offset_of!(Regs, db_linear_iosq) == 0x24910);
    assert!(offset_of!(Nvmmu, num) == 0x28100);
    assert!(offset_of!(Nvmmu, asq_base) == 0x28108);
    assert!(offset_of!(Nvmmu, iosq_base) == 0x28110);
    assert!(offset_of!(Nvmmu, tcb_inval) == 0x28118);
    assert!(offset_of!(Nvmmu, tcb_stat) == 0x29120);
};

/// Hoeveel van het NVMe-venster het board moet mappen.
pub const MMIO_LEN: u64 = size_of::<Regs>() as u64;
/// Hoeveel van het NVMMU-venster het board moet mappen.
pub const NVMMU_LEN: u64 = size_of::<Nvmmu>() as u64;

/// Eén submission-entry (NVMe §4.2). De afstand tussen entries komt uit
/// CC.IOSQES, dat iBoot zet (128 bytes op de M4); op slot 0 doet die niet
/// mee.
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

/// Eén NVMMU-slot (Linux `struct apple_nvmmu_tcb`).
#[repr(C)]
struct Tcb {
    opcode: u8,
    dma_flags: u8,
    command_id: u8,
    _unk0: u8,
    /// NLB, 0-based: CDW12 van de opdracht.
    length: u32,
    _unk1: [u64; 2],
    prp1: u64,
    prp2: u64,
    _unk2: [u64; 2],
    _aes_iv: [u8; 8],
    _aes: [u8; 64],
}

const _: () = {
    assert!(size_of::<Sqe>() == 64);
    assert!(offset_of!(Sqe, nsid) == 4);
    assert!(offset_of!(Sqe, prp1) == 24);
    assert!(offset_of!(Sqe, prp2) == 32);
    assert!(offset_of!(Sqe, cdw10) == 40);
    assert!(offset_of!(Sqe, cdw11) == 44);
    assert!(offset_of!(Sqe, cdw12) == 48);
    assert!(size_of::<Cqe>() == 16);
    assert!(offset_of!(Cqe, cid) == 12);
    assert!(offset_of!(Cqe, status) == 14);
    assert!(size_of::<Tcb>() == 128);
    assert!(offset_of!(Tcb, dma_flags) == 1);
    assert!(offset_of!(Tcb, command_id) == 2);
    assert!(offset_of!(Tcb, length) == 4);
    assert!(offset_of!(Tcb, prp1) == 24);
    assert!(offset_of!(Tcb, prp2) == 32);
};

const SQE: u64 = size_of::<Sqe>() as u64;
const CQE: u64 = size_of::<Cqe>() as u64;
const TCB: u64 = size_of::<Tcb>() as u64;

/// De NVMMU leest in de DMA-richting: van het device naar ons.
const TCB_FROM_DEVICE: u8 = 1 << 0;
/// En van ons naar het device.
const TCB_TO_DEVICE: u8 = 1 << 1;

const CC_EN: u32 = 1 << 0;
const CC_SHN_MASK: u32 = 3 << 14;
const CC_SHN_NORMAL: u32 = 1 << 14;
const CSTS_RDY: u32 = 1 << 0;
const CSTS_CFS: u32 = 1 << 1;
const CSTS_SHST_MASK: u32 = 3 << 2;
const CSTS_SHST_DONE: u32 = 2 << 2;
const LINEAR_SQ_EN: u32 = 1 << 0;
/// `NVME_BOOT_STATUS_OK`: de NVMe-kant van de firmware staat.
pub const BOOT_STATUS_OK: u32 = 0xde71_ce55;

// Opcodes. De richting van de DMA leidt de NVMMU niet zelf af: oneven
// schrijft, even leest (m1n1 en Linux: `opcode & 1`).
const ADM_DELETE_SQ: u8 = 0x00;
const ADM_CREATE_SQ: u8 = 0x01;
const ADM_DELETE_CQ: u8 = 0x04;
const ADM_CREATE_CQ: u8 = 0x05;
const ADM_IDENTIFY: u8 = 0x06;
const IO_FLUSH: u8 = 0x00;
const IO_WRITE: u8 = 0x01;
const IO_READ: u8 = 0x02;

const _: () = {
    assert!(IO_WRITE & 1 == 1 && IO_READ & 1 == 0);
    assert!(ADM_IDENTIFY & 1 == 0);
};

const NSID: u32 = 1;

/// De blokmaat van de ANS: m1n1 neemt 4 KB aan, en de namespace bevragen
/// mag niet (29-08: de firmware antwoordt met `NVME_PERM_ERR` en valt om).
pub const BLOCK: u64 = 4096;
/// Contract-sectoren per ANS-blok.
const PER_BLOCK: u64 = BLOCK / SECTOR;

/// Het slot van de opdrachten die ter plekke wachten (admin, de GPT): 0. De
/// lineaire deurbel wijst het slot aan, en op slot 0 doet de entry-afstand
/// niet mee.
const SLOT: u16 = 0;

/// Zoveel I/O-opdrachten van het blokcontract staan hoogstens tegelijk op de
/// controller: de opdracht van de eigenaar plus één read-ahead. Elk heeft een
/// eigen MiB van het datablok ([`DATA_SIZE`] is 2 MiB).
pub const DEPTH: usize = 2;

/// De tags van die opdrachten. Tag 0 is sinds 29-08 op ijzer bewezen, tag 1
/// op 01-10 (M26: de read-ahead met de juiste bytes, zie [`write_sqe`]).
const IO_TAGS: [u16; DEPTH] = [0, 1];

/// Zonder lijn pollt de wachter zo lang per ronde van de executor: een
/// opdracht van 4 KB is binnen ~10 us terug (M14: schrijven 4, lezen 9 us).
pub const POLL_SPIN_NS: u64 = 20_000;
/// En daarna op deze timer. Een MiB lezen duurt ~600 us, schrijven ~210 us
/// (M14): 20 us kost hoogstens 3 tot 10 procent te laat kijken, en de
/// OS-core slaapt ertussen (WFI op de timer).
pub const POLL_PERIOD: core::time::Duration = core::time::Duration::from_micros(20);

// De DMA-regio: drie tabellen per queue-paar (TCB's, opdrachten,
// completions), elk op 16 KB, dan de PRP-lijst, dan de databuffer in een
// eigen blok van 2 MB dat het board gecached mag mappen (03-09 gemeten:
// ongecachete loads haalden ~100 MB/s op de M4).
const ADMIN_TCB_OFF: u64 = 0x0000;
const ASQ_OFF: u64 = 0x4000;
const ACQ_OFF: u64 = 0x8000;
const IO_TCB_OFF: u64 = 0xc000;
const IOSQ_OFF: u64 = 0x1_0000;
const IOCQ_OFF: u64 = 0x1_4000;
const PRP_OFF: u64 = 0x1_8000;
/// Wat de ANS van de DMA-regio vraagt: queues, PRP-lijst en het hele
/// datablok. RTKit-buffers horen erachter, niet erin.
pub const DMA_NEED: u64 = DATA_OFF + DATA_SIZE;
/// De uitlijning van de DMA-regio: de paginamaat van dit silicium.
pub const DMA_ALIGN: u64 = 0x4000;

const _: () = {
    // 128 bytes per SQE (IOSQES = 7 van iBoot) en per TCB passen in 16 KB.
    assert!(Q_ENTRIES as u64 * 128 <= 0x4000 && Q_ENTRIES as u64 * CQE <= 0x4000);
    assert!(PRP_OFF + DEPTH as u64 * PAGE <= DATA_OFF);
    assert!(DEPTH as u64 * MAX_TRANSFER <= DATA_SIZE);
    assert!(DATA_OFF.is_multiple_of(DMA_ALIGN));
    // Tags binnen de tabel van de NVMMU, de tweede plek van een tag in de
    // 16 KB van zijn queue.
    assert!(IO_TAGS[DEPTH - 1] < Q_ENTRIES && (IO_TAGS[DEPTH - 1] as u64 + 1) * SQE <= 0x4000);
};

/// Hoe lang de firmware mag doen over BOOT_STATUS na het gesprek.
pub const BOOT_TIMEOUT_NS: u64 = 1_000_000_000;
/// Hoe lang CSTS mag doen over RDY of SHST.
pub const READY_TIMEOUT_NS: u64 = 5_000_000_000;

/// Waarom de ANS weigert. `E` is de fout van de [`Coprocessor`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error<E> {
    /// De DMA-regio is te klein, niet op 16 KB, of loopt om.
    Dma {
        /// De basis.
        base: u64,
        /// De maat.
        size: u64,
    },
    /// Nog niet gestart (of afgesloten): eerst [`Ans::start`].
    NotStarted,
    /// Het gesprek met de coprocessor liep mis.
    Coprocessor(E),
    /// De NVMe-kant van de firmware kwam niet op.
    BootStatus {
        /// BOOT_STATUS na de grens.
        status: u32,
    },
    /// CSTS.RDY werd niet wat gevraagd, of de controller meldt een fatale
    /// fout.
    NotReady {
        /// CSTS na de grens.
        csts: u32,
        /// CC op dat moment.
        cc: u32,
        /// De gevraagde RDY.
        want: bool,
    },
    /// CSTS.SHST werd niet "klaar".
    Shutdown {
        /// CSTS na de grens.
        csts: u32,
    },
    /// De controller meldde een fout op een opdracht.
    Status {
        /// De opcode.
        opc: u8,
        /// SCT/SC, zonder de phase.
        status: u16,
    },
    /// Geen completion binnen de grens; de driver is vanaf nu dood.
    Timeout {
        /// De opcode.
        opc: u8,
    },
    /// Een completion voor een ander slot; de driver is vanaf nu dood.
    Cid {
        /// Wat de controller terugzette.
        got: u16,
        /// Wat we verwachtten.
        want: u16,
    },
    /// De NVMMU nam de invalidatie van een slot niet aan; dood.
    Nvmmu {
        /// Het slot.
        slot: u16,
        /// TCB_STAT.
        stat: u32,
    },
    /// Een eerder verzoek liep af; niets gaat meer naar de controller.
    Dead,
    /// Alle I/O-tags staan nog op de controller (een wachter ging weg vóór
    /// zijn completion); de buffers zijn nog van de controller.
    Busy,
    /// Een lengte die geen blokveelvoud is, nul, of buiten de schijf.
    Range {
        /// Het eerste blok.
        block: u64,
        /// Het aantal bytes.
        len: usize,
    },
    /// Blok 1 is geen GPT-header: de capaciteit is onbekend en de schijf
    /// blijft dicht.
    NoGpt,
    /// De GPT-header noemt onbruikbare grenzen.
    Gpt {
        /// Het blok van de reservekopie.
        backup: u64,
        /// Het eerste bruikbare blok.
        first: u64,
        /// Het laatste bruikbare blok.
        last: u64,
    },
    /// Een schrijf zonder venster: de schijf is van macOS.
    NoWindow {
        /// Het eerste blok.
        block: u64,
        /// Het aantal bytes.
        len: usize,
    },
    /// Een schrijf buiten het venster.
    OutsideWindow {
        /// Het eerste blok.
        block: u64,
        /// Het aantal bytes.
        len: usize,
        /// Het venster: eerste blok.
        first: u64,
        /// Het venster: aantal blokken.
        blocks: u64,
    },
    /// Een venster dat leeg is of buiten het bruikbare deel van de GPT valt.
    BadWindow {
        /// Het gevraagde eerste blok.
        first: u64,
        /// Het gevraagde aantal blokken.
        blocks: u64,
        /// Het eerste bruikbare blok van de GPT.
        usable_first: u64,
        /// Het laatste bruikbare blok van de GPT.
        usable_last: u64,
    },
}

impl<E: fmt::Display> fmt::Display for Error<E> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Dma { base, size } => write!(
                f,
                "ans: DMA region {size:#x} at {base:#x} invalid (need {DMA_NEED:#x}, {DMA_ALIGN:#x} aligned)"
            ),
            Self::NotStarted => f.write_str("ans: not started"),
            Self::Coprocessor(e) => write!(f, "ans: coprocessor: {e}"),
            Self::BootStatus { status } => write!(
                f,
                "ans: boot status {status:#x} (expected {BOOT_STATUS_OK:#x}), firmware not ready"
            ),
            Self::NotReady { csts, cc, want } => write!(
                f,
                "ans: CSTS.RDY never became {} (CSTS={csts:#x} CC={cc:#x})",
                u8::from(*want)
            ),
            Self::Shutdown { csts } => {
                write!(f, "ans: shutdown did not complete (CSTS={csts:#x})")
            }
            Self::Status { opc, status } => {
                write!(f, "ans: command {opc:#x} status {status:#x}")
            }
            Self::Timeout { opc } => write!(
                f,
                "ans: timeout on command {opc:#x}, DMA retained, driver dead"
            ),
            Self::Cid { got, want } => {
                write!(f, "ans: completion CID {got}, expected {want}, driver dead")
            }
            Self::Nvmmu { slot, stat } => write!(
                f,
                "ans: NVMMU invalidation for slot {slot} failed ({stat:#x}), driver dead"
            ),
            Self::Dead => f.write_str("ans: driver dead after an unfinished command"),
            Self::Busy => f.write_str("ans: all I/O tags still in flight"),
            Self::Range { block, len } => {
                write!(f, "ans: {len} bytes at block {block} out of range")
            }
            Self::NoGpt => {
                f.write_str("ans: no GPT on block 1, capacity unknown, disk stays closed")
            }
            Self::Gpt {
                backup,
                first,
                last,
            } => write!(
                f,
                "ans: GPT unusable (backup {backup}, usable {first}..{last})"
            ),
            Self::NoWindow { block, len } => write!(
                f,
                "ans: WRITE REFUSED: {len} bytes at block {block}, no write window (the disk belongs to macOS)"
            ),
            Self::OutsideWindow {
                block,
                len,
                first,
                blocks,
            } => write!(
                f,
                "ans: WRITE REFUSED: {len} bytes at block {block} outside window {first}+{blocks}"
            ),
            Self::BadWindow {
                first,
                blocks,
                usable_first,
                usable_last,
            } => write!(
                f,
                "ans: window {first}+{blocks} outside the usable GPT range {usable_first}..{usable_last}"
            ),
        }
    }
}

/// De `Result` van dit pad.
pub type Result<T, E> = core::result::Result<T, Error<E>>;

/// De adressen die het board uit de ADT haalt.
#[derive(Clone, Copy, Debug)]
pub struct Config {
    /// Het NVMe-venster (M4: `reg[9]` van de ans-node).
    pub nvme: Pa,
    /// Het NVMMU-venster (M4: `reg[3]`; eerder hetzelfde als `nvme`).
    pub nvmmu: Pa,
    /// De DMA-regio: queues, TCB's, PRP-lijst en databuffer. DMA-adres ==
    /// fysiek adres (de SART is een filter, geen vertaling).
    pub dma: Pa,
    /// De maat van de DMA-regio, minstens [`DMA_NEED`].
    pub dma_size: u64,
    /// De ADT-vlag `nvme-secure-bar` (M4): dan wil de controller de
    /// I/O-queue-bases nog eens in NVME_IOQ_CQES/CMDS, of hij crasht.
    pub secure_bar: bool,
}

/// Eén queue-paar met zijn NVMMU-tabel en de poll-staat.
#[derive(Clone, Copy, Debug)]
struct Queue {
    tcb: Pa,
    sq: Pa,
    cq: Pa,
    id: u16,
    /// CQ-head: wij consumeren.
    head: u16,
    /// De phase die de volgende geldige CQ-entry draagt.
    phase: bool,
}

impl Queue {
    fn new(dma: Pa, tcb: u64, sq: u64, cq: u64, id: u16) -> Self {
        Self {
            tcb: dma.add(tcb),
            sq: dma.add(sq),
            cq: dma.add(cq),
            id,
            head: 0,
            phase: true,
        }
    }
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

/// Waarvoor een I/O-opdracht op de controller staat.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Use {
    /// De opdracht van de eigenaar: `poll_done` wacht erop.
    Demand,
    /// Een read-ahead. `stale`: een schrijf raakte zijn blokken, of de
    /// eigenaar las iets anders; de bytes worden nooit gebruikt.
    Ahead {
        /// Niet meer bruikbaar.
        stale: bool,
    },
}

/// Eén I/O-opdracht op de controller, op tag `IO_TAGS[i]` met de `i`-de MiB
/// van het datablok.
#[derive(Clone, Copy, Debug)]
struct Inflight {
    opc: u8,
    /// Het eerste ANS-blok en het aantal bytes (0 bij een flush).
    block: u64,
    len: usize,
    /// De LBA van het contract, voor de fout.
    lba: u64,
    t0: u64,
    use_: Use,
    /// `None` zolang de completion er niet is, daarna SCT/SC.
    done: Option<u16>,
}

/// Het schrijfvenster in blokken van [`BLOCK`].
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
struct Window {
    first: u64,
    blocks: u64,
}

/// De NVMe-controller achter de ANS.
///
/// # Invariants
///
/// Een schrijf raakt alleen blokken binnen `window`, en `window` ligt binnen
/// het bruikbare deel van de GPT (`usable`).
pub struct Ans<C: Coprocessor> {
    cfg: Config,
    now: fn() -> u64,
    cop: C,
    admin: Queue,
    io: Queue,
    started: bool,
    dead: bool,
    model: [u8; 40],
    blocks: u64,
    max_transfer: u64,
    /// Eerste en laatste bruikbare blok uit de GPT-header.
    usable: (u64, u64),
    window: Option<Window>,
    /// De I/O-opdrachten van het blokcontract die op de controller staan,
    /// per tag (zie de impl van `blkdev::AsyncBlockDevice`).
    inflight: [Option<Inflight>; DEPTH],
    /// Het blok waar de laatste lees van de eigenaar eindigde: begint de
    /// volgende daar, dan leest hij sequentieel en komt er een read-ahead.
    seq_end: u64,
    /// Meetlat: lezingen die de read-ahead al (deels) gedaan had.
    pub ahead_hits: u64,
    /// Meetlat: read-aheads die niemand gebruikte.
    pub ahead_waste: u64,
    /// Meetlat: afgehandelde opdrachten.
    pub commands: u64,
    /// Meetlat: de langste opdracht in nanoseconden.
    pub slowest_ns: u64,
}

impl<C: Coprocessor> Ans<C> {
    /// De ANS op de adressen van `cfg`, met coprocessor `cop`; nog zonder
    /// één registertoegang. [`start`](Self::start) brengt hem op; die stap
    /// is los, zodat het board bij een weigering de coprocessor nog heeft
    /// (in slaap, power-reset van zijn domein, opnieuw: m1n1's volgorde).
    ///
    /// # Safety
    ///
    /// `cfg.nvme` en `cfg.nvmmu` zijn de vensters uit de ADT, gemapt als
    /// Device voor minstens [`MMIO_LEN`] en [`NVMMU_LEN`] bytes en voor
    /// altijd. `[cfg.dma, cfg.dma + cfg.dma_size)` is gemapt geheugen dat
    /// alleen deze driver en de ANS gebruiken, binnen een SART-venster, op
    /// een adres dat de ANS ziet zoals de CPU. Het datablok (vanaf
    /// [`DATA_OFF`]) is Normal gemapt (write-back of NC; de driver kopieert
    /// er met `memcpy` in en uit, en doet zelf het cache-onderhoud); de
    /// rest niet gecached. GEMETEN 01-10 op de M4: de vluchtige 8-byte-lus
    /// kostte het leespad een kwart van zijn tijd.
    pub unsafe fn new(cfg: Config, cop: C, now: fn() -> u64) -> Result<Self, C::Error> {
        let d = cfg.dma;
        if d.0 == 0
            || !d.is_aligned(DMA_ALIGN)
            || cfg.dma_size < DMA_NEED
            || d.0.checked_add(cfg.dma_size).is_none()
        {
            return Err(Error::Dma {
                base: d.0,
                size: cfg.dma_size,
            });
        }
        Ok(Self {
            cfg,
            now,
            cop,
            admin: Queue::new(d, ADMIN_TCB_OFF, ASQ_OFF, ACQ_OFF, 0),
            io: Queue::new(d, IO_TCB_OFF, IOSQ_OFF, IOCQ_OFF, 1),
            started: false,
            dead: false,
            model: [0; 40],
            blocks: 0,
            max_transfer: MAX_TRANSFER,
            usable: (0, 0),
            window: None,
            inflight: [None; DEPTH],
            seq_end: u64::MAX,
            ahead_hits: 0,
            ahead_waste: 0,
            commands: 0,
            slowest_ns: 0,
        })
    }

    fn regs(&self) -> &'static Regs {
        // SAFETY: de voorwaarde van `new`: het NVMe-venster is gemapt voor
        // MMIO_LEN bytes.
        unsafe { dev::regs(self.cfg.nvme) }
    }

    fn nvmmu(&self) -> &'static Nvmmu {
        // SAFETY: de voorwaarde van `new`: het NVMMU-venster is gemapt voor
        // NVMMU_LEN bytes.
        unsafe { dev::regs(self.cfg.nvmmu) }
    }

    fn data(&self) -> Pa {
        self.cfg.dma.add(DATA_OFF)
    }

    /// De coprocessor ervoor.
    pub fn coprocessor(&mut self) -> &mut C {
        &mut self.cop
    }

    /// Geeft de coprocessor terug (na een mislukte start: in slaap, reset,
    /// opnieuw).
    pub fn into_coprocessor(self) -> C {
        self.cop
    }

    /// Trekt de mailbox van de coprocessor leeg. Een fout is een
    /// omgevallen coprocessor: dan komt er geen completion meer.
    fn service(&mut self) -> Result<(), C::Error> {
        self.cop.service().map_err(|e| {
            self.dead = true;
            Error::Coprocessor(e)
        })
    }

    /// Neemt de NVMe-controller van de ANS over.
    ///
    /// Eerst het gesprek met de coprocessor, ook als hij al draait: zonder
    /// wekbericht blijft hij in de slaapstand die iBoot achterliet
    /// (`CC.SHN = 1`), en negeert de controller elke schrijf naar CC
    /// (les 1, 29-08: CSTS.RDY werd nooit 1).
    pub fn start(&mut self) -> Result<(), C::Error> {
        self.started = false;
        self.dead = false;
        self.window = None;
        self.inflight = [None; DEPTH];
        self.seq_end = u64::MAX;
        self.cop.boot().map_err(Error::Coprocessor)?;

        // Dan wachten tot de NVMe-kant van de firmware er staat. Dit hoort ná
        // het gesprek: dat zet die kant opnieuw op, dus een OK van ervoor zegt
        // niets (29-08: ervoor kijken gaf een controller die de ene boot wel
        // en de andere niet ready werd).
        let r = self.regs();
        self.wait(BOOT_TIMEOUT_NS, |_| r.boot_status.read() == BOOT_STATUS_OK)
            .map_err(|e| match e {
                Error::NotReady { .. } => Error::BootStatus {
                    status: r.boot_status.read(),
                },
                e => e,
            })?;

        // Oude DMA moet aantoonbaar gestopt zijn voordat queuegeheugen
        // wijzigt.
        r.cc.update(|c| c & !CC_EN);
        self.wait_ready(false)?;
        dev::clear(self.cfg.dma, (DATA_OFF + MAX_TRANSFER) as usize);
        dev::push(self.data(), MAX_TRANSFER as usize);
        let d = self.cfg.dma;
        self.admin = Queue::new(d, ADMIN_TCB_OFF, ASQ_OFF, ACQ_OFF, 0);
        self.io = Queue::new(d, IO_TCB_OFF, IOSQ_OFF, IOCQ_OFF, 1);

        // De lineaire modus en de NVMMU (hoeveel slots, waar de twee
        // TCB-tabellen liggen): vóór het inschakelen.
        let q = u32::from(Q_ENTRIES - 1);
        let m = self.nvmmu();
        r.linear_sq_ctrl.update(|v| v | LINEAR_SQ_EN);
        r.max_pend.write((q << 16) | q);
        m.num.write(q);
        write_lo_hi(&m.asq_base, self.admin.tcb.0);
        write_lo_hi(&m.iosq_base, self.io.tcb.0);
        dev::mb();

        // Vanaf hier gewoon NVMe: admin-queue aanmelden, aan.
        //
        // CC wordt NIET overschreven maar bijgesteld (les 2, 29-08). iBoot
        // laat er een waarde in achter (0x474000: shutdown-normal, 128-byte
        // SQE's) en de entry-maten dáárin zijn wat de controller gebruikt.
        // Een verse CC met onze eigen maten komt niet ready; m1n1 wist
        // daarom alleen SHN en zet EN.
        r.aqa.write((q << 16) | q);
        write_lo_hi(&r.asq, self.admin.sq.0);
        write_lo_hi(&r.acq, self.admin.cq.0);
        dev::mb();
        r.cc.update(|c| (c & !CC_SHN_MASK) | CC_EN);
        self.wait_ready(true)?;

        self.identify()?;
        self.create_io_queues()?;
        if self.cfg.secure_bar {
            // M4 (`nvme-secure-bar`): zonder deze twee crasht de coprocessor
            // met een crashlog waarin de I/O-queues op nul staan (m1n1,
            // NVME_T8132).
            write_lo_hi(&r.ioq_cqes, self.io.cq.0);
            write_lo_hi(&r.ioq_cmds, self.io.sq.0);
            dev::mb();
        }
        self.capacity_from_gpt()?;
        self.started = true;
        Ok(())
    }

    /// Pollt tot `done` of tot `timeout_ns`, en trekt onderweg de mailbox
    /// leeg. De fout bij de grens is `NotReady` met CSTS en CC erin; de
    /// aanroeper maakt er iets preciezers van.
    fn wait(&mut self, timeout_ns: u64, done: impl Fn(&Regs) -> bool) -> Result<(), C::Error> {
        let r = self.regs();
        let deadline = (self.now)().saturating_add(timeout_ns);
        loop {
            if done(r) {
                return Ok(());
            }
            let csts = r.csts.read();
            if (self.now)() >= deadline {
                return Err(Error::NotReady {
                    csts,
                    cc: r.cc.read(),
                    want: csts & CSTS_RDY == 0,
                });
            }
            self.service()?;
            core::hint::spin_loop();
        }
    }

    /// Wacht tot CSTS.RDY `want` is; een fatale status stopt meteen.
    fn wait_ready(&mut self, want: bool) -> Result<(), C::Error> {
        let r = self.regs();
        let cfs = |csts: u32| csts & CSTS_CFS != 0 || csts == u32::MAX;
        let res = self.wait(READY_TIMEOUT_NS, |r| {
            let c = r.csts.read();
            cfs(c) || (c & CSTS_RDY != 0) == want
        });
        let csts = r.csts.read();
        match res {
            Ok(()) if !cfs(csts) => Ok(()),
            Ok(()) | Err(Error::NotReady { .. }) => Err(Error::NotReady {
                csts,
                cc: r.cc.read(),
                want,
            }),
            Err(e) => Err(e),
        }
    }

    /// Zet `m` op `tag` van een queue: de SQE, de TCB, de mailbox leeg, en de
    /// lineaire deurbel met de tag. Keert meteen terug.
    fn post(&mut self, admin: bool, tag: u16, m: &Cmd) -> Result<(), C::Error> {
        if self.dead {
            return Err(Error::Dead);
        }
        let q = if admin { self.admin } else { self.io };
        // Les 3 (29-08): de opdracht staat op de plek die de deurbel
        // aanwijst; ergens anders neerzetten laat de firmware een oude
        // opdracht lezen bij een verse TCB, en dat meldt hij als
        // NVME_PERM_ERR, met een crash erachteraan die de hele coprocessor
        // tot de volgende power-reset onbruikbaar maakt (vier boots gekost).
        write_sqe(q.sq, tag, m);
        write_tcb(q.tcb, tag, m);
        // Een wachtende coprocessor doet geen DMA: eerst de mailbox leeg,
        // dan de deurbel.
        self.service()?;
        let r = self.regs();
        if admin {
            r.db_linear_asq.write(u32::from(tag));
        } else {
            r.db_linear_iosq.write(u32::from(tag));
        }
        Ok(())
    }

    /// Haalt de volgende completion van een queue op als die er is: de tag
    /// en SCT/SC. Verklaart de TCB van die tag ongeldig en schuift de head
    /// op; welke opdracht erbij hoort, toetst de aanroeper.
    fn reap_one(&mut self, admin: bool) -> Result<Option<(u16, u16)>, C::Error> {
        let mut q = if admin { self.admin } else { self.io };
        let cqe = q.cq.add(u64::from(q.head) * CQE);
        let st = dev::read16(cqe.add(offset_of!(Cqe, status) as u64));
        if (st & 1 != 0) != q.phase {
            return Ok(None);
        }
        // De phase vóór de inhoud.
        dev::mb();
        let cid = dev::read16(cqe.add(offset_of!(Cqe, cid) as u64));
        // De NVMMU houdt de tag vast tot hij ongeldig verklaard is; zonder
        // dat is de tabel na 64 opdrachten vol en hangt de volgende.
        let mmu = self.nvmmu();
        mmu.tcb_inval.write(u32::from(cid));
        let stat = mmu.tcb_stat.read();
        if stat != 0 {
            self.dead = true;
            return Err(Error::Nvmmu { slot: cid, stat });
        }
        q.head = (q.head + 1) % Q_ENTRIES;
        if q.head == 0 {
            q.phase = !q.phase;
        }
        if let Some(db) = self.regs().db.get(2 * usize::from(q.id) + 1) {
            db.write(u32::from(q.head));
        }
        if admin {
            self.admin = q;
        } else {
            self.io = q;
        }
        self.commands += 1;
        Ok(Some((cid, st >> 1)))
    }

    /// Eén opdracht op [`SLOT`] die ter plekke wacht, terwijl de mailbox
    /// leeggetrokken wordt: alleen voor de opstart, de GPT en het
    /// afsluiten, als er niets anders draait en niets in de lucht is. Het
    /// blokcontract wacht nooit zo (zie de impl van
    /// `blkdev::AsyncBlockDevice`).
    fn exec(&mut self, admin: bool, m: Cmd) -> Result<(), C::Error> {
        self.post(admin, SLOT, &m)?;
        let t0 = (self.now)();
        let deadline = t0.saturating_add(COMMAND_TIMEOUT_NS);
        let status = loop {
            if let Some((cid, st)) = self.reap_one(admin)? {
                if cid != SLOT {
                    self.dead = true;
                    return Err(Error::Cid {
                        got: cid,
                        want: SLOT,
                    });
                }
                break st;
            }
            if (self.now)() >= deadline {
                self.dead = true;
                return Err(Error::Timeout { opc: m.opc });
            }
            self.service()?;
            core::hint::spin_loop();
        };
        self.slowest_ns = self.slowest_ns.max((self.now)().saturating_sub(t0));
        match status {
            0 => Ok(()),
            s => Err(Error::Status {
                opc: m.opc,
                status: s,
            }),
        }
    }

    /// Alleen de controller-identify (CNS 1): model en MDTS. De namespace
    /// bevrágen mag niet: de firmware antwoordt met NVME_PERM_ERR en valt om
    /// (29-08, uit zijn eigen crashlog). m1n1 doet die vraag ook niet.
    fn identify(&mut self) -> Result<(), C::Error> {
        let buf = self.data();
        self.exec(
            true,
            Cmd {
                opc: ADM_IDENTIFY,
                prp1: buf.0,
                cdw10: 1,
                ..Cmd::default()
            },
        )?;
        dev::pull(buf, PAGE as usize);
        dev::copy_out(&mut self.model, buf.add(24));
        let mdts = dev::read8(buf.add(77));
        if mdts != 0 && mdts < 52 {
            self.max_transfer = MAX_TRANSFER.min(PAGE << mdts);
        }
        Ok(())
    }

    /// Het I/O-queue-paar: CQ eerst, zonder interrupts.
    fn create_io_queues(&mut self) -> Result<(), C::Error> {
        let q = u32::from(Q_ENTRIES - 1) << 16;
        let id = u32::from(self.io.id);
        self.exec(
            true,
            Cmd {
                opc: ADM_CREATE_CQ,
                prp1: self.io.cq.0,
                cdw10: q | id,
                cdw11: 1, // PC
                ..Cmd::default()
            },
        )?;
        self.exec(
            true,
            Cmd {
                opc: ADM_CREATE_SQ,
                prp1: self.io.sq.0,
                cdw10: q | id,
                cdw11: (id << 16) | 1, // CQID, PC
                ..Cmd::default()
            },
        )
    }

    /// Hoe groot de schijf is. De ANS meldt dat niet (TNVMCAP is nul, en de
    /// namespace bevragen mag niet); de schijf zegt het zelf: de GPT-header
    /// op blok 1 draagt het adres van zijn reservekopie, en die ligt op het
    /// laatste blok. Zonder GPT weigeren we: een transfer zonder bovengrens
    /// is hoe je andermans data overschrijft.
    fn capacity_from_gpt(&mut self) -> Result<(), C::Error> {
        self.blocks = 2; // Net genoeg om blok 1 te mogen lezen.
        let res = self.transfer(IO_READ, 1, BLOCK as usize);
        let buf = self.data();
        dev::pull(buf, BLOCK as usize);
        let sig = dev::read64(buf);
        self.blocks = 0;
        res?;
        if sig != u64::from_le_bytes(*b"EFI PART") {
            return Err(Error::NoGpt);
        }
        let backup = dev::read64(buf.add(32));
        let first = dev::read64(buf.add(40));
        let last = dev::read64(buf.add(48));
        if !(2..=1 << 44).contains(&backup) || first < 2 || first > last || last >= backup {
            return Err(Error::Gpt {
                backup,
                first,
                last,
            });
        }
        self.blocks = backup + 1;
        self.usable = (first, last);
        Ok(())
    }

    /// De `i`-de MiB van het datablok: de buffer van `IO_TAGS[i]`.
    fn buf(&self, i: usize) -> Pa {
        self.data().add(i as u64 * MAX_TRANSFER)
    }

    /// De paginawijzers voor `n` bytes in buffer `i`, met de PRP-lijst van
    /// die buffer.
    fn prps(&self, i: usize, n: u64) -> (u64, u64) {
        let data = self.buf(i);
        if n <= PAGE {
            return (data.0, 0);
        }
        if n <= 2 * PAGE {
            return (data.0, data.0 + PAGE);
        }
        let list = self.cfg.dma.add(PRP_OFF + i as u64 * PAGE);
        dev::clear(list, PAGE as usize);
        for p in 1..n.div_ceil(PAGE) {
            dev::write64(list.add((p - 1) * 8), data.0 + p * PAGE);
        }
        (data.0, list.0)
    }

    /// De opdracht over `len` bytes vanaf blok `block` via buffer `i`; de
    /// grenzen van de schijf en de buffer getoetst.
    fn rw_cmd(&self, opc: u8, block: u64, len: usize, i: usize) -> Result<Cmd, C::Error> {
        let n = len as u64;
        let bad = Error::Range { block, len };
        if len == 0 || !n.is_multiple_of(BLOCK) || n > self.max_transfer || i >= DEPTH {
            return Err(bad);
        }
        let nlb = n / BLOCK;
        if block.checked_add(nlb).is_none_or(|end| end > self.blocks) {
            return Err(bad);
        }
        let nlb0 = u32::try_from(nlb - 1).map_err(|_| bad)?;
        let (prp1, prp2) = self.prps(i, n);
        Ok(Cmd {
            opc,
            nsid: NSID,
            prp1,
            prp2,
            cdw10: (block & 0xffff_ffff) as u32,
            cdw11: (block >> 32) as u32,
            cdw12: nlb0,
        })
    }

    /// Eén opdracht die ter plekke wacht, via buffer 0 (de GPT bij de
    /// opstart).
    fn transfer(&mut self, opc: u8, block: u64, len: usize) -> Result<(), C::Error> {
        let m = self.rw_cmd(opc, block, len, 0)?;
        self.exec(false, m)
    }

    fn ready(&self) -> Result<(), C::Error> {
        match (self.started, self.dead) {
            (_, true) => Err(Error::Dead),
            (false, _) => Err(Error::NotStarted),
            _ => Ok(()),
        }
    }

    /// De hapgrootte: de grootste transfer, in hele blokken.
    fn step(&self) -> usize {
        (self.max_transfer - self.max_transfer % BLOCK).max(BLOCK) as usize
    }

    /// Leest `buf.len()` bytes (een veelvoud van [`BLOCK`]) vanaf blok
    /// `block`. Lezen mag overal op de schijf. Voor het board bij de probe:
    /// de GPT ligt buiten het venster, en het blokcontract (hieronder) ziet
    /// alleen het venster.
    pub fn read_at(&mut self, block: u64, buf: &mut [u8]) -> Result<(), C::Error> {
        self.ready()?;
        if self.inflight.iter().any(Option::is_some) {
            return Err(Error::Busy);
        }
        if buf.is_empty() {
            return Err(Error::Range { block, len: 0 });
        }
        let step = self.step();
        let mut b = block;
        for chunk in buf.chunks_mut(step) {
            self.transfer(IO_READ, b, chunk.len())?;
            dev::pull(self.data(), chunk.len());
            dev::copy_out_normal(chunk, self.data());
            b += chunk.len() as u64 / BLOCK;
        }
        Ok(())
    }

    /// De grens van schrijven: het hele verzoek binnen het venster.
    fn check_window(&self, block: u64, len: usize) -> Result<(), C::Error> {
        let Some(w) = self.window else {
            return Err(Error::NoWindow { block, len });
        };
        let n = (len as u64).div_ceil(BLOCK);
        let inside = len != 0
            && block >= w.first
            && block
                .checked_add(n)
                .is_some_and(|end| end <= w.first + w.blocks);
        if !inside {
            return Err(Error::OutsideWindow {
                block,
                len,
                first: w.first,
                blocks: w.blocks,
            });
        }
        Ok(())
    }

    /// Zet het schrijfvenster: `blocks` blokken van [`BLOCK`] vanaf
    /// `first`. Het moet binnen het bruikbare deel van de GPT vallen (nooit
    /// over de headers of de entry-tabellen); dat het ook buiten de
    /// partities van macOS valt, is de keuze van de aanroeper (het gat uit
    /// `fw::gpt`).
    pub fn set_window(&mut self, first: u64, blocks: u64) -> Result<(), C::Error> {
        self.ready()?;
        let (uf, ul) = self.usable;
        let ok = blocks != 0
            && first >= uf
            && first.checked_add(blocks - 1).is_some_and(|last| last <= ul);
        if !ok {
            return Err(Error::BadWindow {
                first,
                blocks,
                usable_first: uf,
                usable_last: ul,
            });
        }
        // INVARIANT: het venster ligt binnen `usable`, getoetst hierboven.
        self.window = Some(Window { first, blocks });
        Ok(())
    }

    /// Haalt het schrijfvenster weg: vanaf nu weigert elke schrijf.
    pub fn clear_window(&mut self) {
        self.window = None;
    }

    /// Het schrijfvenster als (eerste blok, blokken).
    #[must_use]
    pub fn window(&self) -> Option<(u64, u64)> {
        self.window.map(|w| (w.first, w.blocks))
    }

    /// Geeft de ANS terug zoals we hem aantroffen: I/O-queues weg, de
    /// controller via CC.SHN netjes uit, en de coprocessor in slaap.
    ///
    /// Les 4 (29-08): een omgevallen of achtergelaten coprocessor blijft dat,
    /// ook na een warme herstart; elke volgende boot komt de controller niet
    /// meer ready. Daarom loopt dit ook op een dode driver door (zonder de
    /// queue-opdrachten) en eindigt het altijd met de slaap; de eerste fout
    /// komt terug.
    pub fn shutdown(&mut self) -> Result<(), C::Error> {
        let mut first: Option<Error<C::Error>> = None;
        let mut note = |r: Result<(), C::Error>| {
            if let Err(e) = r {
                first.get_or_insert(e);
            }
        };
        if self.started && !self.dead {
            let id = u32::from(self.io.id);
            let del_sq = Cmd {
                opc: ADM_DELETE_SQ,
                cdw10: id,
                ..Cmd::default()
            };
            note(self.exec(true, del_sq));
            let del_cq = Cmd {
                opc: ADM_DELETE_CQ,
                cdw10: id,
                ..Cmd::default()
            };
            note(self.exec(true, del_cq));
        }
        self.started = false;
        self.window = None;

        // CC.SHN op "normaal" en wachten tot SHST "klaar" meldt; pas dan
        // de enable eraf.
        let r = self.regs();
        r.cc.update(|c| (c & !CC_SHN_MASK) | CC_SHN_NORMAL);
        let shst = self.wait(READY_TIMEOUT_NS, |r| {
            r.csts.read() & CSTS_SHST_MASK == CSTS_SHST_DONE
        });
        note(shst.map_err(|e| match e {
            Error::NotReady { csts, .. } => Error::Shutdown { csts },
            e => e,
        }));
        r.cc.update(|c| c & !CC_EN);
        note(self.wait_ready(false));
        note(self.service());
        note(self.cop.sleep().map_err(Error::Coprocessor));
        first.map_or(Ok(()), Err)
    }

    /// De blokmaat: [`BLOCK`].
    #[must_use]
    pub fn block_size(&self) -> u64 {
        BLOCK
    }

    /// De grootte van de schijf in blokken, uit de GPT.
    #[must_use]
    pub fn blocks(&self) -> u64 {
        self.blocks
    }

    /// De grootste transfer van één opdracht in bytes.
    #[must_use]
    pub fn max_transfer(&self) -> u64 {
        self.max_transfer
    }

    /// De grootte van het venster in sectoren van [`SECTOR`] bytes: wat het
    /// blokcontract ziet. Nul zonder venster.
    #[must_use]
    pub fn sectors(&self) -> u64 {
        self.window.map_or(0, |w| w.blocks * PER_BLOCK)
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

    /// Het datablok (basis, maat) dat het board cacheable mag mappen.
    #[must_use]
    pub fn data_region(&self) -> (Pa, u64) {
        (self.data(), DATA_SIZE)
    }

    /// Wat de controller, de NVMMU en de coprocessor ervan vinden, als iets
    /// dat je kunt printen: voor een opdracht die niet terugkomt. Leest
    /// alleen registers als [`new`](Self::new) slaagde, en dat is zo als
    /// deze waarde bestaat.
    #[must_use]
    pub fn diag(&self) -> Diag<'_, C> {
        Diag { ans: self }
    }

    /// Een blok-LBA van het contract (512 bytes, 0 = begin van het venster)
    /// als ANS-blok, met de lengte getoetst.
    fn contract_block(&self, lba: u64, len: usize) -> blkdev::Result<u64> {
        let bad = blkdev::Error::OutOfRange { lba, len };
        let w = self.window.ok_or(bad)?;
        let n = len as u64;
        if len == 0 || !lba.is_multiple_of(PER_BLOCK) || !n.is_multiple_of(BLOCK) {
            return Err(bad);
        }
        let first = lba / PER_BLOCK;
        match first.checked_add(n / BLOCK) {
            Some(end) if end <= w.blocks => Ok(w.first + first),
            _ => Err(bad),
        }
    }
}

/// Het I/O-pad van het blokcontract: opdrachten op de tags van
/// [`IO_TAGS`], hoogstens één van de eigenaar ([`Use::Demand`]) en één
/// read-ahead tegelijk. Niets hier wacht: `issue` zet een opdracht op de
/// controller, `reap_io` haalt op wat terug is.
impl<C: Coprocessor> Ans<C> {
    /// De index van de opdracht van de eigenaar.
    fn demand(&self) -> Option<usize> {
        self.inflight
            .iter()
            .position(|f| f.is_some_and(|f| f.use_ == Use::Demand))
    }

    /// Zet `m` op tag `IO_TAGS[i]` met buffer `i`.
    fn issue(&mut self, i: usize, m: &Cmd, f: Inflight) -> Result<(), C::Error> {
        let tag = IO_TAGS.get(i).copied().ok_or(Error::Busy)?;
        self.post(false, tag, m)?;
        if let Some(s) = self.inflight.get_mut(i) {
            *s = Some(Inflight {
                t0: (self.now)(),
                ..f
            });
        }
        Ok(())
    }

    /// Haalt elke completion van de I/O-queue op die er is, en toetst de
    /// time-out van wat nog loopt. Een completion voor een tag die niet in
    /// de lucht is, of een opdracht over zijn grens, maakt de driver dood:
    /// de controller kan dan nog in een buffer schrijven.
    fn reap_io(&mut self) -> Result<(), C::Error> {
        while let Some((cid, st)) = self.reap_one(false)? {
            let i = IO_TAGS.iter().position(|&t| t == cid);
            let now = (self.now)();
            let Some(f) = i
                .and_then(|i| self.inflight.get_mut(i))
                .and_then(|s| s.as_mut().filter(|f| f.done.is_none()))
            else {
                self.dead = true;
                return Err(Error::Cid {
                    got: cid,
                    want: IO_TAGS[0],
                });
            };
            f.done = Some(st);
            let (dt, stale) = (
                now.saturating_sub(f.t0),
                f.use_ == Use::Ahead { stale: true },
            );
            self.slowest_ns = self.slowest_ns.max(dt);
            if stale && let Some(s) = i.and_then(|i| self.inflight.get_mut(i)) {
                *s = None;
                self.ahead_waste += 1;
            }
        }
        // Wat nog loopt, vraagt om de mailbox (een wachtende coprocessor doet
        // geen DMA). Eerst de CQ, zoals de oude wachtlus: de mailbox kost
        // Device-loads, en een completion die er al is wacht daar niet op.
        if self.inflight.iter().flatten().any(|f| f.done.is_none()) {
            self.service()?;
        }
        let now = (self.now)();
        let late = self
            .inflight
            .iter()
            .flatten()
            .find(|f| f.done.is_none() && now >= f.t0.saturating_add(COMMAND_TIMEOUT_NS));
        if let Some(f) = late {
            let opc = f.opc;
            self.dead = true;
            return Err(Error::Timeout { opc });
        }
        Ok(())
    }

    /// Een vrije tag. Er is er altijd een: de eigenaar heeft hoogstens één
    /// opdracht, en er is hoogstens één read-ahead ([`Self::read_ahead`]).
    fn free_tag(&self) -> Result<usize, C::Error> {
        self.inflight
            .iter()
            .position(Option::is_none)
            .ok_or(Error::Busy)
    }

    /// Maakt de read-aheads onbruikbaar die `range` (eerste blok, aantal)
    /// raken, of allemaal bij `None`. Wat al terug is, gaat meteen weg; wat
    /// nog loopt, wordt weggegooid zodra het terugkomt.
    fn drop_aheads(&mut self, range: Option<(u64, u64)>) {
        for s in &mut self.inflight {
            let Some(f) = s else { continue };
            let hit = range.is_none_or(|(b, n)| {
                b < f.block + f.len as u64 / BLOCK && f.block < b.saturating_add(n)
            });
            if f.use_ != (Use::Ahead { stale: false }) || !hit {
                continue;
            }
            if f.done.is_some() {
                *s = None;
                self.ahead_waste += 1;
            } else {
                f.use_ = Use::Ahead { stale: true };
            }
        }
    }

    /// Een lees van de eigenaar: van de read-ahead als die precies deze
    /// blokken leest (of las, zonder fout), anders een nieuwe opdracht.
    fn start_read(&mut self, block: u64, len: usize, lba: u64) -> Result<(), C::Error> {
        self.ready()?;
        let hit = self.inflight.iter_mut().flatten().find(|f| {
            f.use_ == (Use::Ahead { stale: false })
                && f.block == block
                && f.len == len
                && f.done.is_none_or(|s| s == 0)
        });
        if let Some(f) = hit {
            f.use_ = Use::Demand;
            f.lba = lba;
            self.ahead_hits += 1;
            return Ok(());
        }
        self.drop_aheads(None);
        let i = self.free_tag()?;
        let m = self.rw_cmd(IO_READ, block, len, i)?;
        self.issue(i, &m, inflight(IO_READ, block, len, lba, Use::Demand))
    }

    /// Een schrijf van de eigenaar: binnen het venster, de bytes nu in de
    /// buffer van een vrije tag (daarna zijn ze van de aanroeper terug), en
    /// een read-ahead over dezelfde blokken telt niet meer.
    fn start_write(&mut self, block: u64, data: &[u8], lba: u64) -> Result<(), C::Error> {
        self.ready()?;
        self.check_window(block, data.len())?;
        self.drop_aheads(Some((block, data.len() as u64 / BLOCK)));
        let i = self.free_tag()?;
        let m = self.rw_cmd(IO_WRITE, block, data.len(), i)?;
        dev::copy_in_normal(self.buf(i), data);
        dev::push(self.buf(i), data.len());
        self.issue(
            i,
            &m,
            inflight(IO_WRITE, block, data.len(), lba, Use::Demand),
        )
    }

    /// Maakt alles wat de controller al bevestigde duurzaam (NVMe Flush).
    ///
    /// De Go-versie liet dit op de ANS weg ("nooit op ijzer gezien", en een
    /// onverwachte opdracht kan de coprocessor omleggen). Met schrijven erbij
    /// kan dat niet meer: zonder flush is een geschreven boom niet duurzaam.
    /// Linux stuurt hem op dit pad gewoon (`drivers/nvme/host/apple.c`).
    fn start_flush(&mut self) -> Result<(), C::Error> {
        self.ready()?;
        let i = self.free_tag()?;
        let m = Cmd {
            opc: IO_FLUSH,
            nsid: NSID,
            ..Cmd::default()
        };
        self.issue(i, &m, inflight(IO_FLUSH, 0, 0, 0, Use::Demand))
    }

    /// Na een lees van een volle hap die precies verder ging waar de vorige
    /// eindigde: de volgende hap alvast, op een vrije tag, binnen het
    /// venster. Zo leest de schijf de volgende MiB terwijl de kern deze naar
    /// de app brengt (GEMETEN 01-10: schijf 0,59 en transport 0,53 ms per
    /// MiB, tot M21 na elkaar). Eén tegelijk; een fout hier is een dode
    /// driver en dat zegt de volgende opdracht.
    fn read_ahead(&mut self, block: u64, len: usize) {
        let Some(w) = self.window else { return };
        let end = block.saturating_add(len as u64 / BLOCK);
        let busy = self
            .inflight
            .iter()
            .flatten()
            .any(|f| matches!(f.use_, Use::Ahead { .. }));
        let Some(i) = self.inflight.iter().position(Option::is_none) else {
            return;
        };
        if busy || end > w.first + w.blocks {
            return;
        }
        let lba = (block - w.first) * PER_BLOCK;
        if let Ok(m) = self.rw_cmd(IO_READ, block, len, i) {
            let f = inflight(IO_READ, block, len, lba, Use::Ahead { stale: false });
            let _ = self.issue(i, &m, f);
        }
    }

    /// De completion van de opdracht van de eigenaar; bij een lees de bytes
    /// naar `into`, en zo nodig eerst de read-ahead op de controller.
    fn poll_demand(&mut self, into: &mut [u8]) -> core::task::Poll<Result<(), C::Error>> {
        use core::task::Poll;
        let Some(i) = self.demand() else {
            return Poll::Ready(Err(Error::NotStarted));
        };
        if let Err(e) = self.reap_io() {
            return Poll::Ready(Err(e));
        }
        let Some(f) = self.inflight.get(i).copied().flatten() else {
            return Poll::Ready(Err(Error::Dead));
        };
        let Some(st) = f.done else {
            return Poll::Pending;
        };
        // Een flush is pas klaar als er niets anders meer in de lucht is: na
        // de laatste flush van een bevriezing (de kern-flip) is de rij leeg.
        let others = self.inflight.iter().flatten().any(|g| g.done.is_none());
        if f.opc == IO_FLUSH && others {
            return Poll::Pending;
        }
        if st == 0 && f.opc == IO_READ {
            let seq = f.block == self.seq_end;
            self.seq_end = f.block + f.len as u64 / BLOCK;
            if seq && f.len == self.step() {
                self.read_ahead(self.seq_end, f.len);
            }
            let n = f.len.min(into.len());
            dev::pull(self.buf(i), n);
            if let Some(d) = into.get_mut(..n) {
                dev::copy_out_normal(d, self.buf(i));
            }
        }
        if let Some(s) = self.inflight.get_mut(i) {
            *s = None;
        }
        Poll::Ready(match st {
            0 => Ok(()),
            s => Err(Error::Status {
                opc: f.opc,
                status: s,
            }),
        })
    }
}

/// Een opdracht in opbouw voor [`Ans::issue`].
fn inflight(opc: u8, block: u64, len: usize, lba: u64, use_: Use) -> Inflight {
    Inflight {
        opc,
        block,
        len,
        lba,
        t0: 0,
        use_,
        done: None,
    }
}

/// Het blokcontract van `blkdev` over het venster: LBA's van 512 bytes,
/// LBA 0 is het eerste blok van het venster. De ANS rekent in blokken van
/// 4 KB, dus een verzoek moet op een blok beginnen en eindigen; hopfs
/// schrijft in blokken van 4 KB vanaf LBA 0, dus dat doet hij altijd. Een
/// verzoek dat dat niet doet, of buiten het venster valt, is `OutOfRange`.
///
/// `start` zet de opdracht op de controller en keert terug; `poll_done`
/// haalt op wat terug is. Er wacht niets: tot 01-10 (M21) deed `start` de
/// hele opdracht en spinde de OS-core 4 us (4 KB) tot 600 us (1 MiB) per
/// opdracht, en in die tijd stond alles stil, ook het transport van elke
/// andere app. GEMETEN 01-10 (`m4-scale.sh`, twee apps met elk 256 MiB
/// schrijven en lezen): M21 samen ~985 MB/s opgeteld, net als één alleen;
/// M22 ~1400 tegen ~1280 voor één alleen. Met de read-ahead erbij leest een
/// app 1690 MB/s in plaats van 870 tot 960. De wachter
/// (`blkdev::InFlight::done`) pollt op [`POLL_SPIN_NS`] en [`POLL_PERIOD`];
/// de mailbox van de coprocessor gaat leeg zolang er iets loopt.
///
/// Synchroon blijft synchroon: een schrijf of flush is pas klaar als zijn
/// eigen completion terug is, en de eigenaar heeft er één tegelijk. De
/// read-ahead is de enige tweede opdracht, en die leest alleen.
impl<C: Coprocessor> blkdev::AsyncBlockDevice for Ans<C> {
    fn max_transfer(&self) -> usize {
        self.step()
    }

    fn poll_pace(&self) -> (u64, core::time::Duration) {
        (POLL_SPIN_NS, POLL_PERIOD)
    }

    fn start(&mut self, op: blkdev::Op<'_>) -> blkdev::Result {
        if let Some(i) = self.demand() {
            // De wachter van de vorige ging weg (een gedropte `Done`): pas
            // als die opdracht terug is, is haar buffer weer van ons.
            self.reap_io().map_err(|e| blk_err(&e, 0, 0))?;
            match self.inflight.get_mut(i) {
                Some(s) if s.is_some_and(|f| f.done.is_some()) => *s = None,
                _ => return Err(blkdev::Error::Busy),
            }
        }
        match op {
            blkdev::Op::Read { lba, len } => {
                let b = self.contract_block(lba, len)?;
                self.start_read(b, len, lba)
                    .map_err(|e| blk_err(&e, lba, len))
            }
            blkdev::Op::Write { lba, data } => {
                let b = self.contract_block(lba, data.len())?;
                self.start_write(b, data, lba)
                    .map_err(|e| blk_err(&e, lba, data.len()))
            }
            blkdev::Op::Flush => self.start_flush().map_err(|e| blk_err(&e, 0, 0)),
        }
    }

    fn poll_done(&mut self, into: &mut [u8]) -> core::task::Poll<blkdev::Result> {
        let lba = self
            .demand()
            .and_then(|i| self.inflight.get(i).copied().flatten())
            .map_or(0, |f| f.lba);
        self.poll_demand(into).map_err(|e| blk_err(&e, lba, 0))
    }
}

fn blk_err<E>(e: &Error<E>, lba: u64, len: usize) -> blkdev::Error {
    match e {
        Error::Range { .. } | Error::NoWindow { .. } | Error::OutsideWindow { .. } => {
            blkdev::Error::OutOfRange { lba, len }
        }
        Error::Dead
        | Error::Timeout { .. }
        | Error::Cid { .. }
        | Error::Nvmmu { .. }
        | Error::Coprocessor(_) => blkdev::Error::Dead,
        Error::Busy => blkdev::Error::Busy,
        _ => blkdev::Error::Io { lba },
    }
}

/// Een diagnose van de ANS (zie [`Ans::diag`]).
pub struct Diag<'a, C: Coprocessor> {
    ans: &'a Ans<C>,
}

impl<C: Coprocessor> fmt::Display for Diag<'_, C> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let a = self.ans;
        let r = a.regs();
        let q = a.admin;
        let cqe = q.cq.add(u64::from(q.head) * CQE);
        write!(
            f,
            "boot={:#x} csts={:#x} cc={:#x} tcb_stat={:#x} adm_cqe[{}]={:08x}/{:08x}/{:08x}/{:08x} dead={} commands={}",
            r.boot_status.read(),
            r.csts.read(),
            r.cc.read(),
            a.nvmmu().tcb_stat.read(),
            q.head,
            dev::read32(cqe),
            dev::read32(cqe.add(4)),
            dev::read32(cqe.add(8)),
            dev::read32(cqe.add(12)),
            a.dead,
            a.commands,
        )?;
        f.write_str(" crashlog=")?;
        a.cop.crashlog(f)
    }
}

/// Zet de SQE van `tag` op `tag * 64`, zoals Linux (`apple.c`): in de
/// lineaire modus leest de firmware per 64 bytes, ONGEACHT CC.IOSQES (wij
/// houden iBoots 7, les 2). GEMETEN 01-10 op de M4: op `tag * 128` (M25)
/// faalt elke opdracht op tag 1 met een status, op `tag * 64` (M26) niet.
fn write_sqe(sq: Pa, tag: u16, m: &Cmd) {
    let sqe = sq.add(u64::from(tag) * SQE);
    dev::clear(sqe, SQE as usize);
    let at = |off: usize| sqe.add(off as u64);
    dev::write32(
        at(offset_of!(Sqe, cdw0)),
        u32::from(m.opc) | (u32::from(tag) << 16),
    );
    dev::write32(at(offset_of!(Sqe, nsid)), m.nsid);
    dev::write64(at(offset_of!(Sqe, prp1)), m.prp1);
    dev::write64(at(offset_of!(Sqe, prp2)), m.prp2);
    dev::write32(at(offset_of!(Sqe, cdw10)), m.cdw10);
    dev::write32(at(offset_of!(Sqe, cdw11)), m.cdw11);
    dev::write32(at(offset_of!(Sqe, cdw12)), m.cdw12);
    dev::mb();
}

/// Vult de TCB van `slot`. De richting van de DMA: oneven opcodes
/// schrijven (0x01), even lezen (0x02, identify 0x06); zonder buffer geen
/// richting (de Go-versie, op ijzer bewezen voor lezen). De opcode zelf gaat
/// erin zoals Linux het doet; de Go-versie liet hem nul, en lezen werkte ook
/// zo.
fn write_tcb(table: Pa, slot: u16, m: &Cmd) {
    let tcb = table.add(u64::from(slot) * TCB);
    dev::clear(tcb, TCB as usize);
    let at = |off: usize| tcb.add(off as u64);
    dev::write8(at(offset_of!(Tcb, opcode)), m.opc);
    if m.prp1 != 0 {
        let dir = if m.opc & 1 != 0 {
            TCB_TO_DEVICE
        } else {
            TCB_FROM_DEVICE
        };
        dev::write8(at(offset_of!(Tcb, dma_flags)), dir);
    }
    dev::write8(at(offset_of!(Tcb, command_id)), slot as u8);
    dev::write32(at(offset_of!(Tcb, length)), m.cdw12);
    dev::write64(at(offset_of!(Tcb, prp1)), m.prp1);
    dev::write64(at(offset_of!(Tcb, prp2)), m.prp2);
    dev::mb();
}

/// Schrijft een 64-bit registerpaar in twee helften, laag eerst (m1n1 heeft
/// er een eigen helper voor).
fn write_lo_hi(r: &[Reg<u32>; 2], v: u64) {
    let [lo, hi] = r;
    lo.write((v & 0xffff_ffff) as u32);
    hi.write((v >> 32) as u32);
}

#[cfg(test)]
mod tests {
    //! De ANS tegen een nep-controller: NVMe- en NVMMU-venster en de
    //! DMA-regio in RAM, en de klok van de test IS de controller. Bij elke
    //! blik op de klok spiegelt hij CC in CSTS (maar alleen na het
    //! wekbericht en met iBoots IOSQES = 7 in CC: les 1 en 2), voert hij de
    //! opdracht uit op het slot dat een lineaire deurbel aanwijst, met de
    //! entry-afstand uit CC (les 3), toetst hij de TCB, en zet hij de
    //! completion terug. De schijf is RAM met een GPT-header op blok 1.

    use super::*;
    use blkdev::{BlockIo, Paced, Spin, block_on};
    use std::cell::RefCell;
    use std::string::{String, ToString};
    use std::vec;
    use std::vec::Vec;

    const NBLOCKS: u64 = 64;
    const USABLE: (u64, u64) = (6, NBLOCKS - 6);
    /// Wat iBoot in CC achterlaat (29-08 gemeten): SHN = 01, IOSQES = 7,
    /// IOCQES = 4.
    const IBOOT_CC: u32 = 0x0047_4000;
    const NOTHING: u32 = u32::MAX;

    #[derive(Default)]
    struct Ctl {
        regs: Pa,
        mmu: Pa,
        disk: Vec<u8>,
        booted: bool,
        /// Per queue: (SQ, CQ, CQ-tail, phase).
        q: [Option<(Pa, Pa, u16, bool)>; 2],
        /// Wat er uitgevoerd werd: (queue, opcode, blok, nlb, slot).
        log: Vec<(usize, u8, u64, u32, u32)>,
        /// De TCB-richting per uitgevoerde opdracht.
        dirs: Vec<u8>,
        /// Opdrachten waarvan de TCB niet bij het slot hoorde.
        tcb_mismatch: u32,
        invalidated: Vec<u32>,
        mute: bool,
        now: u64,
        events: Vec<&'static str>,
        /// Houd de completions van de I/O-queue vast tot [`release`]: de
        /// opdracht is uitgevoerd (de DMA is gebeurd), maar niet bevestigd.
        hold: bool,
        /// Vastgehouden completions: (tag, SCT/SC, opcode).
        held: Vec<(u16, u16, u8)>,
        /// Bevestigde I/O-opdrachten, in volgorde: (opcode, tag).
        acked: Vec<(u8, u16)>,
        /// De MDTS van de identify (0 = geen grens).
        mdts: u8,
    }

    thread_local! {
        static CTL: RefCell<Ctl> = RefCell::new(Ctl::default());
    }

    fn lo_hi(p: Pa) -> u64 {
        u64::from(dev::read32(p)) | (u64::from(dev::read32(p.add(4))) << 32)
    }

    fn clock() -> u64 {
        CTL.with(|c| {
            let mut c = c.borrow_mut();
            c.now += 10_000;
            tick(&mut c);
            c.now
        })
    }

    fn tick(c: &mut Ctl) {
        let r = c.regs;
        if r.0 == 0 {
            return;
        }
        let cc = dev::read32(r.add(0x14));
        let mut csts = dev::read32(r.add(0x1c));
        let iosqes = (cc >> 16) & 0xf;
        let rdy = cc & CC_EN != 0 && c.booted && iosqes == 7;
        csts = (csts & !CSTS_RDY) | u32::from(rdy);
        if cc & CC_SHN_MASK == CC_SHN_NORMAL && cc & CC_EN != 0 {
            csts = (csts & !CSTS_SHST_MASK) | CSTS_SHST_DONE;
        }
        dev::write32(r.add(0x1c), csts);
        let inval = c.mmu.add(0x28118);
        let v = dev::read32(inval);
        if v != NOTHING {
            c.invalidated.push(v);
            dev::write32(inval, NOTHING);
        }
        if c.mute || !rdy {
            return;
        }
        for (qi, db) in [(0usize, 0x2490c), (1, 0x24910)] {
            let slot = dev::read32(r.add(db));
            if slot != NOTHING {
                dev::write32(r.add(db), NOTHING);
                // De lineaire modus: 64 bytes per entry, ongeacht IOSQES (M26).
                exec(c, qi, slot, SQE);
            }
        }
    }

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

    fn exec(c: &mut Ctl, qi: usize, slot: u32, stride: u64) {
        let (sq, tcbs) = if qi == 0 {
            (Pa(lo_hi(c.regs.add(0x28))), Pa(lo_hi(c.mmu.add(0x28108))))
        } else {
            (c.q[1].unwrap().0, Pa(lo_hi(c.mmu.add(0x28110))))
        };
        let sqe = sq.add(u64::from(slot) * stride);
        let cdw0 = dev::read32(sqe);
        let opc = (cdw0 & 0xff) as u8;
        let cid = (cdw0 >> 16) as u16;
        let prp1 = dev::read64(sqe.add(24));
        let prp2 = dev::read64(sqe.add(32));
        let cdw10 = dev::read32(sqe.add(40));
        let cdw11 = dev::read32(sqe.add(44));
        let cdw12 = dev::read32(sqe.add(48));
        let tcb = tcbs.add(u64::from(slot) * 128);
        let t_opc = dev::read8(tcb);
        let t_dir = dev::read8(tcb.add(1));
        let t_id = u32::from(dev::read8(tcb.add(2)));
        let t_len = dev::read32(tcb.add(4));
        let t_prp1 = dev::read64(tcb.add(24));
        c.dirs.push(t_dir);
        // De firmware: een TCB die niet bij deze opdracht hoort is
        // NVME_PERM_ERR (status 0x8 hier).
        let mut sc: u16 = 0;
        if t_id != slot || t_opc != opc || t_len != cdw12 || t_prp1 != prp1 {
            c.tcb_mismatch += 1;
            sc = 0x8;
        }
        let block = u64::from(cdw10) | (u64::from(cdw11) << 32);
        c.log.push((qi, opc, block, cdw12, slot));
        if sc == 0 {
            if qi == 0 {
                admin(c, opc, prp1, prp2, cdw10);
            } else {
                sc = io(c, opc, block, cdw12, prp1, prp2);
            }
        }
        if qi == 1 && c.hold {
            c.held.push((cid, sc, opc));
            return;
        }
        complete(c, qi, cid, sc, opc);
    }

    /// Zet de completion van `cid` in de CQ van queue `qi`.
    fn complete(c: &mut Ctl, qi: usize, cid: u16, sc: u16, opc: u8) {
        if qi == 1 {
            c.acked.push((opc, cid));
        }
        let (cq, tail, phase) = if qi == 0 {
            let (t, p) = c.q[0].map_or((0, true), |q| (q.2, q.3));
            (Pa(lo_hi(c.regs.add(0x30))), t, p)
        } else {
            let q = c.q[1].unwrap();
            (q.1, q.2, q.3)
        };
        let cqe = cq.add(u64::from(tail) * 16);
        dev::write16(cqe.add(12), cid);
        dev::write16(cqe.add(14), (sc << 1) | u16::from(phase));
        let t = (tail + 1) % Q_ENTRIES;
        let p = if t == 0 { !phase } else { phase };
        if qi == 0 {
            let old = c.q[0].unwrap_or((Pa(0), cq, 0, true));
            c.q[0] = Some((old.0, old.1, t, p));
        } else if let Some(q) = c.q[1].as_mut() {
            q.2 = t;
            q.3 = p;
        }
    }

    fn admin(c: &mut Ctl, opc: u8, prp1: u64, _prp2: u64, cdw10: u32) {
        match opc {
            ADM_IDENTIFY => {
                assert_eq!(cdw10, 1, "only the controller identify (CNS 1)");
                dev::clear(Pa(prp1), 4096);
                dev::copy_in(Pa(prp1 + 24), b"APPLE SSD AP0512Z                       ");
                dev::write8(Pa(prp1 + 77), c.mdts);
            }
            ADM_CREATE_CQ => c.q[1] = Some((Pa(0), Pa(prp1), 0, true)),
            ADM_CREATE_SQ => {
                if let Some(q) = c.q[1].as_mut() {
                    q.0 = Pa(prp1);
                }
            }
            ADM_DELETE_SQ | ADM_DELETE_CQ => c.events.push("delete queue"),
            _ => {}
        }
    }

    fn io(c: &mut Ctl, opc: u8, block: u64, cdw12: u32, prp1: u64, prp2: u64) -> u16 {
        if opc == IO_FLUSH {
            c.events.push("flush");
            return 0;
        }
        let n = u64::from(cdw12 & 0xffff) + 1;
        let len = n * BLOCK;
        if block + n > NBLOCKS {
            return 0x80;
        }
        let mut off = (block * BLOCK) as usize;
        for p in pages(prp1, prp2, len) {
            let chunk = &mut c.disk[off..off + PAGE as usize];
            if opc == IO_WRITE {
                dev::copy_out(chunk, Pa(p));
            } else {
                dev::copy_in(Pa(p), chunk);
            }
            off += PAGE as usize;
        }
        0
    }

    /// De coprocessor van de test: het wekbericht zet BOOT_STATUS en maakt
    /// de controller bereid om ready te worden (les 1).
    struct Cop {
        services: u32,
        crashed: bool,
    }

    impl Coprocessor for Cop {
        type Error = u32;
        fn boot(&mut self) -> core::result::Result<(), u32> {
            CTL.with(|c| {
                let mut c = c.borrow_mut();
                c.booted = true;
                c.events.push("boot");
                dev::write32(c.regs.add(0x1300), BOOT_STATUS_OK);
            });
            Ok(())
        }
        fn service(&mut self) -> core::result::Result<(), u32> {
            self.services += 1;
            if self.crashed { Err(7) } else { Ok(()) }
        }
        fn sleep(&mut self) -> core::result::Result<(), u32> {
            CTL.with(|c| c.borrow_mut().events.push("sleep"));
            Ok(())
        }
        fn crashlog(&self, out: &mut dyn fmt::Write) -> fmt::Result {
            out.write_str("none")
        }
    }

    struct Mem {
        _regs: Vec<u64>,
        _mmu: Vec<u64>,
        _dma: Vec<u64>,
        cfg: Config,
    }

    fn machine(secure_bar: bool, gpt: bool) -> Mem {
        let mut regs = vec![0u64; MMIO_LEN as usize / 8 + 1];
        let mut mmu = vec![0u64; NVMMU_LEN as usize / 8 + 1];
        let mut dma = vec![0u64; ((DMA_NEED + DMA_ALIGN) / 8) as usize];
        let r = Pa(regs.as_mut_ptr() as usize as u64);
        let m = Pa(mmu.as_mut_ptr() as usize as u64);
        let d = Pa((dma.as_mut_ptr() as usize as u64).next_multiple_of(DMA_ALIGN));
        dev::write32(r.add(0x14), IBOOT_CC);
        dev::write32(r.add(0x2490c), NOTHING);
        dev::write32(r.add(0x24910), NOTHING);
        dev::write32(m.add(0x28118), NOTHING);
        let mut disk = vec![0u8; (NBLOCKS * BLOCK) as usize];
        if gpt {
            let h = BLOCK as usize;
            disk[h..h + 8].copy_from_slice(b"EFI PART");
            disk[h + 32..h + 40].copy_from_slice(&(NBLOCKS - 1).to_le_bytes());
            disk[h + 40..h + 48].copy_from_slice(&USABLE.0.to_le_bytes());
            disk[h + 48..h + 56].copy_from_slice(&USABLE.1.to_le_bytes());
        }
        CTL.with(|c| {
            *c.borrow_mut() = Ctl {
                regs: r,
                mmu: m,
                disk,
                ..Ctl::default()
            };
        });
        Mem {
            _regs: regs,
            _mmu: mmu,
            _dma: dma,
            cfg: Config {
                nvme: r,
                nvmmu: m,
                dma: d,
                dma_size: DMA_NEED,
                secure_bar,
            },
        }
    }

    fn ans(m: &Mem) -> Ans<Cop> {
        let cop = Cop {
            services: 0,
            crashed: false,
        };
        // SAFETY: vensters en DMA liggen in `m`, dat de test overleeft.
        unsafe { Ans::new(m.cfg, cop, clock) }.unwrap()
    }

    fn up(m: &Mem) -> Ans<Cop> {
        let mut a = ans(m);
        a.start().unwrap();
        a
    }

    /// De ANS zoals hopfs hem ziet: het blokcontract met een `Pace` die
    /// pollt.
    fn blk(a: &mut Ans<Cop>) -> Paced<&mut Ans<Cop>, Spin> {
        Paced::new(a, Spin)
    }

    fn with<R>(f: impl FnOnce(&mut Ctl) -> R) -> R {
        CTL.with(|c| f(&mut c.borrow_mut()))
    }

    /// Schrijft op schijfblok `block` over het I/O-pad en wacht erop.
    fn write_abs(a: &mut Ans<Cop>, block: u64, data: &[u8]) -> Result<(), u32> {
        a.start_write(block, data, 0)?;
        loop {
            if let core::task::Poll::Ready(r) = a.poll_demand(&mut []) {
                return r;
            }
        }
    }

    /// Bevestigt de vastgehouden opdracht op `tag`.
    fn release(tag: u16) {
        with(|c| {
            let i = c.held.iter().position(|h| h.0 == tag).unwrap();
            let (cid, sc, opc) = c.held.remove(i);
            complete(c, 1, cid, sc, opc);
        });
    }

    /// Pollt de opdracht van de eigenaar een paar keer (de klok loopt mee).
    fn poll(a: &mut Ans<Cop>, into: &mut [u8]) -> core::task::Poll<blkdev::Result> {
        let mut r = blkdev::AsyncBlockDevice::poll_done(a, into);
        for _ in 0..3 {
            if r.is_ready() {
                break;
            }
            r = blkdev::AsyncBlockDevice::poll_done(a, into);
        }
        r
    }

    #[test]
    fn start_wakes_first_and_only_clears_shn_and_sets_en() {
        let m = machine(false, true);
        let a = up(&m);
        // Les 2: iBoots entry-maten staan er nog; alleen SHN weg, EN aan.
        let cc = dev::read32(m.cfg.nvme.add(0x14));
        assert_eq!(cc, (IBOOT_CC & !CC_SHN_MASK) | CC_EN);
        assert_eq!(cc, 0x0047_0001);
        // Les 1: het wekbericht kwam vóór alles.
        with(|c| assert_eq!(c.events.first(), Some(&"boot")));
        assert_eq!(a.model(), "APPLE SSD AP0512Z");
        assert_eq!(a.blocks(), NBLOCKS);
        assert_eq!(a.block_size(), 4096);
        assert_eq!(a.max_transfer(), MAX_TRANSFER);
        assert!(a.cop.services > 0, "the mailbox was drained while waiting");
        // De lineaire modus en de NVMMU.
        let r = m.cfg.nvme;
        assert_eq!(dev::read32(r.add(0x24908)) & 1, 1);
        assert_eq!(dev::read32(m.cfg.nvmmu.add(0x28100)), 63);
        assert_eq!(lo_hi(m.cfg.nvmmu.add(0x28108)), m.cfg.dma.0);
        assert_eq!(lo_hi(m.cfg.nvmmu.add(0x28110)), m.cfg.dma.0 + IO_TCB_OFF);
    }

    #[test]
    fn a_fresh_cc_would_never_become_ready() {
        // De nep-controller wil IOSQES = 7; wie CC overschrijft met eigen
        // maten (6) komt niet ready. Dit bewijst dat de nep-chip les 2 kent.
        let m = machine(false, true);
        dev::write32(m.cfg.nvme.add(0x14), 6 << 16);
        let mut a = ans(&m);
        assert!(matches!(a.start(), Err(Error::NotReady { want: true, .. })));
    }

    #[test]
    fn secure_bar_writes_the_io_queue_bases_again() {
        let m = machine(true, true);
        let _a = up(&m);
        let r = m.cfg.nvme;
        assert_eq!(lo_hi(r.add(0x1200)), m.cfg.dma.0 + IOSQ_OFF);
        assert_eq!(lo_hi(r.add(0x1208)), m.cfg.dma.0 + IOCQ_OFF);
        let m = machine(false, true);
        let _a = up(&m);
        assert_eq!(lo_hi(m.cfg.nvme.add(0x1200)), 0);
    }

    #[test]
    fn command_sits_on_the_slot_the_doorbell_names() {
        let m = machine(false, true);
        let mut a = up(&m);
        let mut buf = vec![0u8; 3 * BLOCK as usize];
        a.read_at(7, &mut buf).unwrap();
        with(|c| {
            // Elke opdracht las de firmware van het slot dat de deurbel
            // noemde (met iBoots afstand van 128 bytes), en de TCB van dat
            // slot hoorde erbij.
            assert_eq!(c.tcb_mismatch, 0);
            assert!(
                c.log
                    .iter()
                    .all(|&(_, _, _, _, slot)| slot == u32::from(SLOT))
            );
            // Na elke completion is het slot ongeldig verklaard.
            assert_eq!(c.invalidated.len(), c.log.len());
            assert_eq!(c.log.last(), Some(&(1, IO_READ, 7, 2, 0)));
        });
        assert_eq!(a.commands as usize, with(|c| c.log.len()));
    }

    #[test]
    fn read_anywhere_but_write_only_inside_the_window() {
        let m = machine(false, true);
        let mut a = up(&m);
        // Lezen mag overal: de GPT op blok 1.
        let mut b = vec![0u8; BLOCK as usize];
        a.read_at(1, &mut b).unwrap();
        assert_eq!(&b[..8], b"EFI PART");

        // Zonder venster: luid geweigerd, niets naar de controller.
        let before = with(|c| c.log.len());
        let data = vec![0xaau8; BLOCK as usize];
        assert_eq!(
            write_abs(&mut a, 20, &data),
            Err(Error::NoWindow {
                block: 20,
                len: 4096
            })
        );
        assert!(
            write_abs(&mut a, 20, &data)
                .unwrap_err()
                .to_string()
                .contains("WRITE REFUSED")
        );
        a.set_window(10, 5).unwrap();
        for (blk, len) in [(9, 1), (14, 2), (15, 1), (0, 1), (1, 1)] {
            let d = vec![0u8; len * BLOCK as usize];
            assert!(
                matches!(write_abs(&mut a, blk, &d), Err(Error::OutsideWindow { .. })),
                "block {blk}+{len}"
            );
        }
        assert_eq!(with(|c| c.log.len()), before, "nothing reached the disk");
        with(|c| assert!(c.disk.iter().skip(2 * BLOCK as usize).all(|&x| x == 0)));

        // Binnen het venster wel, over het schrijfpad (opcode 0x01,
        // richting naar het device).
        write_abs(&mut a, 13, &vec![0x5a; 2 * BLOCK as usize]).unwrap();
        with(|c| {
            assert!(c.disk[13 * 4096..15 * 4096].iter().all(|&x| x == 0x5a));
            assert_eq!(c.log.last().map(|l| l.1), Some(IO_WRITE));
            assert_eq!(c.dirs.last(), Some(&TCB_TO_DEVICE));
        });
        a.read_at(13, &mut b).unwrap();
        assert!(b.iter().all(|&x| x == 0x5a));
        with(|c| assert_eq!(c.dirs.last(), Some(&TCB_FROM_DEVICE)));

        a.clear_window();
        assert!(matches!(
            write_abs(&mut a, 13, &data),
            Err(Error::NoWindow { .. })
        ));
    }

    #[test]
    fn window_must_stay_inside_the_usable_gpt_range() {
        let m = machine(false, true);
        let mut a = up(&m);
        for (first, n) in [(0, 4), (5, 2), (USABLE.1, 2), (10, 0), (u64::MAX, 2)] {
            assert!(
                matches!(a.set_window(first, n), Err(Error::BadWindow { .. })),
                "{first}+{n}"
            );
        }
        assert_eq!(a.window(), None);
        a.set_window(USABLE.0, USABLE.1 - USABLE.0 + 1).unwrap();
        assert_eq!(a.window(), Some((6, 53)));
    }

    #[test]
    fn contract_lbas_translate_to_window_blocks() {
        let m = machine(false, true);
        let mut a = up(&m);
        let mut buf = vec![0u8; BLOCK as usize];
        // Zonder venster ziet het contract geen schijf.
        assert_eq!(a.sectors(), 0);
        assert!(matches!(
            block_on(blk(&mut a).read(0, &mut buf)),
            Err(blkdev::Error::OutOfRange { .. })
        ));
        a.set_window(10, 20).unwrap();
        assert_eq!(a.sectors(), 160);
        // LBA 16 (512 bytes) = blok 2 van het venster = schijfblok 12.
        block_on(blk(&mut a).write(16, &vec![0x11; 2 * BLOCK as usize])).unwrap();
        with(|c| {
            assert_eq!(c.log.last().map(|l| (l.2, l.3)), Some((12, 1)));
            assert!(c.disk[12 * 4096..14 * 4096].iter().all(|&x| x == 0x11));
            assert!(c.disk[11 * 4096..12 * 4096].iter().all(|&x| x == 0));
        });
        block_on(blk(&mut a).read(8, &mut buf)).unwrap(); // Schijfblok 11.
        assert!(buf.iter().all(|&x| x == 0));
        block_on(blk(&mut a).read(24, &mut buf)).unwrap(); // Schijfblok 13.
        assert!(buf.iter().all(|&x| x == 0x11));
        // Niet op een blok, geen blokveelvoud, voorbij het venster.
        let oor = |r: blkdev::Result| matches!(r, Err(blkdev::Error::OutOfRange { .. }));
        assert!(oor(block_on(blk(&mut a).read(3, &mut buf))));
        assert!(oor(block_on(blk(&mut a).read(8, &mut buf[..512]))));
        assert!(oor(blkdev::AsyncBlockDevice::start(
            &mut a,
            blkdev::Op::Write { lba: 0, data: &[] }
        )));
        assert!(oor(block_on(
            blk(&mut a).write(152, &vec![0; 2 * BLOCK as usize])
        )));
        assert!(!oor(block_on(
            blk(&mut a).write(152, &vec![0; BLOCK as usize])
        )));
        with(|c| assert_eq!(c.log.last().map(|l| l.2), Some(29)));
        block_on(blk(&mut a).flush()).unwrap();
        with(|c| assert_eq!(c.events.last(), Some(&"flush")));
    }

    #[test]
    fn no_gpt_keeps_the_disk_closed() {
        let m = machine(false, false);
        let mut a = ans(&m);
        assert_eq!(a.start(), Err(Error::NoGpt));
        assert_eq!(a.blocks(), 0);
        let mut b = vec![0u8; BLOCK as usize];
        assert_eq!(a.read_at(1, &mut b), Err(Error::NotStarted));
    }

    #[test]
    fn shutdown_deletes_queues_then_shn_then_sleeps() {
        let m = machine(false, true);
        let mut a = up(&m);
        a.shutdown().unwrap();
        with(|c| {
            let admin: Vec<u8> = c.log.iter().filter(|l| l.0 == 0).map(|l| l.1).collect();
            assert_eq!(admin[admin.len() - 2..], [ADM_DELETE_SQ, ADM_DELETE_CQ]);
            assert_eq!(c.events.last(), Some(&"sleep"));
        });
        let cc = dev::read32(m.cfg.nvme.add(0x14));
        assert_eq!(cc & CC_EN, 0);
        assert_eq!(cc & CC_SHN_MASK, CC_SHN_NORMAL);
        assert_eq!(cc & !(CC_SHN_MASK | CC_EN), IBOOT_CC & !CC_SHN_MASK);
        let mut b = vec![0u8; BLOCK as usize];
        assert_eq!(a.read_at(1, &mut b), Err(Error::NotStarted));
    }

    #[test]
    fn a_silent_controller_kills_the_driver_but_shutdown_still_sleeps() {
        let m = machine(false, true);
        let mut a = up(&m);
        with(|c| c.mute = true);
        let mut b = vec![0u8; BLOCK as usize];
        assert_eq!(a.read_at(1, &mut b), Err(Error::Timeout { opc: IO_READ }));
        assert_eq!(a.read_at(1, &mut b), Err(Error::Dead));
        // Geen queue-opdrachten meer naar een controller die niet antwoordt,
        // maar CC.SHN en de slaap wel.
        let before = with(|c| c.log.len());
        a.shutdown().unwrap();
        with(|c| {
            assert_eq!(c.log.len(), before);
            assert_eq!(c.events.last(), Some(&"sleep"));
        });
        assert_eq!(dev::read32(m.cfg.nvme.add(0x14)) & CC_EN, 0);
    }

    #[test]
    fn a_crashed_coprocessor_is_fatal() {
        let m = machine(false, true);
        let mut a = up(&m);
        a.coprocessor().crashed = true;
        let mut b = vec![0u8; BLOCK as usize];
        assert_eq!(a.read_at(1, &mut b), Err(Error::Coprocessor(7)));
        assert_eq!(a.read_at(1, &mut b), Err(Error::Dead));
        let d = a.diag().to_string();
        assert!(
            d.contains("boot=0xde71ce55") && d.ends_with("crashlog=none"),
            "{d}"
        );
    }

    #[test]
    fn dma_region_is_checked() {
        let m = machine(false, true);
        let cop = Cop {
            services: 0,
            crashed: false,
        };
        let mut cfg = m.cfg;
        cfg.dma = cfg.dma.add(0x1000);
        // SAFETY: wordt geweigerd vóór één toegang.
        let r = unsafe { Ans::new(cfg, cop, clock) };
        assert!(matches!(r, Err(Error::Dma { .. })));
        let e: Error<u32> = Error::Dma { base: 1, size: 2 };
        let s: String = e.to_string();
        assert!(s.contains("0x1"));
    }

    use blkdev::Op;
    use core::task::Poll;

    /// Een hap van 16 KB (MDTS 2) en een venster van 40 blokken vanaf het
    /// begin van het bruikbare deel: zo past een reeks happen op de
    /// nepschijf. Elk blok draagt zijn eigen nummer.
    fn small_steps(hold: bool) -> (Mem, Ans<Cop>) {
        let m = machine(false, true);
        with(|c| {
            c.mdts = 2;
            for (i, b) in c.disk.chunks_mut(BLOCK as usize).enumerate().skip(2) {
                b.fill(i as u8);
            }
        });
        let mut a = up(&m);
        a.set_window(USABLE.0, 40).unwrap();
        with(|c| {
            c.hold = hold;
            c.acked.clear();
        });
        (m, a)
    }

    /// Het blokcontract (de inherente `start` is de opstart).
    fn op(a: &mut Ans<Cop>, o: Op<'_>) -> blkdev::Result {
        blkdev::AsyncBlockDevice::start(a, o)
    }

    const HAP: usize = 4 * BLOCK as usize;
    const LBAS: u64 = HAP as u64 / SECTOR;

    /// Draagt `b` de blokken vanaf schijfblok `first`?
    fn is_hap(b: &[u8], first: u64) -> bool {
        b.chunks(BLOCK as usize)
            .zip(first..)
            .all(|(c, n)| c.iter().all(|&x| x == n as u8))
    }

    /// Leest één hap op `lba` met de firmware vastgehouden: geeft de
    /// opdracht vrij op tag `tag` en wacht.
    fn read_held(a: &mut Ans<Cop>, lba: u64, tag: u16, b: &mut [u8]) {
        op(a, Op::Read { lba, len: HAP }).unwrap();
        assert!(
            poll(a, b).is_pending(),
            "nothing back before the completion"
        );
        release(tag);
        assert_eq!(poll(a, b), Poll::Ready(Ok(())));
    }

    #[test]
    fn a_write_comes_back_only_after_its_completion_and_a_flush_after_it() {
        let (_m, mut a) = small_steps(true);
        let data = vec![0x77u8; BLOCK as usize];
        op(
            &mut a,
            Op::Write {
                lba: 8,
                data: &data,
            },
        )
        .unwrap();
        // De firmware deed de DMA al, maar bevestigde nog niet: de schrijf
        // komt niet terug, hoe vaak de wachter ook kijkt.
        let at = (USABLE.0 + 1) as usize * BLOCK as usize;
        with(|c| assert!(c.disk[at..at + 4096].iter().all(|&x| x == 0x77)));
        assert!(poll(&mut a, &mut []).is_pending());
        // De flush kan er niet tussendoor: één opdracht van de eigenaar.
        assert_eq!(op(&mut a, Op::Flush), Err(blkdev::Error::Busy));
        release(0);
        assert_eq!(poll(&mut a, &mut []), Poll::Ready(Ok(())));
        // Nu de flush, en ook die alleen met zijn eigen completion.
        op(&mut a, Op::Flush).unwrap();
        assert!(poll(&mut a, &mut []).is_pending());
        release(0);
        assert_eq!(poll(&mut a, &mut []), Poll::Ready(Ok(())));
        with(|c| {
            assert_eq!(c.acked, vec![(IO_WRITE, 0), (IO_FLUSH, 0)]);
            assert_eq!(c.events.last(), Some(&"flush"));
        });
    }

    #[test]
    fn sequential_reads_keep_one_read_ahead_in_flight_and_a_flush_waits_for_it() {
        let (_m, mut a) = small_steps(true);
        let mut b = vec![0u8; HAP];
        read_held(&mut a, 0, 0, &mut b);
        assert!(is_hap(&b, USABLE.0));
        read_held(&mut a, LBAS, 0, &mut b);
        // Twee happen op rij: de derde staat al op de controller, op tag 1.
        with(|c| {
            assert_eq!(c.held.iter().map(|h| h.0).collect::<Vec<_>>(), [1]);
            assert_eq!(c.log.last(), Some(&(1, IO_READ, USABLE.0 + 8, 3, 1)));
        });
        // Een flush is pas klaar als er niets meer in de lucht is.
        op(&mut a, Op::Flush).unwrap();
        release(0);
        assert!(poll(&mut a, &mut []).is_pending());
        release(1);
        assert_eq!(poll(&mut a, &mut []), Poll::Ready(Ok(())));
        // De derde hap komt van de read-ahead: geen nieuwe opdracht.
        let before = with(|c| c.log.len());
        op(
            &mut a,
            Op::Read {
                lba: 2 * LBAS,
                len: HAP,
            },
        )
        .unwrap();
        assert_eq!(poll(&mut a, &mut b), Poll::Ready(Ok(())));
        assert!(is_hap(&b, USABLE.0 + 8));
        assert_eq!(a.ahead_hits, 1);
        // En de vierde staat alweer op de controller.
        with(|c| {
            assert_eq!(c.log.len(), before + 1);
            assert_eq!(c.log.last().map(|l| l.2), Some(USABLE.0 + 12));
        });
    }

    #[test]
    fn a_write_over_the_read_ahead_makes_it_stale() {
        let (_m, mut a) = small_steps(true);
        let mut b = vec![0u8; HAP];
        read_held(&mut a, 0, 0, &mut b);
        read_held(&mut a, LBAS, 0, &mut b);
        // De read-ahead op tag 1 loopt nog; een schrijf over zijn blokken
        // gaat ernaast op tag 0, en komt eerder terug dan hij.
        let data = vec![0x5au8; BLOCK as usize];
        op(
            &mut a,
            Op::Write {
                lba: 2 * LBAS,
                data: &data,
            },
        )
        .unwrap();
        release(0);
        assert_eq!(poll(&mut a, &mut []), Poll::Ready(Ok(())));
        release(1);
        // De lees daarna ziet de schrijf, niet de oude bytes van de read-ahead.
        read_held(&mut a, 2 * LBAS, 0, &mut b);
        assert!(b[..4096].iter().all(|&x| x == 0x5a));
        assert!(is_hap(&b[4096..], USABLE.0 + 9));
        assert_eq!((a.ahead_hits, a.ahead_waste), (0, 1));
        with(|c| {
            assert_eq!(
                c.acked.iter().map(|x| x.1).collect::<Vec<_>>(),
                [0, 0, 0, 1, 0]
            );
            assert_eq!(c.tcb_mismatch, 0);
        });
    }

    #[test]
    fn the_second_tag_sits_at_the_linear_entry_stride() {
        // De nep-controller leest per 64 bytes zoals de firmware (M26), en
        // niet op CC.IOSQES; de read-ahead op tag 1 komt met de juiste bytes.
        let (_m, mut a) = small_steps(false);
        let mut b = vec![0u8; HAP];
        for k in 0..5u64 {
            block_on(blk(&mut a).read(k * LBAS, &mut b)).unwrap();
            assert!(is_hap(&b, USABLE.0 + 4 * k));
        }
        assert_eq!(a.ahead_hits, 3);
        with(|c| {
            assert_eq!(c.tcb_mismatch, 0);
            assert!(c.log.iter().any(|l| l.4 == 1));
        });
    }

    #[test]
    fn opcodes_and_layout() {
        assert_eq!((IO_FLUSH, IO_WRITE, IO_READ), (0x00, 0x01, 0x02));
        assert_eq!((ADM_DELETE_SQ, ADM_DELETE_CQ), (0x00, 0x04));
        assert_eq!(
            (ADM_CREATE_SQ, ADM_CREATE_CQ, ADM_IDENTIFY),
            (0x01, 0x05, 0x06)
        );
        assert_eq!(PER_BLOCK, 8);
        assert_eq!(DMA_NEED, 0x40_0000);
    }
}
