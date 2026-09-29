//! De bestandscalls van de system-API en de hopfs-actor (Go:
//! `OLD/metal/kern/slots/rpc.go`).
//!
//! Paden van een app worden hier tegen de volume-tabel van DIE levensduur
//! geresolved: dat is de toegangsgrens op opslag. Zichtbaar is de eigen,
//! lege root (`/.tasks/slot<N>`, schoon bij elke start) plus uitsluitend de
//! expliciet gemounte gedeelde mappen ([`crate::slots::Mount`], `local ->
//! shared`). Een `..` is geen pad maar een weigering ([`Error::Denied`]).
//!
//! # Eigendom
//!
//! De [`FsActor`] is de enige eigenaar van de [`Fs`] (PORT.md §3, de rij
//! `hopfs.FS.mu`): één taak met een [`Mailbox`], en de boom is zijn
//! `&mut self`. Een verbindingstaak stuurt een [`FsCall`] en VERPLAATST
//! daarin haar eigen call- en antwoordbuffer (handboek §1.2); de actor leest
//! de data uit de ene, schrijft het antwoord in de andere en geeft beide
//! terug via de [`Reply`] van de verbinding. Er leeft dus geen lening over
//! een `.await`, en er wordt per call niets gealloceerd (de kern-heap is een
//! bump-allocator: wat terugkomt uit het midden lekt).
//!
//! De I/O blijft voorlopig in de actor, synchroon, zoals in Go: PORT.md §3
//! splitst later in een metadata-actor en een blok-actor, waarbij een
//! servicer zijn extents vraagt ([`Fs::lookup`]) en zijn eigen I/O doet.
//!
//! # De kern als lezer
//!
//! De kern leest zelf ook van het volume, zonder slot en zonder generatie
//! ([`FsMsg::KernRead`], [`kern_read`], [`read_file`]): de firmware van de
//! videocodec (`/firmware/<naam>.fwb`, zestien blobs van zo'n 300 KB) en de
//! teststream van het meetinstrument. Dezelfde actor, dezelfde brievenbus:
//! de lezing is een bericht als elk ander en loopt dus nooit door een
//! schrijf van een app heen. Het pad is een hopfs-pad, geen app-pad; de
//! eigen roots van de taken ([`TASKS_DIR`]) zijn ook voor de kern dicht,
//! want er is geen kern-lezing die daar iets te zoeken heeft.
//!
//! # Vastleggen
//!
//! [`committer`] legt de boom vast elke [`COMMIT_EVERY`] (Go:
//! `fsCommitEvery`) en meteen na de stop van een slot, zodat wat een app
//! schreef een stroomuitval overleeft. De data zelf staat er al; de commit is
//! de boom die haar terugvindt.

use crate::cage::{Console, Timer};
use crate::hopfs::{BlockDevice, Fs};
use crate::slots::{Mount, Reply, Servicers, try_push};
use crate::system::{MAX_IO_CHUNK, REQ_HEADER};
use crate::{Error, Result, SLOT_CAP, Slot};
use abi::hopabi::{OP_LIST, OP_READ, OP_REMOVE, OP_STAT, OP_TRUNCATE, OP_WRITE};
use alloc::vec::Vec;
use core::ops::Range;
use core::time::Duration;
use sync::mpsc::Mailbox;

/// De map onder hopfs waar de eigen roots van de taken wonen.
pub const TASKS_DIR: &[u8] = b"/.tasks";
/// Het langste pad na resolutie. De ABI laat 64 KiB toe (`path_len u16`);
/// een pad van meer dan een kilobyte is een fout van de app, en zo past de
/// resolutie in twee vaste buffers op de stack van de actor.
pub const MAX_PATH: usize = 1024;
/// Hoeveel volumes één levensduur hoogstens draagt (de flip-grens van
/// `slots::MAX_FLIP_MOUNTS`).
pub const MAX_MOUNTS: usize = crate::slots::MAX_FLIP_MOUNTS;
/// De diepte van de brievenbus van de actor: elke verbindingstaak heeft
/// hoogstens één call tegelijk uitstaan, plus de committer.
pub const FS_DEPTH: usize = 16;
/// Hoe vaak de boom vastgelegd wordt als hij veranderde (Go:
/// `fsCommitEvery`): de grens van wat een harde stroomuitval kost aan namen
/// en groottes.
pub const COMMIT_EVERY: Duration = Duration::from_secs(10);
/// Het ritme waarop de committer de servicer-tabel naloopt voor een stop.
pub const COMMIT_POLL: Duration = Duration::from_secs(1);

/// Is `op` een bestandscall die deze module bedient?
#[must_use]
pub const fn is_fs_op(op: u8) -> bool {
    matches!(
        op,
        OP_STAT | OP_READ | OP_WRITE | OP_LIST | OP_REMOVE | OP_TRUNCATE
    )
}

// ---------------------------------------------------------------------------
// Paden: normaliseren, de volume-tabel en de resolutie.
// ---------------------------------------------------------------------------

/// Een pad in een vaste buffer: de resolutie alloceert niets.
pub struct PathBuf {
    b: [u8; MAX_PATH],
    n: usize,
}

impl PathBuf {
    /// Een leeg pad.
    #[must_use]
    pub const fn new() -> PathBuf {
        PathBuf {
            b: [0; MAX_PATH],
            n: 0,
        }
    }

    /// De bytes.
    #[must_use]
    pub fn as_bytes(&self) -> &[u8] {
        self.b.get(..self.n).unwrap_or(&[])
    }

    fn clear(&mut self) {
        self.n = 0;
    }

    fn push(&mut self, s: &[u8]) -> Result {
        let end = self.n + s.len();
        let d = self.b.get_mut(self.n..end).ok_or(Error::TooLarge {
            len: end,
            max: MAX_PATH,
        })?;
        d.copy_from_slice(s);
        self.n = end;
        Ok(())
    }

    fn set(&mut self, s: &[u8]) -> Result {
        self.clear();
        self.push(s)
    }
}

impl Default for PathBuf {
    fn default() -> Self {
        PathBuf::new()
    }
}

/// Normaliseert een app-pad naar de vorm `/a/b` in `out` (Go: `cleanAbs`):
/// lege en `.`-segmenten vallen weg, `..` is een weigering (de app heeft
/// buiten zijn zicht niets te zoeken). Een leeg pad is `/`.
pub fn clean_abs(p: &[u8], out: &mut PathBuf) -> Result {
    out.clear();
    for seg in p.split(|b| *b == b'/') {
        match seg {
            b"" | b"." => {}
            b".." => return Err(Error::Denied),
            _ => {
                out.push(b"/")?;
                out.push(seg)?;
            }
        }
    }
    if out.n == 0 {
        out.push(b"/")?;
    }
    Ok(())
}

/// Ligt `p` (genormaliseerd) in of op `dir`?
fn under(p: &[u8], dir: &[u8]) -> bool {
    p == dir || (p.starts_with(dir) && p.get(dir.len()) == Some(&b'/'))
}

/// Normaliseert de volumes van een start tot een tabel, langste `local`
/// eerst (voor de prefix-resolutie). Go: `mountTable`, plus twee grenzen die
/// daar ontbraken: een volume is een gedeelde map, dus nooit `/` en nooit
/// iets onder [`TASKS_DIR`] (dan zag een app de root van een andere).
pub fn mount_table(mounts: &[Mount]) -> Result<Vec<Mount>> {
    if mounts.len() > MAX_MOUNTS {
        return Err(Error::Full { cap: MAX_MOUNTS });
    }
    let mut t: Vec<Mount> = Vec::new();
    t.try_reserve_exact(mounts.len())
        .map_err(|_| Error::OutOfMemory {
            bytes: core::mem::size_of_val(mounts),
        })?;
    let mut buf = PathBuf::new();
    for m in mounts {
        clean_abs(&m.local, &mut buf)?;
        let local = crate::slots::try_vec(buf.as_bytes())?;
        clean_abs(&m.shared, &mut buf)?;
        let shared = crate::slots::try_vec(buf.as_bytes())?;
        // De taak houdt haar eigen root; `/` overmounten zou hem verbergen.
        if local == b"/" || shared == b"/" || under(&shared, TASKS_DIR) {
            return Err(Error::Denied);
        }
        if t.iter().any(|x| x.local == local) {
            return Err(Error::BadPath); // Dubbel local-pad.
        }
        try_push(&mut t, Mount { local, shared })?;
    }
    // In-place en zonder allocatie: `sort_by` zou een hulpbuffer vragen.
    t.sort_unstable_by(|a, b| b.local.len().cmp(&a.local.len()));
    Ok(t)
}

/// Schrijft de eigen root van `slot` (`/.tasks/slot<N>`) in `out`.
pub fn push_root(slot: Slot, out: &mut PathBuf) -> Result {
    out.push(TASKS_DIR)?;
    out.push(b"/slot")?;
    let mut digits = [0u8; 3];
    let mut n = slot.get();
    let mut i = digits.len();
    loop {
        i -= 1;
        if let Some(d) = digits.get_mut(i) {
            *d = b'0' + (n % 10) as u8;
        }
        n /= 10;
        if n == 0 || i == 0 {
            break;
        }
    }
    out.push(digits.get(i..).unwrap_or(&[]))
}

/// Vertaalt een app-pad naar een hopfs-pad in `out` (Go: `resolve`): een
/// gemount prefix wordt de gedeelde map, de rest valt in de eigen root van
/// het slot. Geeft de index van het volume, of `None` voor de eigen root.
pub fn resolve(slot: Slot, mounts: &[Mount], p: &[u8], out: &mut PathBuf) -> Result<Option<usize>> {
    let mut cp = PathBuf::new();
    clean_abs(p, &mut cp)?;
    let cp = cp.as_bytes();
    for (i, m) in mounts.iter().enumerate() {
        if under(cp, &m.local) {
            out.set(&m.shared)?;
            out.push(cp.get(m.local.len()..).unwrap_or(&[]))?;
            return Ok(Some(i));
        }
    }
    out.clear();
    push_root(slot, out)?;
    if cp != b"/" {
        out.push(cp)?;
    }
    Ok(None)
}

// ---------------------------------------------------------------------------
// De berichten.
// ---------------------------------------------------------------------------

/// Eén bestandscall, met de buffers van de verbinding als waarde.
///
/// `buf` is de callbuffer (pad en data staan erin op `path` en `data`),
/// `out` de antwoordbuffer: de data van een antwoord komt op
/// `out[REQ_HEADER..]`, de kop schrijft de verbinding zelf.
#[derive(Debug)]
pub struct FsCall {
    /// Het slot van de peer.
    pub slot: Slot,
    /// De levensduur waarvoor de verbinding toegelaten werd.
    pub generation: u32,
    /// De operatie (`abi::hopabi::OP_*`).
    pub op: u8,
    /// De offset.
    pub off: u64,
    /// De lengte (read) of de maat (truncate).
    pub n: u64,
    /// Waar het pad in `buf` staat.
    pub path: Range<usize>,
    /// Waar de data in `buf` staat.
    pub data: Range<usize>,
    /// De callbuffer.
    pub buf: Vec<u8>,
    /// De antwoordbuffer.
    pub out: Vec<u8>,
}

/// Het antwoord van de actor: de buffers terug, en de uitkomst als
/// `(size, data_len)`.
#[derive(Debug)]
pub struct FsDone {
    /// De callbuffer, terug.
    pub buf: Vec<u8>,
    /// De antwoordbuffer, terug; de data staat op `out[REQ_HEADER..]`.
    pub out: Vec<u8>,
    /// Het `size`-veld van het antwoord en het aantal databytes.
    pub result: Result<(u64, usize)>,
}

/// Waarom de boom vastgelegd wordt, voor de ene consoleregel.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum CommitWhy {
    /// De periodieke commit ([`COMMIT_EVERY`]).
    Periodic,
    /// Een slot stopte.
    Stopped(Slot),
    /// De kern-flip: de laatste commit van deze kern, vlak vóór de sprong.
    Flip,
}

/// Een bericht aan de actor.
#[derive(Debug)]
pub enum FsMsg {
    /// Een bestandscall; het antwoord gaat naar de [`Reply`].
    Call(FsCall),
    /// Leg de boom vast als hij veranderde.
    Commit(CommitWhy),
    /// De kern-flip: leg de boom vast en neem daarna geen call meer aan
    /// (elke call krijgt [`Error::Busy`], luid) tot [`FsMsg::Thaw`]. Het
    /// antwoord draagt de vastgelegde generatie als `size`.
    Freeze,
    /// De flip ging niet door: de actor neemt weer calls aan.
    Thaw,
    /// Een lezing voor de kern zelf ([`kern_read`]); het antwoord is een
    /// [`FsDone`] met het pad in `buf`, de data vooraan in `out` en als
    /// uitkomst `(bestandsmaat, gelezen bytes)`.
    KernRead(KernRead),
}

/// Een lezing voor de kern zelf: een hopfs-pad, zonder slot en generatie.
/// De buffers gaan als waarde heen en terug (handboek §1.2), zodat een
/// aanroeper die in stukken leest (de teststream) nooit per stuk alloceert:
/// de kern-heap is een bump-allocator.
#[derive(Debug)]
pub struct KernRead {
    /// Het absolute hopfs-pad (`/firmware/hevcdec.fwb`).
    pub path: Vec<u8>,
    /// De offset in het bestand.
    pub off: u64,
    /// De bestemming: hooguit `out.len()` bytes, vooraan. Leeg is een stat.
    pub out: Vec<u8>,
}

/// Een bericht met zijn antwoordplek.
pub struct FsEnvelope<'a> {
    msg: FsMsg,
    reply: Option<&'a Reply>,
}

/// De brievenbus van de hopfs-actor.
pub type FsInbox<'a> = Mailbox<FsEnvelope<'a>, FS_DEPTH>;

/// Stuurt een bestandscall naar de actor en wacht op het antwoord. Zit de
/// brievenbus vol, dan komt de call (met de buffers) meteen terug.
pub async fn call<'a>(
    inbox: &FsInbox<'a>,
    reply: &'a Reply,
    c: FsCall,
) -> core::result::Result<FsDone, FsCall> {
    let _ = reply.done.take();
    if let Err(sync::Full(env)) = inbox.try_send(FsEnvelope {
        msg: FsMsg::Call(c),
        reply: Some(reply),
    }) {
        return match env.msg {
            FsMsg::Call(c) => Err(c),
            FsMsg::Commit(_) | FsMsg::Freeze | FsMsg::Thaw | FsMsg::KernRead(_) => Ok(FsDone {
                buf: Vec::new(),
                out: Vec::new(),
                result: Err(Error::Busy),
            }),
        };
    }
    loop {
        reply.done.wait().await;
        if let Some(d) = reply.take_fs() {
            return Ok(d);
        }
    }
}

/// Bevriest de actor voor de kern-flip en geeft de generatie die hij net
/// vastlegde: wat daarna op de schijf staat, is precies wat de nieuwe kern
/// mount. Een volle brievenbus is [`Error::Busy`] (en dan geen flip).
pub async fn freeze<'a>(inbox: &FsInbox<'a>, reply: &'a Reply) -> Result<u64> {
    let _ = reply.done.take();
    if inbox
        .try_send(FsEnvelope {
            msg: FsMsg::Freeze,
            reply: Some(reply),
        })
        .is_err()
    {
        return Err(Error::Busy);
    }
    loop {
        reply.done.wait().await;
        if let Some(d) = reply.take_fs() {
            return d.result.map(|(generation, _)| generation);
        }
    }
}

/// Leest voor de kern zelf: `r.out` vooraan gevuld vanaf `r.off`, of alleen
/// de maat als `r.out` leeg is. Geeft altijd beide buffers terug (`buf` is
/// het pad), ook bij een fout; een volle brievenbus is [`Error::Busy`].
/// Een termijn legt de aanroeper er zelf omheen (een `select` met zijn
/// timer), zoals bij [`freeze`].
pub async fn kern_read<'a>(inbox: &FsInbox<'a>, reply: &'a Reply, r: KernRead) -> FsDone {
    let _ = reply.done.take();
    if let Err(sync::Full(env)) = inbox.try_send(FsEnvelope {
        msg: FsMsg::KernRead(r),
        reply: Some(reply),
    }) {
        let (buf, out) = match env.msg {
            FsMsg::KernRead(r) => (r.path, r.out),
            FsMsg::Call(c) => (c.buf, c.out),
            FsMsg::Commit(_) | FsMsg::Freeze | FsMsg::Thaw => (Vec::new(), Vec::new()),
        };
        return FsDone {
            buf,
            out,
            result: Err(Error::Busy),
        };
    }
    loop {
        reply.done.wait().await;
        if let Some(d) = reply.take_fs() {
            return d;
        }
    }
}

/// Leest een heel bestand voor de kern (de firmware van de codec): eerst de
/// maat, dan precies zoveel bytes in één lezing. Groter dan `max` is
/// [`Error::TooLarge`] vóór er iets gealloceerd wordt; een bestand dat
/// tussen maat en lezing kromp, is [`Error::Corrupt`] in plaats van een
/// blob met een staart van nullen.
pub async fn read_file<'a>(
    inbox: &FsInbox<'a>,
    reply: &'a Reply,
    path: &[u8],
    max: usize,
) -> Result<Vec<u8>> {
    let r = KernRead {
        path: crate::slots::try_vec(path)?,
        off: 0,
        out: Vec::new(),
    };
    let d = kern_read(inbox, reply, r).await;
    let (size, _) = d.result?;
    let len = usize::try_from(size).unwrap_or(usize::MAX);
    if len > max {
        return Err(Error::TooLarge { len, max });
    }
    let mut out: Vec<u8> = Vec::new();
    out.try_reserve_exact(len)
        .map_err(|_| Error::OutOfMemory { bytes: len })?;
    out.resize(len, 0);
    let r = KernRead {
        path: d.buf,
        off: 0,
        out,
    };
    let d = kern_read(inbox, reply, r).await;
    let (_, n) = d.result?;
    if n != len {
        return Err(Error::Corrupt { at: n });
    }
    Ok(d.out)
}

/// Ontdooit de actor na een flip die niet doorging. Vuur-en-vergeet: een
/// volle brievenbus is `false`, en dan zegt de aanroeper het luid.
#[must_use]
pub fn thaw(inbox: &FsInbox<'_>) -> bool {
    inbox
        .try_send(FsEnvelope {
            msg: FsMsg::Thaw,
            reply: None,
        })
        .is_ok()
}

// ---------------------------------------------------------------------------
// De actor.
// ---------------------------------------------------------------------------

/// De eigenaar van hopfs: boom, extents, vrije lijst en (nog) de I/O.
pub struct FsActor<'s, D, L> {
    fs: Fs<D>,
    svc: &'s Servicers,
    log: L,
    /// Per slot de levensduur waarvoor root en volumes klaarstaan.
    prepared: [Option<u32>; SLOT_CAP + 1],
    /// Per slot de levensduur waarvan de eerste schrijf in een volume al
    /// een regel kreeg.
    saved: [Option<u32>; SLOT_CAP + 1],
    /// Mislukte commits: de eerste paar krijgen een regel, daarna tellen we.
    commit_fails: u64,
    /// Blokfouten onder een call; dezelfde regel.
    io_fails: u64,
    /// Bevroren voor de kern-flip ([`FsMsg::Freeze`]): calls worden
    /// geweigerd, commits overgeslagen. De boom op de schijf is dan die
    /// van de nieuwe kern.
    frozen: bool,
    /// Geweigerde calls tijdens de bevriezing.
    frozen_calls: u64,
    path: PathBuf,
}

/// Hoeveel mislukte commits (en blokfouten) een eigen regel krijgen.
const LOUD_COMMIT_FAILS: u64 = 3;

impl<'s, D: BlockDevice, L: Console> FsActor<'s, D, L> {
    /// Een actor over een gemounte `fs`, met de volume-tabellen uit `svc`.
    pub fn new(fs: Fs<D>, svc: &'s Servicers, log: L) -> FsActor<'s, D, L> {
        FsActor {
            fs,
            svc,
            log,
            prepared: [None; SLOT_CAP + 1],
            saved: [None; SLOT_CAP + 1],
            commit_fails: 0,
            io_fails: 0,
            frozen: false,
            frozen_calls: 0,
            path: PathBuf::new(),
        }
    }

    /// De lus: één bericht tegelijk, elk antwoord terug naar zijn plek.
    pub async fn run(&mut self, inbox: &FsInbox<'_>) {
        loop {
            let env = inbox.recv().await;
            match env.msg {
                FsMsg::Call(mut c) if self.frozen => {
                    // Luid, de eerste paar keer: een call die hier strandt
                    // hoort de aanroeper opnieuw te doen op de nieuwe kern.
                    self.frozen_calls += 1;
                    if self.frozen_calls <= LOUD_COMMIT_FAILS {
                        self.log.log(format_args!(
                            "hopfs: slot {} op {} refused, frozen for the kernel flip HOPOS_FS_FROZEN_CALL",
                            c.slot, c.op
                        ));
                    }
                    if let Some(reply) = env.reply {
                        reply.put_fs(FsDone {
                            buf: core::mem::take(&mut c.buf),
                            out: core::mem::take(&mut c.out),
                            result: Err(Error::Busy),
                        });
                    }
                }
                FsMsg::Call(mut c) => {
                    let result = self.handle(&mut c);
                    if let Err(Error::Io { lba }) = result {
                        // Een blokfout is de schijf, niet de app: luid, de
                        // eerste paar keer (de driver zelf print niet).
                        self.io_fails += 1;
                        if self.io_fails <= LOUD_COMMIT_FAILS {
                            self.log.log(format_args!(
                                "hopfs: slot {} op {}: block I/O failed at LBA {lba} ({} so far) HOPOS_FS_IO",
                                c.slot, c.op, self.io_fails
                            ));
                        }
                    }
                    if let Some(reply) = env.reply {
                        reply.put_fs(FsDone {
                            buf: c.buf,
                            out: c.out,
                            result,
                        });
                    }
                }
                FsMsg::Commit(_) if self.frozen => {}
                FsMsg::Commit(why) => self.commit(why),
                FsMsg::Freeze => {
                    let result = self.freeze();
                    if let Some(reply) = env.reply {
                        reply.put_fs(FsDone {
                            buf: Vec::new(),
                            out: Vec::new(),
                            result,
                        });
                    }
                }
                FsMsg::KernRead(mut r) => {
                    // Bevroren weigert ook de kern: na de bevriezing hoort de
                    // schijf van de volgende kern, en een lezing die daarna
                    // nog lukt, zou een vergeten afhankelijkheid verstoppen.
                    let result = if self.frozen {
                        Err(Error::Busy)
                    } else {
                        self.kern_read(&r.path, r.off, &mut r.out)
                    };
                    if let Some(reply) = env.reply {
                        reply.put_fs(FsDone {
                            buf: r.path,
                            out: r.out,
                            result,
                        });
                    }
                }
                FsMsg::Thaw => {
                    self.frozen = false;
                    self.log.log(format_args!(
                        "hopfs: thawed, the flip did not go through ({} call(s) were refused) HOPOS_FS_THAWED",
                        self.frozen_calls
                    ));
                    self.frozen_calls = 0;
                }
            }
        }
    }

    /// De bevriezing van de kern-flip: eerst vastleggen, dan pas dicht. Een
    /// commit die faalt bevriest niet: dan zou de nieuwe kern een oudere
    /// boom mounten dan de apps denken, en dat is geen flip maar verlies.
    fn freeze(&mut self) -> Result<(u64, usize)> {
        self.fs.commit()?;
        self.frozen = true;
        self.frozen_calls = 0;
        let g = self.fs.generation();
        self.log.log(format_args!(
            "hopfs: tree committed as generation {g} and frozen for the kernel flip HOPOS_FS_FROZEN generation={g}"
        ));
        Ok((g, 0))
    }

    /// Eén lezing voor de kern ([`FsMsg::KernRead`]): de bestandsmaat en
    /// hoeveel bytes er vooraan in `out` kwamen. Een map is [`Error::Kind`],
    /// een pad onder [`TASKS_DIR`] [`Error::Denied`].
    pub fn kern_read(&mut self, path: &[u8], off: u64, out: &mut [u8]) -> Result<(u64, usize)> {
        clean_abs(path, &mut self.path)?;
        let p = self.path.as_bytes();
        if under(p, TASKS_DIR) {
            return Err(Error::Denied);
        }
        let (size, dir) = self.fs.stat(p)?;
        if dir {
            return Err(Error::Kind);
        }
        if out.is_empty() {
            return Ok((size, 0));
        }
        let n = self.fs.read_at(p, off, out)?;
        Ok((size, n))
    }

    /// De generatie van de laatst vastgelegde boom.
    #[must_use]
    pub fn generation(&self) -> u64 {
        self.fs.generation()
    }

    /// Legt de boom vast als hij veranderde; één regel per nieuwe generatie.
    pub fn commit(&mut self, why: CommitWhy) {
        let before = self.fs.generation();
        match self.fs.commit() {
            Ok(()) if self.fs.generation() != before => {
                let g = self.fs.generation();
                match why {
                    CommitWhy::Periodic => self.log.log(format_args!(
                        "hopfs: tree committed as generation {g} (every {} s) HOPOS_FS_COMMIT",
                        COMMIT_EVERY.as_secs()
                    )),
                    CommitWhy::Stopped(s) => self.log.log(format_args!(
                        "hopfs: tree committed as generation {g} (slot {s} stopped) HOPOS_FS_COMMIT"
                    )),
                    CommitWhy::Flip => self.log.log(format_args!(
                        "hopfs: tree committed as generation {g} (kernel flip) HOPOS_FS_COMMIT"
                    )),
                }
            }
            Ok(()) => {}
            Err(e) => {
                self.commit_fails += 1;
                if self.commit_fails <= LOUD_COMMIT_FAILS {
                    self.log.log(format_args!(
                        "hopfs: commit failed: {e} ({} so far) HOPOS_FS_COMMIT_FAIL",
                        self.commit_fails
                    ));
                }
            }
        }
    }

    /// Zet root en volumes klaar voor een nieuwe levensduur (Go:
    /// `startImage`): de root van de vorige bewoner gaat weg, een verse lege
    /// komt ervoor in de plaats, en de gedeelde mappen bestaan.
    fn prepare(&mut self, slot: Slot, generation: u32) -> Result {
        let i = slot.get();
        if self.prepared.get(i).copied().flatten() == Some(generation) {
            return Ok(());
        }
        self.path.clear();
        push_root(slot, &mut self.path)?;
        match self.fs.remove(self.path.as_bytes(), true) {
            Ok(()) | Err(Error::NoEnt) => {}
            Err(e) => return Err(e),
        }
        self.fs.mkdir_all(self.path.as_bytes())?;
        let (fs, path) = (&mut self.fs, &mut self.path);
        self.svc
            .with_mounts(slot, |mounts| {
                for m in mounts {
                    path.set(&m.shared)?;
                    fs.mkdir_all(path.as_bytes())?;
                }
                Ok::<(), Error>(())
            })
            .ok_or(Error::Denied)??;
        if let Some(p) = self.prepared.get_mut(i) {
            *p = Some(generation);
        }
        Ok(())
    }

    /// Eén bestandscall (Go: `handleWithLimit`, zonder store en codec).
    /// Geeft het `size`-veld van het antwoord en het aantal databytes op
    /// `c.out[REQ_HEADER..]`.
    pub fn handle(&mut self, c: &mut FsCall) -> Result<(u64, usize)> {
        if !is_fs_op(c.op) {
            return Err(Error::Kind);
        }
        // Een verbinding van een vorige levensduur krijgt niets: de
        // generatie moet de levende zijn.
        if self.svc.current(c.slot) != Some(c.generation) {
            return Err(Error::Denied);
        }
        self.prepare(c.slot, c.generation)?;
        let app_path = c.buf.get(c.path.clone()).ok_or(Error::Corrupt { at: 2 })?;
        let (slot, path) = (c.slot, &mut self.path);
        let volume = self
            .svc
            .with_mounts(slot, |m| {
                let v = resolve(slot, m, app_path, path)?;
                Ok::<_, Error>(v.and_then(|i| m.get(i)).map(|m| m.shared.len()))
            })
            .ok_or(Error::Denied)??;
        let p = self.path.as_bytes();
        let room = c.out.len().saturating_sub(REQ_HEADER).min(MAX_IO_CHUNK);
        match c.op {
            OP_STAT => {
                let (size, _dir) = self.fs.stat(p)?;
                Ok((size, 0))
            }
            OP_READ => {
                let n = usize::try_from(c.n).unwrap_or(usize::MAX).min(room);
                let dst = c
                    .out
                    .get_mut(REQ_HEADER..REQ_HEADER + n)
                    .ok_or(Error::TooLarge { len: n, max: room })?;
                let got = self.fs.read_at(p, c.off, dst)?;
                Ok((got as u64, got))
            }
            OP_WRITE => {
                let data = c.buf.get(c.data.clone()).ok_or(Error::Corrupt { at: 0 })?;
                if data.len() > MAX_IO_CHUNK {
                    return Err(Error::TooLarge {
                        len: data.len(),
                        max: MAX_IO_CHUNK,
                    });
                }
                self.fs.write_at(p, c.off, data)?;
                if volume.is_some() {
                    self.first_save(c, data.len());
                }
                Ok((data.len() as u64, 0))
            }
            OP_LIST => {
                let dst = c
                    .out
                    .get_mut(REQ_HEADER..REQ_HEADER + room)
                    .unwrap_or(&mut []);
                let (count, len) = self.fs.list_into(p, dst)?;
                Ok((count as u64, len))
            }
            OP_REMOVE => {
                // De eigen root en een volume zelf zijn het zicht, geen
                // bestand: die blijven.
                let mut cp = PathBuf::new();
                clean_abs(app_path, &mut cp)?;
                let is_mount = self
                    .svc
                    .with_mounts(slot, |m| m.iter().any(|m| m.local == cp.as_bytes()))
                    .unwrap_or(false);
                if cp.as_bytes() == b"/" || is_mount {
                    return Err(Error::Denied);
                }
                self.fs.remove(p, false)?;
                Ok((0, 0))
            }
            OP_TRUNCATE => {
                self.fs.truncate(p, c.n)?;
                Ok((c.n, 0))
            }
            _ => Err(Error::Kind),
        }
    }

    /// Eén regel per levensduur bij de eerste geslaagde schrijf in een
    /// volume: het bewijs dat een bewoner zijn staat buiten zijn eigen root
    /// bewaart (Hop: `/hop/agent-state.json`).
    fn first_save(&mut self, c: &FsCall, len: usize) {
        let i = c.slot.get();
        if self.saved.get(i).copied().flatten() == Some(c.generation) {
            return;
        }
        if let Some(s) = self.saved.get_mut(i) {
            *s = Some(c.generation);
        }
        let app = c.buf.get(c.path.clone()).unwrap_or(&[]);
        let app = core::str::from_utf8(app).unwrap_or("<not utf-8>");
        let to = core::str::from_utf8(self.path.as_bytes()).unwrap_or("<not utf-8>");
        self.log.log(format_args!(
            "hopfs: slot {} saved {app} as {to} ({len} bytes at {}) HOPOS_FS_SAVED",
            c.slot, c.off
        ));
    }
}

/// Legt de boom vast: elke [`COMMIT_EVERY`], en meteen als een slot stopte
/// (de servicer-tabel verliest zijn generatie). Vuur-en-vergeet: een volle
/// brievenbus is de volgende ronde opnieuw.
pub async fn committer<T: Timer>(
    svc: &Servicers,
    inbox: &FsInbox<'_>,
    timer: &T,
    max_slots: usize,
) {
    let mut live = [false; SLOT_CAP + 1];
    let every = COMMIT_EVERY.as_secs() / COMMIT_POLL.as_secs().max(1);
    let mut rounds = 0u64;
    loop {
        timer.sleep(COMMIT_POLL).await;
        rounds += 1;
        let mut why = None;
        for i in 1..=max_slots.min(SLOT_CAP) {
            let Some(slot) = Slot::new(i) else { continue };
            let now = svc.current(slot).is_some();
            if let Some(was) = live.get_mut(i) {
                if *was && !now {
                    why = Some(CommitWhy::Stopped(slot));
                }
                *was = now;
            }
        }
        if why.is_none() && rounds >= every {
            why = Some(CommitWhy::Periodic);
        }
        if let Some(w) = why
            && inbox
                .try_send(FsEnvelope {
                    msg: FsMsg::Commit(w),
                    reply: None,
                })
                .is_ok()
        {
            rounds = 0;
        }
    }
}

#[cfg(test)]
pub(crate) mod tests;
