//! De system-API: het control- en datakanaal van een app naar HOP, over het
//! gewone interne LAN.
//!
//! Netwerkisolatie bepaalt de peer, de peer bepaalt het slot (het bron-IP,
//! [`slot_from_remote`]) en de levende servicer van dat slot bepaalt de
//! levensduur. Er is geen tweede identiteitspad.
//!
//! Bovenop de gewone calls staan de BEVOEGDE operaties
//! ([`abi::systemapi::PrivOp`], 0x40 tot en met 0x46): een slot reserveren,
//! een image erin stromen, stoppen, status, logregels, de klok, de flip.
//! Alleen het slot met de [`Privilege`] mag ze: dat is Hop, de eerste
//! bewoner (PORT.md beslissing 1). De kern maakt het token één keer bij boot
//! en geeft het aan het slot van Hop; nergens anders bestaat er een. Een
//! privileged operatie is een functie die `&Privilege` eist: bewijs als
//! parameter (handboek §2.1), geen vlag die je vergeet te toetsen.
//!
//! # Een start
//!
//! `START_SLOT` kiest zelf een leeg slot en claimt het bij de
//! lifecycle-actor ([`Request::Claim`]); de [`ImageGrant`] blijft hier, in
//! een tabel van hoogstens [`MAX_STREAMS`] stromen. `STREAM_IMAGE` routeert
//! elke byte meteen naar zijn eindadres (de `Placer`, de Rust-vorm van
//! `OLD/metal/abi/place/stream.go`): binnen een PT_LOAD naar de partitie, de
//! staart van het bestand (sectieheaders, symbooltabel) naar een
//! schrapruimte bovenin het app-RAM. Na de laatste byte leest `leanelf` de
//! symbolen, bouwt [`abi::place::build`] het plan met álle toetsen, gaan de
//! patches en de env erin, en krijgt de actor de grant terug met
//! [`Request::Arm`]. Faalt iets, dan gaat de grant terug met
//! [`Request::Abort`]: de kern ruimt zijn eigen reserveringen op.
//!
//! De TCP-verbinding komt van `leannet`, over de [`Conn`]-trait.
//!
//! # Verbindingen naast elkaar
//!
//! [`System`] is deelbaar: [`System::serve`] leent hem als `&self`, en elke
//! verbinding is een eigen taak uit een vaste pool met haar eigen buffers en
//! haar eigen [`Reply`]. De staat die tussen twee calls leeft (de stromen)
//! is een leesbare tabel in een `LocalCell` (handboek §1.1): elke lening
//! duurt één synchrone stap, nooit over een `.await`. Een verbinding hoort
//! bij één levensduur van haar slot: wisselt die (stop, herstart), dan sluit
//! de kern haar binnen [`LIFE_TICK`], ook als de peer zwijgt. Gemeten 29-09
//! vóór deze vorm: één verbinding tegelijk, en de half-open verbinding van
//! een geparkeerde app hield de listener voor altijd vast (de toets van
//! buiten zag daarna nooit meer een antwoord).

use crate::cage::{Console, CoreClass, PhysMem, Timer};
use crate::pool::{GroupName, Placement};
use crate::slots::{
    self, Envelope, ImageGrant, Occupancy, Reply, Request, Response, Servicers, StartSpec, try_vec,
};
use crate::{Error, Result, SLOT_CAP, Slot};
use abi::layout::{ABI_CTRL_OFF, ABI_TAIL, LINK_BASE};
use abi::place::{self, MAX_SEGMENTS, Segment};
use abi::systemapi::{PrivOp, SlotInfo, SlotState, StartReq, StreamResp, StreamState};
use alloc::vec::Vec;
use bounded::BoundedVec;
use core::fmt;
use core::future::Future;
use core::sync::atomic::{AtomicBool, Ordering::AcqRel};
use core::time::Duration;
use sync::mpsc::Mailbox;
use sync::{Either, LocalCell, select};

/// De versie van het frame (`systemapi.Version`).
pub const VERSION: u8 = 1;
/// HOP's vaste interne servicepoort.
pub const PORT: u16 = 10100;
/// Een call van de app.
pub const KIND_CALL: u8 = 1;
/// Het antwoord op een call.
pub const KIND_RESULT: u8 = 2;
/// Een logregel van de app.
pub const KIND_LOG: u8 = 3;
/// De grootste I/O-brok van één call.
pub const MAX_IO_CHUNK: usize = 1 << 20;
/// De grootste payload van één frame.
pub const MAX_PAYLOAD: usize = MAX_IO_CHUNK + (64 << 10);
/// "HOPS" little-endian op de draad.
pub const MAGIC: u32 = 0x5350_4f48;
/// De kop: magic, versie, soort, twee gereserveerd, lengte.
pub const HEADER_LEN: usize = 12;
/// Open system-verbindingen per levensduur. applib houdt er één open; de
/// tweede is voor een herverbinding waarvan HOP de FIN van de oude nog niet
/// zag. Elke verbinding houdt netwerk- en callbuffers vast.
pub const MAX_SYSTEM_CONNS: u8 = 2;
/// Het interne net: 10.100.0.0/24, HOP is .1, slot i is .(i+1).
pub const NET: u32 = (10 << 24) | (100 << 16);
/// Hoeveel images tegelijk mogen stromen. Hop begrenst zelf op vier
/// (`runner::MAX_CONCURRENT_DOWNLOADS`); de kern houdt ruimte voor een
/// flip-bundel en een herstart ernaast.
pub const MAX_STREAMS: usize = 8;
/// Hoeveel bytes van de kop gebufferd mogen worden vóór de program headers
/// compleet zijn. Een echte Go-ELF heeft ze binnen een kilobyte; dit is de
/// enige heap die een streamende plaatsing per stroom kost (`maxHead` in
/// `stream.go`).
pub const MAX_HEAD: usize = 64 << 10;
/// De grootste symbooltabel plus stringtabel die de plaatsing leest. Ze
/// komen één keer, bij de laatste byte, uit de schrapruimte naar de heap,
/// omdat `leanelf` over een `&[u8]` werkt; een Go-image draagt er een paar
/// MB van. Koud pad; de grens maakt een verzonnen header geen OOM.
pub const MAX_SYMBOLS: u64 = 16 << 20;
/// De logring per slot voor `NEXT_LOG`, in bytes (kop van 2 bytes per regel).
pub const LOG_RING_BYTES: usize = 2048;
/// De langste regel in die ring; langer wordt afgekapt.
pub const LOG_LINE_MAX: usize = 256;
/// Hoe vaak een wachtende verbinding kijkt of haar levensduur nog loopt.
///
/// De stopbel van de servicer heeft één wachter (de servicer zelf), dus een
/// verbinding kijkt naar de generatie in de servicer-tabel: bij elke wek, en
/// zonder verkeer op deze tik. Go sloot de verbinding op `<-s.stop`; hier is
/// het hoogstens één tik later. 100 ms is ruim onder elke herstart van een
/// slot (scrub en image-stream duren seconden), dus de nieuwe levensduur
/// krijgt haar toelating ([`MAX_SYSTEM_CONNS`]) altijd terug; tien wekken
/// per seconde per open verbinding is op de meetlat van 29-09 (ruim 700
/// slaapjes per seconde op QEMU) ruis.
pub const LIFE_TICK: Duration = Duration::from_millis(100);
/// Hoe lang een verbinding zonder enig verkeer open blijft.
///
/// Go had geen time-out, wel `evict`; de generatietoets hierboven is die
/// evict. Dit is de vangrail daarachter: een levend slot waarvan de peer
/// verdween zonder FIN (een app-stack die opnieuw begon) houdt anders een
/// van zijn twee plaatsen voor altijd bezet. Vijf minuten, want applib
/// houdt zijn verbinding stil open tussen twee logregels en merkt een
/// gesloten verbinding pas bij de volgende schrijf (die regel kan dan
/// verloren gaan); een app die minder dan eens per vijf minuten logt, betaalt
/// dat hoogstens één keer per vijf minuten.
pub const IDLE_TIMEOUT: Duration = Duration::from_secs(300);

// De framing en het adresplan zijn van `abi`; de kern spiegelt ze als
// constanten en de compiler bewaakt dat ze gelijk blijven.
const _: () = {
    use abi::systemapi as sa;
    assert!(VERSION == sa::VERSION && PORT == sa::PORT && MAGIC == sa::MAGIC);
    assert!(MAX_IO_CHUNK == sa::MAX_IO_CHUNK && MAX_PAYLOAD == sa::MAX_PAYLOAD);
    assert!(HEADER_LEN == sa::HEADER_LEN);
    assert!(KIND_CALL == sa::Kind::Call as u8 && KIND_RESULT == sa::Kind::Result as u8);
    assert!(KIND_LOG == sa::Kind::Log as u8);
    assert!(NET | 1 == abi::layout::HOST_IP4);
    assert!(ABI_VERSION == abi::hopabi::VERSION);
    assert!(REQ_HEADER == abi::hopabi::HDR_LEN);
    assert!(STATUS_OK == abi::hopabi::STATUS_OK && STATUS_ERROR == abi::hopabi::STATUS_ERROR);
    assert!(STATUS_NO_ENT == abi::hopabi::STATUS_NO_ENT);
    assert!(STATUS_DENIED == abi::hopabi::STATUS_DENIED);
    assert!(LOG_LINE_MAX + 2 <= LOG_RING_BYTES);
};

/// De hopabi-versie van een call.
pub const ABI_VERSION: u8 = 1;
/// De kop van een hopabi-call of -antwoord.
pub const REQ_HEADER: usize = 24;
/// Status: gelukt.
pub const STATUS_OK: u16 = 0;
/// Status: fout (tekst in de data).
pub const STATUS_ERROR: u16 = 1;
/// Status: bestaat niet.
pub const STATUS_NO_ENT: u16 = 2;
/// Status: niet toegestaan.
pub const STATUS_DENIED: u16 = 3;

/// De bevoegdheid van Hop: het enige token waarmee de lifecycle over de
/// system-API bestuurd wordt.
///
/// Geen `Clone`, geen publieke constructor: [`Privilege::boot`] geeft er
/// precies één, aan precies één slot.
#[derive(Debug)]
pub struct Privilege {
    slot: Slot,
}

static MINTED: AtomicBool = AtomicBool::new(false);

impl Privilege {
    /// Geeft de bevoegdheid aan `hop`: de eerste aanroep sinds boot krijgt
    /// het token, elke volgende `None`.
    pub fn boot(hop: Slot) -> Option<Privilege> {
        if MINTED.swap(true, AcqRel) {
            return None;
        }
        Some(Privilege { slot: hop })
    }

    /// Het slot dat de bevoegdheid draagt.
    #[must_use]
    pub fn slot(&self) -> Slot {
        self.slot
    }

    #[cfg(test)]
    pub(crate) fn for_test(slot: Slot) -> Privilege {
        Privilege { slot }
    }
}

/// Een TCP-verbinding van `leannet`: een handvat met een rij.
pub trait Conn {
    /// Leest hooguit `buf.len()` bytes; 0 = de peer sloot.
    fn read(&mut self, buf: &mut [u8]) -> impl Future<Output = Result<usize>>;
    /// Schrijft een deel van `buf`; geeft terug hoeveel.
    fn write(&mut self, buf: &[u8]) -> impl Future<Output = Result<usize>>;
    /// Het IPv4-bronadres van de peer (big-endian als getal).
    fn remote_ip4(&self) -> u32;
}

/// Wat de system-API buiten de lifecycle nodig heeft.
///
/// Alle verbindingen delen dezelfde haken, dus `&self`: een haak met staat
/// houdt die zelf in een `LocalCell` of stuurt een bericht aan zijn eigenaar.
pub trait Hooks {
    /// Zet de klok (Unix-nanoseconden).
    fn set_clock(&self, unix_ns: u64);
    /// Start een kern-flip naar de bundel die in slot `bundle` gestroomd is
    /// en de SHA-256 `sha256` moet hebben. De details (reserveren zonder
    /// starten) komen met de flip zelf.
    fn flip(&self, bundle: Slot, sha256: &[u8; 32]) -> Result;
}

/// Het slot achter een bron-IP, of `None` als het geen app-adres is.
#[must_use]
pub fn slot_from_remote(ip: u32, max_slots: usize) -> Option<Slot> {
    if ip & 0xFFFF_FF00 != NET {
        return None;
    }
    let slot = Slot::new(((ip & 0xFF) as usize).checked_sub(1)?)?;
    (slot.get() <= max_slots).then_some(slot)
}

async fn read_full(c: &mut impl Conn, buf: &mut [u8]) -> Result {
    let mut got = 0;
    while got < buf.len() {
        let n = c.read(buf.get_mut(got..).unwrap_or(&mut [])).await?;
        if n == 0 {
            return Err(Error::Conn);
        }
        got += n;
    }
    Ok(())
}

async fn write_all(c: &mut impl Conn, mut buf: &[u8]) -> Result {
    while !buf.is_empty() {
        let n = c.write(buf).await?;
        if n == 0 {
            return Err(Error::Conn);
        }
        buf = buf.get(n..).unwrap_or(&[]);
    }
    Ok(())
}

/// Leest een framekop: soort en lengte.
pub async fn read_header(c: &mut impl Conn) -> Result<(u8, usize)> {
    let mut h = [0u8; HEADER_LEN];
    read_full(c, &mut h).await?;
    let magic = u32::from_le_bytes([h[0], h[1], h[2], h[3]]);
    if magic != MAGIC {
        return Err(Error::Version {
            have: u64::from(magic),
            want: u64::from(MAGIC),
        });
    }
    if h[4] != VERSION {
        return Err(Error::Version {
            have: u64::from(h[4]),
            want: u64::from(VERSION),
        });
    }
    let n = u32::from_le_bytes([h[8], h[9], h[10], h[11]]) as usize;
    if n > MAX_PAYLOAD {
        return Err(Error::TooLarge {
            len: n,
            max: MAX_PAYLOAD,
        });
    }
    Ok((h[5], n))
}

/// Schrijft één frame.
pub async fn write_frame(c: &mut impl Conn, kind: u8, payload: &[u8]) -> Result {
    if payload.len() > MAX_PAYLOAD {
        return Err(Error::TooLarge {
            len: payload.len(),
            max: MAX_PAYLOAD,
        });
    }
    let mut h = [0u8; HEADER_LEN];
    h[..4].copy_from_slice(&MAGIC.to_le_bytes());
    h[4] = VERSION;
    h[5] = kind;
    h[8..].copy_from_slice(&(payload.len() as u32).to_le_bytes());
    write_all(c, &h).await?;
    write_all(c, payload).await
}

/// Een hopabi-call.
#[derive(Debug, PartialEq, Eq)]
pub struct Call<'a> {
    /// De operatie.
    pub op: u8,
    /// Het volgnummer (komt terug in het antwoord).
    pub seq: u32,
    /// Offset (of slot, bij de bevoegde ops).
    pub off: u64,
    /// Lengte (of maat, adres, time-out).
    pub n: u64,
    /// Het pad.
    pub path: &'a [u8],
    /// De data.
    pub data: &'a [u8],
}

impl<'a> Call<'a> {
    /// Decodeert een call.
    pub fn decode(b: &'a [u8]) -> Result<Call<'a>> {
        let u = |o: usize| -> u64 {
            b.get(o..o + 8)
                .and_then(|s| <[u8; 8]>::try_from(s).ok())
                .map_or(0, u64::from_le_bytes)
        };
        if b.len() < REQ_HEADER {
            return Err(Error::Corrupt { at: b.len() });
        }
        if b[0] != ABI_VERSION {
            return Err(Error::Version {
                have: u64::from(b[0]),
                want: u64::from(ABI_VERSION),
            });
        }
        let plen = usize::from(u16::from_le_bytes([b[2], b[3]]));
        let path = b
            .get(REQ_HEADER..REQ_HEADER + plen)
            .ok_or(Error::Corrupt { at: 2 })?;
        Ok(Call {
            op: b[1],
            seq: u32::from_le_bytes([b[4], b[5], b[6], b[7]]),
            off: u(8),
            n: u(16),
            path,
            data: b.get(REQ_HEADER + plen..).unwrap_or(&[]),
        })
    }

    /// Dezelfde call als [`abi::hopabi::Req`], voor de decoders van `abi`.
    fn as_req(&self) -> abi::hopabi::Req<'a> {
        abi::hopabi::Req {
            op: self.op,
            seq: self.seq,
            off: self.off,
            n: self.n,
            path: self.path,
            data: self.data,
        }
    }
}

/// Schrijft alleen de antwoordkop in `out[..REQ_HEADER]`.
fn put_resp_head(out: &mut [u8], op: u8, status: u16, seq: u32, size: u64) {
    if let Some(h) = out.get_mut(..REQ_HEADER) {
        h[0] = ABI_VERSION;
        h[1] = op;
        h[2..4].copy_from_slice(&status.to_le_bytes());
        h[4..8].copy_from_slice(&seq.to_le_bytes());
        h[8..16].copy_from_slice(&size.to_le_bytes());
        h[16..24].fill(0);
    }
}

/// Schrijft een antwoordkop in `out` gevolgd door `data`; geeft de lengte.
pub fn encode_resp(out: &mut [u8], op: u8, status: u16, seq: u32, size: u64, data: &[u8]) -> usize {
    let n = (REQ_HEADER + data.len()).min(out.len());
    put_resp_head(out, op, status, seq, size);
    if let (Some(d), Some(s)) = (
        out.get_mut(REQ_HEADER..n),
        data.get(..n.saturating_sub(REQ_HEADER)),
    ) {
        d.copy_from_slice(s);
    }
    n
}

// ---------------------------------------------------------------------------
// De logring per slot (NEXT_LOG).
// ---------------------------------------------------------------------------

/// Eén korte ring van logregels: `[len u16][bytes]`, de oudste valt eruit.
/// De buffer komt pas bij de eerste regel (faalbaar); een slot dat nooit
/// logt kost niets.
#[derive(Debug, Default)]
struct LineRing {
    buf: Vec<u8>,
    /// De leespositie.
    head: usize,
    /// Het aantal bezette bytes.
    used: usize,
}

impl LineRing {
    const fn new() -> LineRing {
        LineRing {
            buf: Vec::new(),
            head: 0,
            used: 0,
        }
    }

    fn byte(&self, i: usize) -> u8 {
        self.buf
            .get((self.head + i) % LOG_RING_BYTES)
            .copied()
            .unwrap_or(0)
    }

    fn put(&mut self, at: usize, b: u8) {
        if let Some(x) = self.buf.get_mut(at % LOG_RING_BYTES) {
            *x = b;
        }
    }

    /// De lengte van de oudste regel, of `None` als de ring leeg is.
    fn front_len(&self) -> Option<usize> {
        (self.used >= 2).then(|| usize::from(u16::from_le_bytes([self.byte(0), self.byte(1)])))
    }

    /// Laat de oudste regel vallen.
    fn drop_front(&mut self) {
        let n = self
            .front_len()
            .map_or(self.used, |n| (n + 2).min(self.used));
        self.head = (self.head + n) % LOG_RING_BYTES;
        self.used -= n;
    }

    fn push(&mut self, line: &[u8]) {
        if self.buf.is_empty() {
            if self.buf.try_reserve_exact(LOG_RING_BYTES).is_err() {
                return; // Geen heap: de regel gaat alleen naar de console.
            }
            self.buf.resize(LOG_RING_BYTES, 0);
        }
        let line = line.get(..LOG_LINE_MAX).unwrap_or(line);
        let need = line.len() + 2;
        while LOG_RING_BYTES - self.used < need {
            self.drop_front();
        }
        let tail = self.head + self.used;
        let len = (line.len() as u16).to_le_bytes();
        for (i, b) in len.iter().chain(line).enumerate() {
            self.put(tail + i, *b);
        }
        self.used += need;
    }

    fn pop(&mut self, dst: &mut [u8]) -> Option<usize> {
        let n = self.front_len()?;
        let k = n.min(dst.len());
        for (i, d) in dst.iter_mut().take(k).enumerate() {
            *d = self.byte(2 + i);
        }
        self.drop_front();
        Some(k)
    }

    fn clear(&mut self) {
        self.head = 0;
        self.used = 0;
    }
}

/// De korte logringen van alle slots, voor `NEXT_LOG`: Hop haalt er de
/// regels van zijn apps uit.
///
/// Een leesbare tabel (handboek §1.1): de servicers schrijven via
/// [`LogTee`], de system-API leest; elke lening duurt één regel, zonder
/// `.await`. Vol is de oudste regel eruit; niets blokkeert.
pub struct SlotLogs {
    rings: LocalCell<[LineRing; SLOT_CAP + 1]>,
}

impl SlotLogs {
    /// Een lege tabel, voor in een `static`.
    #[must_use]
    pub const fn new() -> SlotLogs {
        SlotLogs {
            rings: LocalCell::cell([const { LineRing::new() }; SLOT_CAP + 1]),
        }
    }

    /// Zet een regel van `slot` in zijn ring.
    pub fn push(&self, slot: Slot, line: &[u8]) {
        if let Some(r) = self.rings.borrow_mut().get_mut(slot.get()) {
            r.push(line);
        }
    }

    /// Haalt de oudste regel van `slot` in `dst` (afgekapt op de lengte van
    /// `dst`); `None` als er niets klaarstaat.
    pub fn pop(&self, slot: Slot, dst: &mut [u8]) -> Option<usize> {
        self.rings.borrow_mut().get_mut(slot.get())?.pop(dst)
    }

    /// Veegt de ring van `slot` (een nieuwe levensduur begint schoon).
    pub fn clear(&self, slot: Slot) {
        if let Some(r) = self.rings.borrow_mut().get_mut(slot.get()) {
            r.clear();
        }
    }
}

impl Default for SlotLogs {
    fn default() -> Self {
        SlotLogs::new()
    }
}

/// De haak die de logringen vult: een [`Console`] die elke app-regel ook in
/// [`SlotLogs`] zet. Geef hem aan de servicer-taken en aan
/// [`System::serve`] in plaats van de kale console.
pub struct LogTee<'a, C> {
    inner: C,
    logs: &'a SlotLogs,
}

impl<'a, C: Console> LogTee<'a, C> {
    /// Een tee over `inner` naar `logs`; `const`, zodat hij in een `static`
    /// kan die alle verbindingstaken delen.
    pub const fn new(inner: C, logs: &'a SlotLogs) -> Self {
        LogTee { inner, logs }
    }
}

impl<C: Console> Console for LogTee<'_, C> {
    fn log(&self, args: fmt::Arguments<'_>) {
        self.inner.log(args);
    }
    fn app_line(&self, slot: Slot, line: &[u8]) {
        self.logs.push(slot, line);
        self.inner.app_line(slot, line);
    }
}

// ---------------------------------------------------------------------------
// De streamende plaatsing.
// ---------------------------------------------------------------------------

/// Waarom een start of stroom niet lukte; de tekst gaat naar Hop.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Fail {
    Kern(Error),
    Place(abi::Error),
    Elf(leanelf::Error),
    /// Een stroom die niet klopt, met twee getallen.
    Stream(&'static str, u64, u64),
}

impl From<Error> for Fail {
    fn from(e: Error) -> Fail {
        Fail::Kern(e)
    }
}

impl From<abi::Error> for Fail {
    fn from(e: abi::Error) -> Fail {
        Fail::Place(e)
    }
}

impl From<leanelf::Error> for Fail {
    fn from(e: leanelf::Error) -> Fail {
        Fail::Elf(e)
    }
}

impl fmt::Display for Fail {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Fail::Kern(e) => write!(f, "{e}"),
            Fail::Place(e) => write!(f, "placement: {e}"),
            Fail::Elf(e) => write!(f, "placement: {e}"),
            Fail::Stream(what, a, b) => write!(f, "stream placement: {what} ({a}, {b})"),
        }
    }
}

/// Een little-endian getal uit `b` op `i`, of 0.
fn le(b: &[u8], i: usize, n: usize) -> u64 {
    let mut w = [0u8; 8];
    if let (Some(d), Some(s)) = (w.get_mut(..n), b.get(i..i + n)) {
        d.copy_from_slice(s);
    }
    u64::from_le_bytes(w)
}

/// Leest `dst.len()` bytes op `pa`, ook ongealigneerd (woordgewijs).
fn read_bytes(mem: &impl PhysMem, pa: u64, dst: &mut [u8]) {
    let mut i = 0;
    while i < dst.len() {
        let a = pa.wrapping_add(i as u64);
        let (word, lead) = (a & !7, (a & 7) as usize);
        let w = mem.read64(word).to_le_bytes();
        let n = (8 - lead).min(dst.len() - i);
        if let (Some(d), Some(s)) = (dst.get_mut(i..i + n), w.get(lead..lead + n)) {
            d.copy_from_slice(s);
        }
        i += n;
    }
}

/// Nult `len` bytes op offset `off` van de partitie van `g`: de randen via
/// de grant, het gealigneerde midden in één `clear`.
fn zero(g: &mut ImageGrant, mem: &mut impl PhysMem, off: u64, len: u64) -> Result {
    const Z: [u8; 8] = [0; 8];
    let r = g.region();
    match off.checked_add(len) {
        Some(end) if end <= r.size => {}
        _ => {
            return Err(Error::Range {
                base: r.base.wrapping_add(off),
                size: len,
            });
        }
    }
    let (a, b) = ((off + 7) & !7, (off + len) & !7);
    if a >= b {
        let mut o = off;
        while o < off + len {
            let n = (off + len - o).min(8);
            g.write(mem, o, Z.get(..n as usize).unwrap_or(&Z))?;
            o += n;
        }
        return Ok(());
    }
    g.write(mem, off, Z.get(..(a - off) as usize).unwrap_or(&[]))?;
    mem.clear(r.base + a, b - a);
    g.write(mem, b, Z.get(..(off + len - b) as usize).unwrap_or(&[]))
}

/// De streamende plaatsing van één image: elke byte meteen op zijn plek.
///
/// Volgorde is de enige aanname (zoals in Go, getoetst op echte
/// tamago-images): de PT_LOAD's komen in oplopende bestandsoffset. Een image
/// dat dat niet doet, faalt luid in plaats van stil verkeerd geplaatst te
/// worden. De schrapruimte voor de staart is exact passend: de image-maat
/// is vooraf bekend en zodra de headers binnen zijn ook het einde van het
/// laatste PT_LOAD.
#[derive(Debug)]
struct Placer {
    /// De aangekondigde image-maat.
    size: u64,
    /// De app-RAM-maat van de partitie.
    app_ram: u64,
    /// Hoeveel bytes er gerouteerd zijn (na `ready`).
    pos: u64,
    /// De kop tot de program headers compleet zijn.
    head: Vec<u8>,
    /// De ELF-header, bewaard voor de sectietabel bij de laatste byte.
    ehdr: [u8; 64],
    /// De routeringstabel: de PT_LOAD's in bestandsvolgorde.
    segs: BoundedVec<Segment, MAX_SEGMENTS>,
    /// Waar het laatste PT_LOAD in het bestand eindigt.
    seg_end: u64,
    /// De schrapruimte (offset vanaf de linkbasis) voor de staart.
    scr_off: u64,
    /// De entry uit de header.
    entry: u64,
    ready: bool,
}

impl Placer {
    fn new(size: u64, app_ram: u64) -> core::result::Result<Placer, Fail> {
        if size == 0 {
            return Err(Fail::Place(abi::Error::ImageSize(0)));
        }
        place::Window::canonical(app_ram, 0, app_ram).check()?;
        Ok(Placer {
            size,
            app_ram,
            pos: 0,
            head: Vec::new(),
            ehdr: [0; 64],
            segs: BoundedVec::new(),
            seg_end: 0,
            scr_off: 0,
            entry: 0,
            ready: false,
        })
    }

    /// Hoeveel bytes er binnen zijn.
    fn received(&self) -> u64 {
        if self.ready {
            self.pos
        } else {
            self.head.len() as u64
        }
    }

    fn fail(what: &'static str, a: u64, b: u64) -> Fail {
        Fail::Stream(what, a, b)
    }

    /// Voert de volgende bytes in.
    fn feed(
        &mut self,
        g: &mut ImageGrant,
        mem: &mut impl PhysMem,
        chunk: &[u8],
    ) -> core::result::Result<(), Fail> {
        let have = self.received();
        if chunk.len() as u64 > self.size - have {
            return Err(Self::fail(
                "more bytes than announced",
                have + chunk.len() as u64,
                self.size,
            ));
        }
        if self.ready {
            return self.route(g, mem, chunk);
        }
        let take = chunk.len().min(MAX_HEAD - self.head.len());
        let (now, rest) = chunk.split_at(take);
        self.head
            .try_reserve(take)
            .map_err(|_| Error::OutOfMemory { bytes: take })?;
        self.head.extend_from_slice(now);
        if !self.parse_head()? {
            if !rest.is_empty() || self.head.len() >= MAX_HEAD {
                return Err(Self::fail(
                    "program headers not complete within",
                    MAX_HEAD as u64,
                    0,
                ));
            }
            if self.head.len() as u64 == self.size {
                return Err(Self::fail(
                    "image ended before its program headers",
                    self.size,
                    0,
                ));
            }
            return Ok(());
        }
        // De headers staan: de gebufferde kop alsnog routeren, daarna
        // stroomt de rest er rechtstreeks doorheen.
        let head = core::mem::take(&mut self.head);
        self.ready = true;
        self.route(g, mem, &head)?;
        self.route(g, mem, rest)
    }

    /// Kijkt of de program headers binnen zijn en bouwt dan de routering;
    /// `false` is "nog te weinig".
    fn parse_head(&mut self) -> core::result::Result<bool, Fail> {
        let f = match leanelf::File::parse(&self.head) {
            Ok(f) => f,
            Err(leanelf::Error::Truncated {
                part: leanelf::Part::Magic | leanelf::Part::Header | leanelf::Part::ProgramHeaders,
                ..
            }) => return Ok(false),
            Err(e) => return Err(e.into()),
        };
        let lo = LINK_BASE;
        let mut last = 0;
        for s in f.segments().filter(|s| s.kind == leanelf::PT_LOAD) {
            let seg = Segment {
                paddr: s.paddr,
                off: s.off,
                filesz: s.filesz,
                memsz: s.memsz,
            };
            if seg.off < last {
                return Err(Self::fail(
                    "PT_LOAD out of file order, not streamable",
                    seg.off,
                    last,
                ));
            }
            // Grof vóór de eerste byte landt; de gezaghebbende toets doet
            // `place::build` bij de laatste.
            if seg.filesz > seg.memsz
                || seg.memsz > self.app_ram
                || seg.paddr < lo
                || seg.paddr > lo + self.app_ram - seg.memsz
            {
                return Err(Self::fail(
                    "segment outside the partition window",
                    seg.paddr,
                    seg.memsz,
                ));
            }
            if seg.off > self.size || seg.filesz > self.size - seg.off {
                return Err(Self::fail("segment outside the image", seg.off, seg.filesz));
            }
            last = seg.off + seg.filesz;
            self.segs.push(seg).map_err(|_| abi::Error::TooMany {
                what: "PT_LOAD segments",
                cap: MAX_SEGMENTS,
            })?;
        }
        if self.segs.is_empty() {
            return Err(Fail::Place(abi::Error::NoSegments));
        }
        // De staart (sectieheaders, symbooltabel) krijgt een exact passende
        // schrapruimte bovenin het app-RAM; de segmenten, met hun BSS,
        // blijven eronder.
        let tail = self.size - last;
        if tail >= self.app_ram {
            return Err(Self::fail(
                "image tail larger than the partition",
                tail,
                self.app_ram,
            ));
        }
        self.scr_off = (self.app_ram - tail) & !7;
        if let Some(s) = self
            .segs
            .iter()
            .find(|s| s.paddr + s.memsz > lo + self.scr_off)
        {
            return Err(Self::fail(
                "segments and image tail do not fit together",
                s.paddr + s.memsz,
                tail,
            ));
        }
        self.seg_end = last;
        self.entry = f.entry;
        if let Some(h) = self.head.get(..64) {
            self.ehdr.copy_from_slice(h);
        }
        Ok(true)
    }

    /// Het PT_LOAD dat bestandsoffset `off` draagt.
    fn seg_at(&self, off: u64) -> Option<Segment> {
        self.segs
            .iter()
            .find(|s| off >= s.off && off < s.off + s.filesz)
            .copied()
    }

    /// Hoeveel bytes vanaf `off` buiten elk segment vallen (hoogstens `max`).
    fn gap_len(&self, off: u64, max: u64) -> u64 {
        let next = self
            .segs
            .iter()
            .map(|s| s.off)
            .filter(|&o| o > off && o < off + max)
            .min()
            .unwrap_or(off + max);
        next - off
    }

    /// Stuurt elke byte naar zijn plek: binnen een PT_LOAD naar de
    /// partitie, voorbij het laatste naar de schrapruimte, uitlijngaten
    /// nergens heen (daar staan nullen in het bestand).
    fn route(
        &mut self,
        g: &mut ImageGrant,
        mem: &mut impl PhysMem,
        mut p: &[u8],
    ) -> core::result::Result<(), Fail> {
        while !p.is_empty() {
            let off = self.pos;
            let len = p.len() as u64;
            let (n, dst) = match self.seg_at(off) {
                Some(s) => (
                    (s.off + s.filesz - off).min(len),
                    Some(s.paddr - LINK_BASE + (off - s.off)),
                ),
                None => {
                    let n = self.gap_len(off, len);
                    (
                        n,
                        (off >= self.seg_end).then(|| self.scr_off + (off - self.seg_end)),
                    )
                }
            };
            let (now, rest) = p.split_at(n as usize);
            if let Some(d) = dst {
                g.write(mem, d, now)?;
            }
            p = rest;
            self.pos += n;
        }
        Ok(())
    }

    /// Leest `len` bytes van bestandsoffset `off` uit de schrapruimte.
    fn tail_bytes(
        &self,
        g: &ImageGrant,
        mem: &impl PhysMem,
        off: u64,
        len: u64,
        dst: &mut Vec<u8>,
    ) -> core::result::Result<(), Fail> {
        match off.checked_add(len) {
            Some(end) if off >= self.seg_end && end <= self.size => {}
            _ => {
                return Err(Self::fail(
                    "symbol tables inside a PT_LOAD, not streamable",
                    off,
                    len,
                ));
            }
        }
        let at = dst.len();
        dst.try_reserve_exact(len as usize)
            .map_err(|_| Error::OutOfMemory {
                bytes: len as usize,
            })?;
        dst.resize(at + len as usize, 0);
        let pa = g.region().base + self.scr_off + (off - self.seg_end);
        read_bytes(mem, pa, dst.get_mut(at..).unwrap_or(&mut []));
        Ok(())
    }

    /// Een ELF met alleen de header, de sectietabel, `.symtab` en zijn
    /// stringtabel, met de offsets herschreven: genoeg voor
    /// [`leanelf::File::lookup`], zonder het hele image op de heap.
    fn symbol_image(
        &self,
        g: &ImageGrant,
        mem: &impl PhysMem,
    ) -> core::result::Result<Vec<u8>, Fail> {
        const SHDR: u64 = 64;
        let mut img = Vec::new();
        img.try_reserve_exact(64)
            .map_err(|_| Error::OutOfMemory { bytes: 64 })?;
        img.extend_from_slice(&self.ehdr);
        // Geen program headers: de synthetische ELF heeft er geen.
        for r in [32..40, 56..58] {
            if let Some(b) = img.get_mut(r) {
                b.fill(0);
            }
        }
        let shoff = le(&self.ehdr, 40, 8);
        let shentsize = le(&self.ehdr, 58, 2);
        let shnum = le(&self.ehdr, 60, 2);
        if shoff == 0 || shnum == 0 || shentsize != SHDR || shnum > u64::from(leanelf::MAX_SHNUM) {
            // Laat leanelf de precieze fout zeggen.
            return Ok(img);
        }
        let tab_len = shnum * SHDR;
        self.tail_bytes(g, mem, shoff, tab_len, &mut img)?;
        if let Some(b) = img.get_mut(40..48) {
            b.copy_from_slice(&64u64.to_le_bytes());
        }
        let entry = |i: u64| 64 + (i * SHDR) as usize;
        let Some(sym) = (0..shnum).find(|&i| le(&img, entry(i) + 4, 4) == 2) else {
            return Ok(img); // Geen .symtab: leanelf zegt NoSymtab.
        };
        let link = le(&img, entry(sym) + 40, 4);
        if link >= shnum {
            return Ok(img); // leanelf zegt SymtabLink.
        }
        let (sym_off, sym_len) = (le(&img, entry(sym) + 24, 8), le(&img, entry(sym) + 32, 8));
        let (str_off, str_len) = (le(&img, entry(link) + 24, 8), le(&img, entry(link) + 32, 8));
        if sym_len.saturating_add(str_len) > MAX_SYMBOLS {
            return Err(Self::fail(
                "symbol tables larger than",
                sym_len.saturating_add(str_len),
                MAX_SYMBOLS,
            ));
        }
        let new_sym = img.len() as u64;
        self.tail_bytes(g, mem, sym_off, sym_len, &mut img)?;
        let new_str = img.len() as u64;
        self.tail_bytes(g, mem, str_off, str_len, &mut img)?;
        for (i, at) in [(sym, new_sym), (link, new_str)] {
            if let Some(b) = img.get_mut(entry(i) + 24..entry(i) + 32) {
                b.copy_from_slice(&at.to_le_bytes());
            }
        }
        Ok(img)
    }

    /// Sluit de plaatsing af: symbolen lezen, het plan bouwen en toetsen,
    /// BSS nullen, patchen, de schrapruimte wissen. Geeft de entry.
    fn finish(
        &mut self,
        g: &mut ImageGrant,
        mem: &mut impl PhysMem,
        slot: Slot,
    ) -> core::result::Result<u64, Fail> {
        if !self.ready || self.pos != self.size {
            return Err(Self::fail("image incomplete", self.received(), self.size));
        }
        let img = self.symbol_image(g, mem)?;
        let f = leanelf::File::parse(&img)?;
        let [ram_start, ram_size, hint, stamp] = f.lookup([
            place::SYM_RAM_START,
            place::SYM_RAM_SIZE,
            place::SYM_SLOT_HINT,
            place::SYM_ABI,
        ])?;
        // De stempel is inhoud, geen adres: hij staat al op zijn plek.
        let abi_value = stamp.and_then(|s| {
            let n = if s.size == 4 { 4 } else { 8 };
            let off = s.value.checked_sub(LINK_BASE)?;
            (off.checked_add(n)? <= self.app_ram).then(|| {
                let mut b = [0u8; 8];
                read_bytes(
                    mem,
                    g.region().base + off,
                    b.get_mut(..n as usize).unwrap_or(&mut []),
                );
                u64::from_le_bytes(b)
            })
        });
        let image = place::Image {
            size: self.size,
            entry: self.entry,
            segments: &self.segs,
            symbols: place::Symbols {
                ram_start: ram_start.map(|s| s.value),
                ram_size: ram_size.map(|s| s.value),
                slot_hint: hint.map(|s| s.value),
                abi: abi_value,
            },
        };
        let w = place::Window::canonical(self.app_ram, 0, self.scr_off);
        let abi_slot = abi::layout::Slot::new(slot.get()).ok_or(Error::SlotRange {
            slot: slot.get(),
            max: SLOT_CAP,
        })?;
        let plan = place::build(&image, &w, abi_slot, Some(abi::ABI_VERSION))?;
        drop(img);
        // Build zag dezelfde headers; toch getoetst, want uiteenlopen is
        // precies de klasse fouten die stil blijft.
        if plan.segments.as_slice() != self.segs.as_slice() {
            return Err(Self::fail(
                "plan disagrees with the stream routing",
                plan.segments.len() as u64,
                self.segs.len() as u64,
            ));
        }
        for s in plan.segments.iter() {
            zero(g, mem, s.paddr - LINK_BASE + s.filesz, s.memsz - s.filesz)?;
        }
        for p in plan.patches.iter() {
            g.write(mem, p.addr - LINK_BASE, &p.val.to_le_bytes())?;
        }
        zero(g, mem, self.scr_off, self.size - self.seg_end)?;
        for s in plan.segments.iter() {
            mem.clean_inv(g.region().base + (s.paddr - LINK_BASE), s.memsz);
        }
        Ok(plan.entry)
    }
}

/// Eén lopende stroom: de grant uit de claim, de plaatsing en de env.
struct Stream {
    grant: ImageGrant,
    placer: Placer,
    env: Vec<u8>,
}

impl Stream {
    fn slot(&self) -> Slot {
        self.grant.slot()
    }

    /// Zet de env-blob op de control-page (de kern schrijft, de app leest
    /// hem bij de start).
    fn put_env(&mut self, mem: &mut impl PhysMem) -> Result {
        use abi::hopabi::{CTRL_ENV_DATA, CTRL_ENV_LEN};
        let ctrl = self.placer.app_ram + ABI_CTRL_OFF;
        self.grant.write(mem, ctrl + CTRL_ENV_DATA, &self.env)?;
        self.grant.write(
            mem,
            ctrl + CTRL_ENV_LEN,
            &(self.env.len() as u64).to_le_bytes(),
        )?;
        mem.clean_inv(self.grant.region().base + ctrl, abi::layout::CTRL_STRIDE);
        Ok(())
    }
}

/// Wat een bevoegde op teruggeeft: `size` en hoeveel databytes er al op
/// `out[REQ_HEADER..]` staan.
type Answer = core::result::Result<(u64, usize), Fail>;

/// Waarom een verbinding eindigde; de listener zet het in zijn regel.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum End {
    /// De peer sloot (EOF) of het transport viel weg.
    Peer,
    /// De levensduur van het slot is voorbij (stop of herstart); de kern
    /// sloot, ook als de peer nog open stond.
    Evicted,
    /// Geen verkeer binnen [`IDLE_TIMEOUT`].
    Idle,
    /// Een ongeldig frame of een andere fout.
    Failed(Error),
}

impl fmt::Display for End {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            End::Peer => f.write_str("peer closed"),
            End::Evicted => f.write_str("slot lifetime ended, closed by the kernel"),
            End::Idle => write!(f, "idle for {} s", IDLE_TIMEOUT.as_secs()),
            End::Failed(e) => write!(f, "{e}"),
        }
    }
}

/// Een verbinding onder toezicht: elke wachtende lees of schrijf kijkt op
/// elke wek, en minstens elke [`LIFE_TICK`], of de levensduur waarvoor ze
/// toegelaten werd nog loopt en of er recent verkeer was.
struct Watched<'a, C, T> {
    conn: &'a mut C,
    timer: &'a T,
    svc: &'a Servicers,
    slot: Slot,
    generation: u32,
    /// Het laatste moment met verkeer (nanoseconden van `timer`).
    last: u64,
    /// Waarom het toezicht de verbinding sloot.
    end: Option<End>,
}

impl<C: Conn, T: Timer> Watched<'_, C, T> {
    /// Een verbinding hoort bij één levensduur: een oude app die na een
    /// herstart nog bytes stuurt, krijgt de nieuwe eigenaar van hetzelfde
    /// IP nooit cadeau, en een geparkeerde app houdt niets meer vast.
    fn alive(&mut self) -> Result {
        if self.svc.current(self.slot) == Some(self.generation) {
            return Ok(());
        }
        self.end = Some(End::Evicted);
        Err(Error::Conn)
    }

    /// De toets op een tik zonder verkeer: levensduur en stilte.
    fn check(&mut self) -> Result {
        self.alive()?;
        let idle = u64::try_from(IDLE_TIMEOUT.as_nanos()).unwrap_or(u64::MAX);
        if self.timer.now().saturating_sub(self.last) >= idle {
            self.end = Some(End::Idle);
            return Err(Error::Conn);
        }
        Ok(())
    }

    /// Noteert verkeer.
    fn touch(&mut self, r: &Result<usize>) {
        if matches!(r, Ok(n) if *n > 0) {
            self.last = self.timer.now();
        }
    }
}

impl<C: Conn, T: Timer> Conn for Watched<'_, C, T> {
    async fn read(&mut self, buf: &mut [u8]) -> Result<usize> {
        loop {
            // De lees links: klaar werk gaat vóór de tik. Een gedropte lees
            // verliest niets, want een `Conn`-lees kopieert pas als hij klaar
            // is.
            match select(self.conn.read(buf), self.timer.sleep(LIFE_TICK)).await {
                Either::Left(r) => {
                    self.touch(&r);
                    return r;
                }
                Either::Right(()) => self.check()?,
            }
        }
    }

    async fn write(&mut self, buf: &[u8]) -> Result<usize> {
        loop {
            // Een peer die niet leest (een geparkeerde app met een vol
            // venster) houdt de schrijf anders voor altijd vast.
            match select(self.conn.write(buf), self.timer.sleep(LIFE_TICK)).await {
                Either::Left(r) => {
                    self.touch(&r);
                    return r;
                }
                Either::Right(()) => self.check()?,
            }
        }
    }

    fn remote_ip4(&self) -> u32 {
        self.conn.remote_ip4()
    }
}

/// De system-API: de bevoegdheid en de stromen die tussen twee calls leven.
///
/// Deelbaar tussen de verbindingstaken: alles hier is `&self`. De stromen
/// zijn een leesbare tabel (handboek §1.1); de bevoegdheid staat vast vanaf
/// de bouw.
pub struct System<'i, 'r, const N: usize> {
    inbox: &'i Mailbox<Envelope<'r>, N>,
    svc: &'i Servicers,
    privilege: Option<Privilege>,
    streams: LocalCell<[Option<Stream>; MAX_STREAMS]>,
    logs: Option<&'i SlotLogs>,
    max_slots: usize,
}

impl<'i, 'r, const N: usize> System<'i, 'r, N> {
    /// Een system-API over deze actor en servicers, met de bevoegdheid van
    /// Hop (als die er al is). `const`, zodat de kern hem als `static` aan
    /// al zijn verbindingstaken kan geven.
    pub const fn new(
        inbox: &'i Mailbox<Envelope<'r>, N>,
        svc: &'i Servicers,
        privilege: Option<Privilege>,
        max_slots: usize,
    ) -> Self {
        System {
            inbox,
            svc,
            privilege,
            streams: LocalCell::cell([const { None }; MAX_STREAMS]),
            logs: None,
            max_slots,
        }
    }

    /// Koppelt de logringen voor `NEXT_LOG` (gevuld door een [`LogTee`]).
    /// Zonder ringen antwoordt `NEXT_LOG` altijd leeg.
    #[must_use]
    pub const fn with_logs(mut self, logs: &'i SlotLogs) -> Self {
        self.logs = Some(logs);
        self
    }

    /// Laat een verbinding toe: het slot uit het bron-IP, een levende
    /// servicer, en hooguit [`MAX_SYSTEM_CONNS`]. Het resultaat geeft de
    /// verbinding weer vrij in zijn `Drop`.
    pub fn admit(&self, ip: u32) -> Option<Admitted<'i>> {
        let slot = slot_from_remote(ip, self.max_slots)?;
        let generation = self.svc.current(slot)?;
        let ctl = self.svc.ctl(slot)?;
        // `then`, niet `then_some`: een gretig gebouwde `Admitted` die bij
        // een weigering wegvalt, geeft in zijn `Drop` een plaats terug die
        // nooit genomen was (gemeten 29-09: na één weigering liet de cap een
        // derde verbinding toe).
        ctl.try_conn(MAX_SYSTEM_CONNS).then(|| Admitted {
            slot,
            generation,
            svc: self.svc,
        })
    }

    /// Dient één verbinding: frames lezen tot de peer weggaat, de
    /// levensduur van het slot eindigt, de verbinding [`IDLE_TIMEOUT`] stil
    /// is of een frame ongeldig is. Meerdere verbindingen tegelijk mogen
    /// (elk in een eigen taak): `buf` is de callbuffer van déze verbinding,
    /// `out` haar antwoordbuffer, `reply` haar antwoordplek bij de actor.
    /// `timer` draagt de tik van het toezicht. Een `KIND_LOG`-frame gaat
    /// naar `log.app_line`: geef een [`LogTee`] mee om het ook in `NEXT_LOG`
    /// te zien.
    #[expect(
        clippy::too_many_arguments,
        reason = "de verbinding brengt haar eigen buffers, antwoordplek en tik mee"
    )]
    pub async fn serve(
        &self,
        conn: &mut impl Conn,
        who: &Admitted<'_>,
        reply: &'r Reply,
        timer: &impl Timer,
        mem: &mut impl PhysMem,
        hooks: &impl Hooks,
        log: &impl Console,
        buf: &mut [u8],
        out: &mut [u8],
    ) -> End {
        let mut w = Watched {
            conn,
            timer,
            svc: self.svc,
            slot: who.slot,
            generation: who.generation,
            last: timer.now(),
            end: None,
        };
        let r = self
            .frames(&mut w, who.slot, reply, mem, hooks, log, buf, out)
            .await;
        match (w.end, r) {
            (Some(end), _) => end,
            (None, Ok(()) | Err(Error::Conn)) => End::Peer,
            (None, Err(e)) => End::Failed(e),
        }
    }

    /// De framelus van één verbinding.
    #[expect(
        clippy::too_many_arguments,
        reason = "de verbinding brengt haar eigen buffers en antwoordplek mee"
    )]
    async fn frames<C: Conn, T: Timer>(
        &self,
        w: &mut Watched<'_, C, T>,
        slot: Slot,
        reply: &'r Reply,
        mem: &mut impl PhysMem,
        hooks: &impl Hooks,
        log: &impl Console,
        buf: &mut [u8],
        out: &mut [u8],
    ) -> Result {
        loop {
            let (kind, n) = read_header(w).await?;
            if kind != KIND_CALL && kind != KIND_LOG {
                return Err(Error::Corrupt { at: 5 });
            }
            let payload = buf.get_mut(..n).ok_or(Error::TooLarge { len: n, max: 0 })?;
            read_full(w, payload).await?;
            w.alive()?;
            if kind == KIND_LOG {
                log.app_line(slot, payload);
                continue;
            }
            let len = match Call::decode(payload) {
                Ok(call) => self.call(slot, &call, reply, mem, hooks, out).await,
                Err(_) => encode_resp(out, 0, STATUS_ERROR, 0, 0, b"bad request"),
            };
            write_frame(w, KIND_RESULT, out.get(..len).unwrap_or(&[])).await?;
        }
    }

    async fn call(
        &self,
        slot: Slot,
        c: &Call<'_>,
        reply: &'r Reply,
        mem: &mut impl PhysMem,
        hooks: &impl Hooks,
        out: &mut [u8],
    ) -> usize {
        let r = match (PrivOp::from_op(c.op), &self.privilege) {
            // De gewone calls (hopfs, store, codec) zijn nog niet geport.
            (None, _) => Err(Fail::Kern(Error::Kind)),
            (Some(op), Some(p)) if p.slot == slot => {
                self.privileged(p, op, c, reply, mem, hooks, out).await
            }
            (Some(_), _) => Err(Fail::Kern(Error::Privilege { slot: slot.get() })),
        };
        match r {
            Ok((size, data_len)) => {
                put_resp_head(out, c.op, STATUS_OK, c.seq, size);
                (REQ_HEADER + data_len).min(out.len())
            }
            Err(e) => {
                let status = match e {
                    Fail::Kern(Error::NoEnt) => STATUS_NO_ENT,
                    Fail::Kern(Error::Privilege { .. }) => STATUS_DENIED,
                    _ => STATUS_ERROR,
                };
                let mut msg = [0u8; 160];
                let n = fmt_into(&mut msg, &e);
                encode_resp(out, c.op, status, c.seq, 0, msg.get(..n).unwrap_or(&[]))
            }
        }
    }

    /// De bevoegde operaties. `&Privilege` is het bewijs.
    #[expect(
        clippy::too_many_arguments,
        reason = "de call brengt verbinding, geheugen en haken mee"
    )]
    async fn privileged(
        &self,
        _proof: &Privilege,
        op: PrivOp,
        c: &Call<'_>,
        reply: &'r Reply,
        mem: &mut impl PhysMem,
        hooks: &impl Hooks,
        out: &mut [u8],
    ) -> Answer {
        let data = out.get_mut(REQ_HEADER..).unwrap_or(&mut []);
        match op {
            PrivOp::StartSlot => self.start(c, reply).await,
            PrivOp::StreamImage => self.stream(c, reply, mem, data).await,
            PrivOp::StopSlot => {
                let slot = target(c)?;
                if let Some(s) = self.take_stream(slot) {
                    return done(slots::call(self.inbox, reply, Request::Abort(s.grant)).await?);
                }
                let timeout = Duration::from_millis(c.n);
                done(slots::call(self.inbox, reply, Request::Stop { slot, timeout }).await?)
            }
            PrivOp::SlotStatus => self.status(c, reply, data).await,
            PrivOp::NextLog => {
                let slot = target(c)?;
                let max = usize::try_from(c.n).unwrap_or(usize::MAX).min(data.len());
                let dst = data.get_mut(..max).unwrap_or(&mut []);
                match self.logs.and_then(|l| l.pop(slot, dst)) {
                    Some(n) => Ok((1, n)),
                    None => Ok((0, 0)),
                }
            }
            PrivOp::SetClock => {
                hooks.set_clock(c.n);
                Ok((0, 0))
            }
            PrivOp::Flip => {
                let sha: &[u8; 32] = c.path.try_into().map_err(|_| Error::TooLarge {
                    len: c.path.len(),
                    max: 32,
                })?;
                hooks.flip(target(c)?, sha)?;
                Ok((0, 0))
            }
        }
    }

    // De stromentabel. Elke toegang is één synchrone lening; wie iets met
    // een stroom wil over een `.await` heen, haalt hem eruit (`take_stream`)
    // en is dan de enige eigenaar.

    /// Loopt er een stroom voor `slot`?
    fn has_stream(&self, slot: Slot) -> bool {
        self.streams
            .borrow()
            .iter()
            .flatten()
            .any(|s| s.slot() == slot)
    }

    /// Is er plaats voor nog een stroom?
    fn has_room(&self) -> bool {
        self.streams.borrow().iter().any(Option::is_none)
    }

    /// Doet `f` op de stroom van `slot`, binnen één lening.
    fn with_stream<R>(&self, slot: Slot, f: impl FnOnce(&mut Stream) -> R) -> Option<R> {
        self.streams
            .borrow_mut()
            .iter_mut()
            .flatten()
            .find(|s| s.slot() == slot)
            .map(f)
    }

    fn take_stream(&self, slot: Slot) -> Option<Stream> {
        self.streams
            .borrow_mut()
            .iter_mut()
            .find(|s| s.as_ref().is_some_and(|s| s.slot() == slot))
            .and_then(Option::take)
    }

    /// Zet een stroom in een vrije plaats; is de tabel vol, dan komt hij
    /// terug.
    fn put_stream(&self, s: Stream) -> Option<Stream> {
        match self.streams.borrow_mut().iter_mut().find(|e| e.is_none()) {
            Some(e) => {
                *e = Some(s);
                None
            }
            None => Some(s),
        }
    }

    /// START_SLOT: de kern kiest een leeg slot, claimt het en houdt de
    /// grant voor de stroom.
    async fn start(&self, c: &Call<'_>, reply: &'r Reply) -> Answer {
        let req = StartReq::decode(&c.as_req())?;
        if req.image_size == 0 {
            return Err(Fail::Place(abi::Error::ImageSize(0)));
        }
        if req.env.len() as u64 > abi::hopabi::CTRL_ENV_MAX {
            return Err(Error::TooLarge {
                len: req.env.len(),
                max: abi::hopabi::CTRL_ENV_MAX as usize,
            }
            .into());
        }
        if req.memory_limit <= ABI_TAIL {
            return Err(Error::PartitionSize {
                size: req.memory_limit,
            }
            .into());
        }
        // Vooraf, zodat een volle tabel niets claimt; de gezaghebbende
        // toets is `put_stream` hieronder, want tussen de twee kan een
        // andere verbinding van Hop een plaats nemen.
        if !self.has_room() {
            return Err(Error::Full { cap: MAX_STREAMS }.into());
        }
        let placement = placement(&req)?;
        let env = try_vec(req.env)?;
        let job = try_vec(req.job)?;
        let slot = self.free_slot(reply).await?;
        let mut spec = StartSpec::new(slot, req.memory_limit, placement);
        spec.job = job;
        let grant = match slots::call(self.inbox, reply, Request::Claim(spec)).await? {
            Response::Granted(g) => g,
            Response::Failed(e) => return Err(e.into()),
            _ => return Err(Error::Busy.into()),
        };
        let region = grant.region();
        let placer = region
            .size
            .checked_sub(ABI_TAIL)
            .filter(|&ram| abi::layout::Tail::new(LINK_BASE, ram).is_some())
            .ok_or(Fail::Kern(Error::PartitionSize { size: region.size }))
            .and_then(|ram| Placer::new(req.image_size, ram));
        let placer = match placer {
            Ok(p) => p,
            Err(e) => {
                let _ = slots::call(self.inbox, reply, Request::Abort(grant)).await;
                return Err(e);
            }
        };
        if let Some(e) = self.logs {
            e.clear(slot);
        }
        if let Some(s) = self.put_stream(Stream { grant, placer, env }) {
            let _ = slots::call(self.inbox, reply, Request::Abort(s.grant)).await;
            return Err(Error::Full { cap: MAX_STREAMS }.into());
        }
        Ok((slot.get() as u64, 0))
    }

    /// Het eerste slot zonder eigenaar. Hop serialiseert zijn starts, dus
    /// tussen de vraag en de claim komt er niemand tussen; komt er toch
    /// iemand, dan weigert de claim met `StillOwned` en faalt de start luid.
    async fn free_slot(&self, reply: &'r Reply) -> Result<Slot> {
        for i in 1..=self.max_slots.min(SLOT_CAP) {
            let Some(slot) = Slot::new(i) else { continue };
            if self.privilege.as_ref().is_some_and(|p| p.slot == slot) || self.has_stream(slot) {
                continue;
            }
            match slots::call(self.inbox, reply, Request::Status(slot)).await? {
                Response::Status(st) if st.occupancy == Occupancy::Empty => return Ok(slot),
                Response::Status(_) => {}
                Response::Failed(e) => return Err(e),
                _ => return Err(Error::Busy),
            }
        }
        Err(Error::Full {
            cap: self.max_slots,
        })
    }

    /// STREAM_IMAGE: bytes naar hun plek; bij de laatste plaatsen en armen.
    async fn stream(
        &self,
        c: &Call<'_>,
        reply: &'r Reply,
        mem: &mut impl PhysMem,
        data: &mut [u8],
    ) -> Answer {
        let slot = target(c)?;
        let (fed, received, size) = self
            .with_stream(slot, |s| {
                let have = s.placer.received();
                let fed = if c.n == have {
                    s.placer.feed(&mut s.grant, mem, c.data)
                } else {
                    Err(Placer::fail(
                        "chunk offset differs from bytes received",
                        c.n,
                        have,
                    ))
                };
                (fed, s.placer.received(), s.placer.size)
            })
            .ok_or(Error::NotOwned { slot: slot.get() })?;
        if let Err(e) = fed {
            // Een geweigerde brok breekt de stroom af: de kern ruimt op.
            if let Some(s) = self.take_stream(slot) {
                let _ = slots::call(self.inbox, reply, Request::Abort(s.grant)).await;
            }
            return Err(e);
        }
        if received < size {
            return answer_stream(data, received, StreamState::More, None);
        }
        let Some(mut s) = self.take_stream(slot) else {
            return Err(Error::NotOwned { slot: slot.get() }.into());
        };
        let placed = s
            .placer
            .finish(&mut s.grant, mem, slot)
            .and_then(|entry| s.put_env(mem).map(|()| entry).map_err(Fail::from));
        let r = match placed {
            Ok(entry) => {
                let grant = s.grant;
                slots::call(self.inbox, reply, Request::Arm { grant, entry }).await
            }
            Err(e) => {
                let _ = slots::call(self.inbox, reply, Request::Abort(s.grant)).await;
                return answer_stream(data, received, StreamState::Failed, Some(e));
            }
        };
        match r.map_err(Fail::from).and_then(done) {
            Ok(_) => answer_stream(data, received, StreamState::Placed, None),
            Err(e) => answer_stream(data, received, StreamState::Failed, Some(e)),
        }
    }

    /// SLOT_STATUS: het grootboek van de actor plus de stand van een stroom.
    async fn status(&self, c: &Call<'_>, reply: &'r Reply, data: &mut [u8]) -> Answer {
        let slot = target(c)?;
        let st = match slots::call(self.inbox, reply, Request::Status(slot)).await? {
            Response::Status(st) => st,
            Response::Failed(e) => return Err(e.into()),
            _ => return Err(Error::Busy.into()),
        };
        let (core, span) = st.core.map_or((0, 0), |(c, s)| (c.get() as u16, s as u16));
        let (received, image_size) = self
            .with_stream(slot, |s| (s.placer.received(), s.placer.size))
            .unwrap_or((0, 0));
        let info = SlotInfo {
            state: match st.occupancy {
                Occupancy::Empty => SlotState::Empty,
                Occupancy::Streaming => SlotState::Streaming,
                Occupancy::Running => SlotState::Running,
                Occupancy::Quarantined => SlotState::Quarantined,
            } as u8,
            core_on: u8::from(st.cage.core_on),
            reserved: 0,
            core,
            span,
            app: st.cage.app,
            exit_code: st.cage.exit_code,
            heartbeat: st.cage.heartbeat,
            ram_size: st.cage.ram_size,
            fault_vec: st.cage.fault_vec,
            fault_esr: st.cage.fault_esr,
            fault_far: st.cage.fault_far,
            partition: st.partition.map_or(0, |p| p.size),
            received,
            image_size,
        };
        let b = info.encode();
        let max = data.len();
        let dst = data
            .get_mut(..b.len())
            .ok_or(Error::TooLarge { len: b.len(), max })?;
        dst.copy_from_slice(&b);
        Ok((0, b.len()))
    }
}

/// Het doelslot van een bevoegde call (`off`).
fn target(c: &Call<'_>) -> Result<Slot> {
    usize::try_from(c.off)
        .ok()
        .and_then(Slot::new)
        .ok_or(Error::SlotRange {
            slot: usize::try_from(c.off).unwrap_or(usize::MAX),
            max: SLOT_CAP,
        })
}

/// De core-vraag van een start.
fn placement(req: &StartReq<'_>) -> Result<Placement> {
    use abi::systemapi::CoreClass as Wire;
    let group = if req.group.is_empty() {
        None
    } else {
        let mut g = GroupName::new();
        for x in req.group {
            g.push(*x).map_err(|_| Error::TooLarge {
                len: req.group.len(),
                max: crate::pool::MAX_GROUP_NAME,
            })?;
        }
        Some(g)
    };
    Ok(Placement {
        group,
        pool_cores: usize::from(req.pool_cores),
        cores: usize::from(req.cores),
        class: match req.core_class {
            Wire::Any => None,
            Wire::Small => Some(CoreClass::Small),
            Wire::Mid => Some(CoreClass::Mid),
            Wire::Big => Some(CoreClass::Big),
        },
    })
}

/// Schrijft het stroom-antwoord in `data`.
fn answer_stream(data: &mut [u8], received: u64, state: StreamState, why: Option<Fail>) -> Answer {
    let mut msg = [0u8; 160];
    let n = why.map_or(0, |e| fmt_into(&mut msg, &e));
    let r = StreamResp {
        received,
        state,
        why: msg.get(..n).unwrap_or(&[]),
    };
    let len = r.encode_data(data)?;
    Ok((received, len))
}

fn done(r: Response) -> Answer {
    match r {
        Response::Done => Ok((0, 0)),
        Response::Failed(e) => Err(e.into()),
        _ => Err(Error::Busy.into()),
    }
}

fn fmt_into(buf: &mut [u8], e: &impl fmt::Display) -> usize {
    struct W<'b>(&'b mut [u8], usize);
    impl fmt::Write for W<'_> {
        fn write_str(&mut self, s: &str) -> fmt::Result {
            for b in s.bytes() {
                if let Some(x) = self.0.get_mut(self.1) {
                    *x = b;
                    self.1 += 1;
                }
            }
            Ok(())
        }
    }
    let mut w = W(buf, 0);
    let _ = fmt::write(&mut w, format_args!("{e}"));
    w.1
}

/// Een toegelaten verbinding; `Drop` geeft de plaats terug.
pub struct Admitted<'a> {
    slot: Slot,
    generation: u32,
    svc: &'a Servicers,
}

impl Admitted<'_> {
    /// Het slot van de peer.
    #[must_use]
    pub fn slot(&self) -> Slot {
        self.slot
    }
}

impl Drop for Admitted<'_> {
    fn drop(&mut self) {
        if let Some(ctl) = self.svc.ctl(self.slot) {
            ctl.drop_conn();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::Region;
    use crate::slots::tests::{Actor, FakeConsole, Obey, actor, s};
    use crate::slots::{Outbox, servicer_task};
    use crate::stage2::tests::SparseMem;
    use crate::testutil::FakeTimer;
    use abi::systemapi::{StreamState, plain_req, stream_req};
    use core::cell::Cell;
    use core::pin::pin;
    use core::task::{Context, Poll, Waker};
    use std::collections::VecDeque;
    use std::vec;
    use std::vec::Vec;

    const MIB: u64 = 1 << 20;

    /// Een verbinding in RAM die per `read` hooguit `chunk` bytes geeft.
    /// Met `hold` is een lege rij geen EOF maar een peer die zwijgt en open
    /// blijft: de half-open verbinding van een geparkeerde app.
    struct Pipe {
        rx: VecDeque<u8>,
        tx: Vec<u8>,
        chunk: usize,
        ip: u32,
        hold: bool,
    }

    impl Pipe {
        fn new(ip: u32, calls: &[Vec<u8>]) -> Pipe {
            let mut rx = Vec::new();
            for c in calls {
                rx.extend(frame(KIND_CALL, c));
            }
            Pipe {
                rx: rx.into(),
                tx: Vec::new(),
                chunk: 7,
                ip,
                hold: false,
            }
        }

        /// Een peer die zwijgt en nooit sluit.
        fn silent(ip: u32) -> Pipe {
            Pipe {
                hold: true,
                ..Pipe::new(ip, &[])
            }
        }
    }

    impl Conn for Pipe {
        fn read(&mut self, buf: &mut [u8]) -> impl Future<Output = Result<usize>> {
            let n = buf.len().min(self.chunk).min(self.rx.len());
            for b in buf.iter_mut().take(n) {
                *b = self.rx.pop_front().unwrap();
            }
            let wait = n == 0 && self.hold && !buf.is_empty();
            core::future::poll_fn(move |_| {
                if wait {
                    Poll::Pending
                } else {
                    Poll::Ready(Ok(n))
                }
            })
        }
        fn write(&mut self, buf: &[u8]) -> impl Future<Output = Result<usize>> {
            self.tx.extend_from_slice(buf);
            core::future::ready(Ok(buf.len()))
        }
        fn remote_ip4(&self) -> u32 {
            self.ip
        }
    }

    fn frame(kind: u8, p: &[u8]) -> Vec<u8> {
        let mut v = Vec::new();
        v.extend_from_slice(&MAGIC.to_le_bytes());
        v.extend_from_slice(&[VERSION, kind, 0, 0]);
        v.extend_from_slice(&(p.len() as u32).to_le_bytes());
        v.extend_from_slice(p);
        v
    }

    fn enc(r: &abi::hopabi::Req<'_>) -> Vec<u8> {
        let mut v = vec![0u8; REQ_HEADER + r.path.len() + r.data.len()];
        let n = abi::hopabi::encode_req(&mut v, r).unwrap();
        v.truncate(n);
        v
    }

    fn start_call(seq: u32, image_size: u64, env: &[u8]) -> Vec<u8> {
        let r = StartReq {
            memory_limit: 8 * MIB,
            image_size,
            cores: 1,
            pool_cores: 1,
            core_class: abi::systemapi::CoreClass::Any,
            group: b"",
            env,
            job: b"demo",
        };
        let mut v = vec![0u8; 512];
        let n = r.encode(&mut v, seq).unwrap();
        v.truncate(n);
        v
    }

    fn op(o: PrivOp, seq: u32, slot: u64, n: u64) -> Vec<u8> {
        enc(&plain_req(o, seq, slot, n))
    }

    /// Een antwoord: (op, status, seq, size, data).
    type Res = (u8, u16, u32, u64, Vec<u8>);

    fn results(mut b: &[u8]) -> Vec<Res> {
        let mut out = Vec::new();
        while b.len() >= HEADER_LEN {
            assert_eq!(b[5], KIND_RESULT);
            let n = u32::from_le_bytes(b[8..12].try_into().unwrap()) as usize;
            let r = abi::hopabi::decode_resp(&b[HEADER_LEN..HEADER_LEN + n]).unwrap();
            out.push((r.op, r.status, r.seq, r.size, r.data.to_vec()));
            b = &b[HEADER_LEN + n..];
        }
        out
    }

    fn stream_state(r: &Res) -> StreamState {
        let resp = abi::hopabi::Resp {
            size: r.3,
            data: &r.4,
            ..Default::default()
        };
        StreamResp::decode(&resp).unwrap().state
    }

    #[derive(Default)]
    struct NoHooks(Cell<u64>, Cell<Option<(usize, [u8; 32])>>);
    impl Hooks for NoHooks {
        fn set_clock(&self, unix_ns: u64) {
            self.0.set(unix_ns);
        }
        fn flip(&self, bundle: Slot, sha256: &[u8; 32]) -> Result {
            self.1.set(Some((bundle.get(), *sha256)));
            Ok(())
        }
    }

    /// Een outbox die één regel meldt en daarna leeft tot de stop.
    struct OneLine(Option<&'static [u8]>);
    impl Outbox for OneLine {
        fn read_into(&mut self, buf: &mut [u8]) -> Option<(u8, usize)> {
            let l = self.0.take()?;
            buf[..l.len()].copy_from_slice(l);
            Some((crate::slots::KIND_LOG, l.len()))
        }
        fn corrupt(&self) -> bool {
            false
        }
        fn live(&self) -> bool {
            true
        }
        fn smp_pending(&self) -> bool {
            false
        }
    }

    // Het test-image: één PT_LOAD op LINK_BASE + 0x10000, de symbolen in
    // het segment, de symbool- en stringtabel en de sectieheaders erachter.
    const SEG_OFF: usize = 0x1000;
    const SEG_FILESZ: usize = 0x100;
    const SEG_MEMSZ: u64 = 0x200;
    const SEG_IPA: u64 = LINK_BASE + 0x10000;

    fn put(b: &mut [u8], at: usize, v: u64, n: usize) {
        b[at..at + n].copy_from_slice(&v.to_le_bytes()[..n]);
    }

    fn elf(stamp: u64) -> Vec<u8> {
        let mut img = vec![0u8; SEG_OFF + SEG_FILESZ];
        img[..4].copy_from_slice(b"\x7fELF");
        img[4..7].copy_from_slice(&[2, 1, 1]);
        put(&mut img, 16, 2, 2);
        put(&mut img, 18, u64::from(leanelf::MACHINE_AARCH64), 2);
        put(&mut img, 20, 1, 4);
        put(&mut img, 24, SEG_IPA, 8);
        put(&mut img, 32, 64, 8);
        put(&mut img, 52, 64, 2);
        put(&mut img, 54, 56, 2);
        put(&mut img, 56, 1, 2);
        put(&mut img, 58, 64, 2);
        put(&mut img, 60, 3, 2);
        let ph = 64;
        put(&mut img, ph, 1, 4);
        put(&mut img, ph + 4, 7, 4);
        put(&mut img, ph + 8, SEG_OFF as u64, 8);
        put(&mut img, ph + 16, SEG_IPA, 8);
        put(&mut img, ph + 24, SEG_IPA, 8);
        put(&mut img, ph + 32, SEG_FILESZ as u64, 8);
        put(&mut img, ph + 40, SEG_MEMSZ, 8);
        put(&mut img, ph + 48, 0x1000, 8);
        for (i, b) in img[SEG_OFF..].iter_mut().enumerate() {
            *b = 0xA0 | (i as u8 & 0xF);
        }
        img[SEG_OFF + 0x80..SEG_OFF + 0x90].fill(0);
        put(&mut img, SEG_OFF + 0x90, stamp, 8);
        // De stringtabel en de symbolen.
        let names = [place::SYM_RAM_START, place::SYM_RAM_SIZE, place::SYM_ABI];
        let mut strtab = vec![0u8];
        let mut name_at = Vec::new();
        for n in names {
            name_at.push(strtab.len() as u64);
            strtab.extend_from_slice(n.as_bytes());
            strtab.push(0);
        }
        let symtab_off = img.len();
        img.extend_from_slice(&[0u8; 24]);
        for (i, at) in name_at.iter().enumerate() {
            let mut e = [0u8; 24];
            put(&mut e, 0, *at, 4);
            e[4] = 0x11;
            put(&mut e, 6, 1, 2);
            put(&mut e, 8, SEG_IPA + 0x80 + 8 * i as u64, 8);
            put(&mut e, 16, 8, 8);
            img.extend_from_slice(&e);
        }
        let symtab_len = img.len() - symtab_off;
        let strtab_off = img.len();
        img.extend_from_slice(&strtab);
        while !img.len().is_multiple_of(8) {
            img.push(0);
        }
        let shoff = img.len();
        img.extend_from_slice(&[0u8; 64]);
        for (kind, off, len, link, ent) in [
            (2u64, symtab_off, symtab_len, 2u64, 24u64),
            (3, strtab_off, strtab.len(), 0, 0),
        ] {
            let mut e = [0u8; 64];
            put(&mut e, 4, kind, 4);
            put(&mut e, 24, off as u64, 8);
            put(&mut e, 32, len as u64, 8);
            put(&mut e, 40, link, 4);
            put(&mut e, 56, ent, 8);
            img.extend_from_slice(&e);
        }
        put(&mut img, 40, shoff as u64, 8);
        img
    }

    /// Een servicer-taak die over meerdere verbindingen heen leeft.
    type Servicer<'a> = core::pin::Pin<std::boxed::Box<dyn Future<Output = ()> + 'a>>;

    /// Dient `p` tot EOF, met de actor en (als die er is) een servicer
    /// ernaast. Geeft de uitkomst van `serve`.
    #[expect(clippy::too_many_arguments, reason = "de hele kern van een test")]
    fn drive<'i, 'r, const N: usize>(
        sys: &System<'i, 'r, N>,
        a: &mut Actor<'_>,
        inbox: &'i Mailbox<Envelope<'r>, N>,
        reply: &'r Reply,
        p: &mut Pipe,
        mem: &mut SparseMem,
        hooks: &NoHooks,
        tee: &LogTee<'_, &FakeConsole>,
        mut svc: Option<&mut Servicer<'_>>,
    ) -> End {
        let who = sys.admit(p.ip).unwrap();
        let timer = FakeTimer::default();
        let (mut buf, mut out) = (vec![0u8; 64 << 10], vec![0u8; 8192]);
        let mut serve =
            pin!(sys.serve(p, &who, reply, &timer, mem, hooks, tee, &mut buf, &mut out));
        let mut run = pin!(a.run(inbox));
        let mut cx = Context::from_waker(Waker::noop());
        for _ in 0..100_000 {
            let _ = run.as_mut().poll(&mut cx);
            if let Some(f) = svc.as_mut() {
                let _ = f.as_mut().poll(&mut cx);
            }
            if let Poll::Ready(r) = serve.as_mut().poll(&mut cx) {
                return r;
            }
        }
        panic!("serve did not finish");
    }

    /// Een node met Hop in slot 1 en een gewone app in slot 2, beide levend.
    fn node<'s>(svc: &'s Servicers, con: &'s FakeConsole) -> Actor<'s> {
        let mut a = actor(svc, con, Obey::Exit, 64, 4);
        crate::slots::tests::start(&mut a, 1, 8, 1).unwrap();
        crate::slots::tests::start(&mut a, 2, 8, 1).unwrap();
        a
    }

    #[test]
    fn slot_from_remote_maps_the_internal_net() {
        let ip = |d: u32| NET | d;
        assert_eq!(slot_from_remote(ip(2), 8), Some(s(1)));
        assert_eq!(slot_from_remote(ip(9), 8), Some(s(8)));
        assert_eq!(slot_from_remote(ip(10), 8), None, "beyond max slots");
        assert_eq!(slot_from_remote(ip(1), 8), None, "HOP itself");
        assert_eq!(slot_from_remote(ip(0), 8), None);
        assert_eq!(
            slot_from_remote((10 << 24) | (101 << 16) | 2, 8),
            None,
            "other net"
        );
    }

    #[test]
    fn privilege_is_minted_once() {
        // Het token van deze test-binary: hooguit één keer.
        let a = Privilege::boot(s(1));
        let b = Privilege::boot(s(2));
        assert!(a.is_none() || b.is_none());
    }

    #[test]
    fn frames_survive_fragmentation_and_oversize_is_refused() {
        let big = vec![0xa5u8; MAX_IO_CHUNK];
        let mut p = Pipe {
            rx: frame(KIND_CALL, &big).into(),
            tx: Vec::new(),
            chunk: 1,
            ip: 0,
            hold: false,
        };
        let (kind, n) = crate::testutil::block_on(read_header(&mut p)).unwrap();
        assert_eq!((kind, n), (KIND_CALL, MAX_IO_CHUNK));
        let mut w = Pipe::new(0, &[]);
        let too_big = vec![0u8; MAX_PAYLOAD + 1];
        assert!(crate::testutil::block_on(write_frame(&mut w, KIND_CALL, &too_big)).is_err());
        assert!(w.tx.is_empty(), "oversized frame half written");
    }

    /// De hele levensloop: start (de kern kiest slot 3), het image in drie
    /// brokken, geplaatst en gestart, status, een logregel, de klok, stop;
    /// en slot 2 krijgt voor elke bevoegde op "denied".
    #[test]
    fn hop_drives_the_lifecycle_end_to_end() {
        let (svc, con, logs) = (Servicers::new(), FakeConsole::default(), SlotLogs::new());
        let tee = LogTee::new(&con, &logs);
        let mut a = node(&svc, &con);
        let reply = Reply::new();
        let inbox: Mailbox<Envelope<'_>, 8> = Mailbox::new();
        let sys = System::new(&inbox, &svc, Some(Privilege::for_test(s(1))), 8).with_logs(&logs);
        let timer = FakeTimer::default();
        let mut sbuf = [0u8; 256];
        let mut servicer: Servicer<'_> = std::boxed::Box::pin(servicer_task(
            s(3),
            &svc,
            |_: Region| OneLine(Some(b"app says hi")),
            &timer,
            &tee,
            &inbox,
            &mut sbuf,
        ));
        let img = elf(u64::from(abi::ABI_VERSION));
        let (c1, c2) = (0x50, SEG_OFF + 0x80);
        let env = b"BUCKET=hop-apps\n";
        let mut p = Pipe::new(
            NET | 2,
            &[
                start_call(1, img.len() as u64, env),
                enc(&stream_req(2, 3, 0, &img[..c1])),
                enc(&stream_req(3, 3, c1 as u64, &img[c1..c2])),
                enc(&stream_req(4, 3, c2 as u64, &img[c2..])),
                op(PrivOp::SlotStatus, 5, 3, 0),
                op(PrivOp::NextLog, 6, 3, 64),
                op(PrivOp::NextLog, 7, 3, 64),
                op(PrivOp::SetClock, 8, 0, 1_759_000_000),
            ],
        );
        let (mut mem, hooks) = (SparseMem::default(), NoHooks::default());
        let r = drive(
            &sys,
            &mut a,
            &inbox,
            &reply,
            &mut p,
            &mut mem,
            &hooks,
            &tee,
            Some(&mut servicer),
        );
        assert_eq!(r, End::Peer, "stream should end at EOF");
        let res = results(&p.tx);
        assert_eq!(res.len(), 8);
        assert_eq!(
            (res[0].1, res[0].3),
            (STATUS_OK, 3),
            "the kern picks slot 3"
        );
        assert_eq!(stream_state(&res[1]), StreamState::More);
        assert_eq!(res[1].3, c1 as u64);
        assert_eq!(stream_state(&res[2]), StreamState::More);
        assert_eq!(stream_state(&res[3]), StreamState::Placed, "{:?}", res[3]);
        assert_eq!(res[3].3, img.len() as u64);
        let info = SlotInfo::decode(&res[4].4).unwrap();
        assert_eq!(info.slot_state(), Some(SlotState::Running));
        assert_eq!(info.partition, 8 * MIB);
        assert_eq!((res[5].3, &res[5].4[..]), (1, &b"app says hi"[..]));
        assert_eq!((res[6].3, res[6].4.len()), (0, 0), "log ring drained");
        assert_eq!(hooks.0.get(), 1_759_000_000);

        // Het image staat op zijn plek, gepatcht, met de env op de page.
        let part = a.status(s(3)).partition.unwrap();
        assert_eq!(a.status(s(3)).occupancy, Occupancy::Running);
        let app_ram = part.size - ABI_TAIL;
        let at = |ipa: u64| mem.read64(part.base + (ipa - LINK_BASE));
        assert_eq!(
            at(SEG_IPA),
            u64::from_le_bytes(img[SEG_OFF..SEG_OFF + 8].try_into().unwrap())
        );
        assert_eq!(at(SEG_IPA + 0x80), LINK_BASE, "RamStart patched");
        assert_eq!(at(SEG_IPA + 0x88), app_ram, "RamSize patched");
        assert_eq!(at(SEG_IPA + 0x90), u64::from(abi::ABI_VERSION));
        let ctrl = part.base + app_ram + ABI_CTRL_OFF;
        assert_eq!(
            mem.read64(ctrl + abi::hopabi::CTRL_ENV_LEN),
            env.len() as u64
        );
        let mut got = [0u8; 16];
        read_bytes(&mem, ctrl + abi::hopabi::CTRL_ENV_DATA, &mut got);
        assert_eq!(&got, env);
        // Buiten het segment en de control-page staat niets: BSS en de
        // schrapruimte met de symbooltabel zijn gewist.
        let seg = part.base + (SEG_IPA - LINK_BASE);
        assert!(
            mem.0.keys().all(|&k| k < part.base
                || k >= part.base + part.size
                || (seg..seg + SEG_FILESZ as u64).contains(&k)
                || (ctrl..ctrl + 0x1000).contains(&k)),
            "residue in the partition"
        );

        // Slot 2 is niet bevoegd: stop, start en status worden geweigerd.
        let mut other = Pipe::new(
            NET | 3,
            &[
                op(PrivOp::StopSlot, 9, 3, 10),
                start_call(10, 16, b""),
                op(PrivOp::SlotStatus, 11, 3, 0),
            ],
        );
        let _ = drive(
            &sys, &mut a, &inbox, &reply, &mut other, &mut mem, &hooks, &tee, None,
        );
        assert!(results(&other.tx).iter().all(|r| r.1 == STATUS_DENIED));
        assert_eq!(a.status(s(3)).occupancy, Occupancy::Running);

        // Hop stopt slot 3: de kern bevestigt en geeft vrij.
        let mut stop = Pipe::new(
            NET | 2,
            &[
                op(PrivOp::StopSlot, 12, 3, 50),
                op(PrivOp::SlotStatus, 13, 3, 0),
            ],
        );
        let _ = drive(
            &sys,
            &mut a,
            &inbox,
            &reply,
            &mut stop,
            &mut mem,
            &hooks,
            &tee,
            Some(&mut servicer),
        );
        let res = results(&stop.tx);
        assert_eq!(res[0].1, STATUS_OK, "{:?}", core::str::from_utf8(&res[0].4));
        let info = SlotInfo::decode(&res[1].4).unwrap();
        assert_eq!(info.slot_state(), Some(SlotState::Empty));
    }

    /// Een kapot image, een verkeerde ABI-stempel, een verkeerde offset en
    /// een stop halverwege: telkens ruimt de kern het slot zelf op.
    #[test]
    fn failed_streams_leave_nothing_behind() {
        let (svc, con, logs) = (Servicers::new(), FakeConsole::default(), SlotLogs::new());
        let tee = LogTee::new(&con, &logs);
        let mut a = node(&svc, &con);
        let reply = Reply::new();
        let inbox: Mailbox<Envelope<'_>, 8> = Mailbox::new();
        let sys = System::new(&inbox, &svc, Some(Privilege::for_test(s(1))), 8);
        let bad_abi = elf(10);
        let good = elf(u64::from(abi::ABI_VERSION));
        let mut p = Pipe::new(
            NET | 2,
            &[
                // Geen ELF: de eerste brok wordt al geweigerd.
                start_call(1, 16, b""),
                enc(&stream_req(2, 3, 0, b"not an elf image")),
                // ABI 10: alle bytes binnen, de plaatsing weigert.
                start_call(3, bad_abi.len() as u64, b""),
                enc(&stream_req(4, 3, 0, &bad_abi)),
                // Een brok op de verkeerde offset.
                start_call(5, good.len() as u64, b""),
                enc(&stream_req(6, 3, 8, &good[..8])),
                // Stop halverwege.
                start_call(7, good.len() as u64, b""),
                enc(&stream_req(8, 3, 0, &good[..100])),
                op(PrivOp::StopSlot, 9, 3, 10),
                enc(&stream_req(10, 3, 100, &good[100..])),
                // Te veel bytes.
                start_call(11, 4, b""),
                enc(&stream_req(12, 3, 0, b"\x7fELF!")),
                op(PrivOp::SlotStatus, 13, 3, 0),
            ],
        );
        let (mut mem, hooks) = (SparseMem::default(), NoHooks::default());
        let _ = drive(
            &sys, &mut a, &inbox, &reply, &mut p, &mut mem, &hooks, &tee, None,
        );
        let res = results(&p.tx);
        let text = |r: &Res| std::string::String::from_utf8_lossy(&r.4).into_owned();
        assert_eq!(res[0].3, 3);
        assert_eq!(res[1].1, STATUS_ERROR);
        assert!(text(&res[1]).contains("not an ELF"), "{}", text(&res[1]));
        assert_eq!(res[2].3, 3, "slot 3 was given back");
        assert_eq!(res[3].1, STATUS_OK);
        assert_eq!(stream_state(&res[3]), StreamState::Failed);
        assert!(text(&res[3]).contains("ABI"), "{}", text(&res[3]));
        assert_eq!(res[5].1, STATUS_ERROR, "offset mismatch");
        assert_eq!(
            (res[6].1, res[7].1, res[8].1),
            (STATUS_OK, STATUS_OK, STATUS_OK)
        );
        assert_eq!(res[9].1, STATUS_ERROR, "stream after stop");
        assert_eq!(res[11].1, STATUS_ERROR, "more bytes than announced");
        let info = SlotInfo::decode(&res[12].4).unwrap();
        assert_eq!(info.slot_state(), Some(SlotState::Empty));
        assert_eq!(a.status(s(3)).occupancy, Occupancy::Empty);
    }

    #[test]
    fn flip_passes_slot_and_sha_to_the_hook() {
        let (svc, con, logs) = (Servicers::new(), FakeConsole::default(), SlotLogs::new());
        let tee = LogTee::new(&con, &logs);
        let mut a = node(&svc, &con);
        let reply = Reply::new();
        let inbox: Mailbox<Envelope<'_>, 8> = Mailbox::new();
        let sys = System::new(&inbox, &svc, Some(Privilege::for_test(s(1))), 8);
        let sha = [7u8; 32];
        let flip = abi::hopabi::Req {
            op: PrivOp::Flip.op(),
            seq: 1,
            off: 5,
            path: &sha,
            ..Default::default()
        };
        let short = abi::hopabi::Req {
            path: &sha[..31],
            ..flip
        };
        let mut p = Pipe::new(NET | 2, &[enc(&flip), enc(&short)]);
        let (mut mem, hooks) = (SparseMem::default(), NoHooks::default());
        let _ = drive(
            &sys, &mut a, &inbox, &reply, &mut p, &mut mem, &hooks, &tee, None,
        );
        let res = results(&p.tx);
        assert_eq!((res[0].1, res[1].1), (STATUS_OK, STATUS_ERROR));
        assert_eq!(hooks.1.get(), Some((5, sha)));
    }

    #[test]
    fn log_ring_drops_the_oldest_and_truncates() {
        let logs = SlotLogs::new();
        let mut buf = [0u8; LOG_LINE_MAX + 8];
        assert_eq!(logs.pop(s(4), &mut buf), None);
        let long = [b'x'; LOG_LINE_MAX + 50];
        logs.push(s(4), &long);
        assert_eq!(logs.pop(s(4), &mut buf), Some(LOG_LINE_MAX));
        for i in 0..100u8 {
            logs.push(s(4), &[i; 40]);
        }
        // De ring houdt de nieuwste regels; de oudste zijn eruit.
        let mut seen = Vec::new();
        while let Some(n) = logs.pop(s(4), &mut buf) {
            assert_eq!(n, 40);
            seen.push(buf[0]);
        }
        assert_eq!(*seen.last().unwrap(), 99);
        assert_eq!(seen.len(), LOG_RING_BYTES / 42);
        assert!(seen.windows(2).all(|w| w[1] == w[0] + 1));
        // Afkappen op de maat van de lezer.
        logs.push(s(4), b"abcdef");
        assert_eq!(logs.pop(s(4), &mut buf[..3]), Some(3));
        assert_eq!(&buf[..3], b"abc");
        logs.push(s(5), b"weg");
        logs.clear(s(5));
        assert_eq!(logs.pop(s(5), &mut buf), None);
    }

    #[test]
    fn admission_caps_connections_per_lifetime() {
        let (svc, con) = (Servicers::new(), FakeConsole::default());
        let mut a = actor(&svc, &con, Obey::Exit, 64, 4);
        crate::slots::tests::start(&mut a, 1, 8, 1).unwrap();
        let inbox: Mailbox<Envelope<'_>, 2> = Mailbox::new();
        let sys = System::new(&inbox, &svc, None, 8);
        let one = sys.admit(NET | 2).unwrap();
        let two = sys.admit(NET | 2).unwrap();
        assert!(sys.admit(NET | 2).is_none(), "third connection admitted");
        // Een weigering kost geen plaats: ook de vierde blijft buiten.
        assert!(sys.admit(NET | 2).is_none(), "a refusal gave a seat back");
        assert_eq!(svc.ctl(s(1)).unwrap().conns(), 2);
        drop(one);
        assert!(sys.admit(NET | 2).is_some());
        drop(two);
        assert!(
            sys.admit(NET | 3).is_none(),
            "slot without servicer admitted"
        );
    }

    /// Een lege omgeving van één verbinding: buffers, antwoordplek, geheugen.
    struct Seat {
        buf: Vec<u8>,
        out: Vec<u8>,
        reply: Reply,
        mem: SparseMem,
    }

    impl Seat {
        fn new() -> Seat {
            Seat {
                buf: vec![0u8; 4096],
                out: vec![0u8; 4096],
                reply: Reply::new(),
                mem: SparseMem::default(),
            }
        }
    }

    /// Het gebrek van 29-09: een half-open verbinding (de app parkeerde en
    /// zond nooit zijn FIN) mag een andere verbinding niet ophouden. Hop in
    /// slot 1 krijgt zijn antwoorden terwijl slot 2 zwijgt en openstaat.
    #[test]
    fn a_silent_connection_does_not_hold_up_another() {
        let (svc, con, logs) = (Servicers::new(), FakeConsole::default(), SlotLogs::new());
        let tee = LogTee::new(&con, &logs);
        let mut a = node(&svc, &con);
        // De antwoordplekken leven langer dan de inbox die ernaar wijst.
        let (mut s1, mut s2) = (Seat::new(), Seat::new());
        let inbox: Mailbox<Envelope<'_>, 8> = Mailbox::new();
        let sys = System::new(&inbox, &svc, Some(Privilege::for_test(s(1))), 8).with_logs(&logs);
        let (timer, hooks) = (FakeTimer::default(), NoHooks::default());
        let mut quiet = Pipe::silent(NET | 3);
        let mut hop = Pipe::new(
            NET | 2,
            &[
                op(PrivOp::SlotStatus, 1, 2, 0),
                op(PrivOp::SetClock, 2, 0, 42),
            ],
        );
        let (w1, w2) = (sys.admit(quiet.ip).unwrap(), sys.admit(hop.ip).unwrap());
        let mut done = None;
        {
            let mut f1 = pin!(sys.serve(
                &mut quiet,
                &w1,
                &s1.reply,
                &timer,
                &mut s1.mem,
                &hooks,
                &tee,
                &mut s1.buf,
                &mut s1.out,
            ));
            let mut f2 = pin!(sys.serve(
                &mut hop,
                &w2,
                &s2.reply,
                &timer,
                &mut s2.mem,
                &hooks,
                &tee,
                &mut s2.buf,
                &mut s2.out,
            ));
            let mut run = pin!(a.run(&inbox));
            let mut cx = Context::from_waker(Waker::noop());
            for _ in 0..1000 {
                let _ = run.as_mut().poll(&mut cx);
                assert!(f1.as_mut().poll(&mut cx).is_pending(), "silent peer closed");
                if let Poll::Ready(e) = f2.as_mut().poll(&mut cx) {
                    done = Some(e);
                    break;
                }
            }
            assert!(f1.as_mut().poll(&mut cx).is_pending());
            assert_eq!(
                svc.ctl(s(2)).unwrap().conns(),
                1,
                "silent peer lost its seat"
            );
        }
        assert_eq!(done, Some(End::Peer));
        let res = results(&hop.tx);
        assert_eq!(res.len(), 2);
        let info = SlotInfo::decode(&res[0].4).unwrap();
        assert_eq!(info.slot_state(), Some(SlotState::Running));
        assert_eq!((res[1].1, hooks.0.get()), (STATUS_OK, 42));
    }

    /// Een stop sluit elke verbinding van het slot, ook twee die zwijgen, en
    /// geeft hun plaatsen terug; daartussen houdt de cap de derde buiten.
    #[test]
    fn a_stop_closes_every_connection_of_the_slot() {
        let (svc, con, logs) = (Servicers::new(), FakeConsole::default(), SlotLogs::new());
        let tee = LogTee::new(&con, &logs);
        let mut a = node(&svc, &con);
        let (mut s1, mut s2) = (Seat::new(), Seat::new());
        let inbox: Mailbox<Envelope<'_>, 8> = Mailbox::new();
        let sys = System::new(&inbox, &svc, None, 8);
        let (timer, hooks) = (FakeTimer::default(), NoHooks::default());
        let (mut p1, mut p2) = (Pipe::silent(NET | 3), Pipe::silent(NET | 3));
        let (w1, w2) = (sys.admit(NET | 3).unwrap(), sys.admit(NET | 3).unwrap());
        assert!(sys.admit(NET | 3).is_none(), "third connection admitted");
        let mut ends = [None, None];
        {
            let mut f1 = pin!(sys.serve(
                &mut p1,
                &w1,
                &s1.reply,
                &timer,
                &mut s1.mem,
                &hooks,
                &tee,
                &mut s1.buf,
                &mut s1.out,
            ));
            let mut f2 = pin!(sys.serve(
                &mut p2,
                &w2,
                &s2.reply,
                &timer,
                &mut s2.mem,
                &hooks,
                &tee,
                &mut s2.buf,
                &mut s2.out,
            ));
            let mut cx = Context::from_waker(Waker::noop());
            for _ in 0..100 {
                assert!(f1.as_mut().poll(&mut cx).is_pending());
                assert!(f2.as_mut().poll(&mut cx).is_pending());
            }
            let stop = Request::Stop {
                slot: s(2),
                timeout: Duration::from_millis(50),
            };
            let r = crate::testutil::block_on(a.handle(stop));
            assert!(matches!(r, Response::Done), "stop not confirmed");
            let [e1, e2] = &mut ends;
            for _ in 0..10 {
                for (f, e) in [(f1.as_mut(), &mut *e1), (f2.as_mut(), &mut *e2)] {
                    if e.is_none()
                        && let Poll::Ready(x) = f.poll(&mut cx)
                    {
                        *e = Some(x);
                    }
                }
            }
        }
        assert_eq!(ends, [Some(End::Evicted), Some(End::Evicted)]);
        drop((w1, w2));
        assert_eq!(svc.ctl(s(2)).unwrap().conns(), 0, "seats not given back");
        assert!(sys.admit(NET | 3).is_none(), "stopped slot admitted");
    }

    /// Een verbinding zonder verkeer van een levend slot sluit na
    /// [`IDLE_TIMEOUT`], niet eerder.
    #[test]
    fn a_silent_connection_of_a_live_slot_times_out() {
        let (svc, con, logs) = (Servicers::new(), FakeConsole::default(), SlotLogs::new());
        let tee = LogTee::new(&con, &logs);
        let _a = node(&svc, &con);
        let mut seat = Seat::new();
        let inbox: Mailbox<Envelope<'_>, 8> = Mailbox::new();
        let sys = System::new(&inbox, &svc, None, 8);
        let (timer, hooks) = (FakeTimer::default(), NoHooks::default());
        let mut p = Pipe::silent(NET | 3);
        let who = sys.admit(NET | 3).unwrap();
        let end = crate::testutil::block_on(sys.serve(
            &mut p,
            &who,
            &seat.reply,
            &timer,
            &mut seat.mem,
            &hooks,
            &tee,
            &mut seat.buf,
            &mut seat.out,
        ));
        assert_eq!(end, End::Idle);
        let idle = IDLE_TIMEOUT.as_nanos() as u64;
        let now = timer.now.get();
        assert!(
            (idle..idle + 2 * LIFE_TICK.as_nanos() as u64).contains(&now),
            "closed after {now} ns"
        );
    }
}
