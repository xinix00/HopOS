//! De system-API: het control- en datakanaal van een app naar HOP, over het
//! gewone interne LAN.
//!
//! Netwerkisolatie bepaalt de peer, de peer bepaalt het slot (het bron-IP,
//! [`slot_from_remote`]) en de levende servicer van dat slot bepaalt de
//! levensduur. Er is geen tweede identiteitspad.
//!
//! Bovenop de gewone calls staan de BEVOEGDE operaties ([`OP_START_SLOT`] en
//! verder): de lifecycle besturen, een image streamen, de klok zetten, de
//! kern flippen. Alleen het slot met de [`Privilege`] mag ze: dat is Hop,
//! de eerste bewoner (PORT.md beslissing 1). De kern maakt het token één
//! keer bij boot en geeft het aan het slot van Hop; nergens anders bestaat
//! er een. Een privileged operatie is een functie die `&Privilege` eist:
//! bewijs als parameter (handboek §2.1), geen vlag die je vergeet te toetsen.
//!
//! De TCP-verbinding komt van `leannet`, over de [`Conn`]-trait.

use crate::cage::{Console, CoreClass, PhysMem};
use crate::pool::{GroupName, Placement};
use crate::slots::{
    self, Envelope, ImageGrant, Occupancy, Reply, Request, Response, Servicers, StartSpec,
};
use crate::{Error, Result, SLOT_CAP, Slot};
use core::future::Future;
use core::sync::atomic::{AtomicBool, Ordering::AcqRel};
use core::time::Duration;
use sync::mpsc::Mailbox;

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
    // De bevoegde ops liggen boven de gewone en botsen niet met de
    // (anders ingevulde) `abi::systemapi::PrivOp` vanaf 0x40.
    assert!(OP_START_SLOT > abi::hopabi::OP_MAX && OP_ARM_SLOT < 0x40);
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

// De bevoegde operaties. VOORLOPIG: deze nummers horen in `abi::hopabi` en
// verhuizen daarheen zodra die crate bouwt; tot dan zijn ze hier de waarheid.
/// Claim een slot: `off` = slot, `n` = memory_limit, data = [`encode_placement`].
pub const OP_START_SLOT: u8 = 32;
/// Stop een slot: `off` = slot, `n` = time-out in ms. Een lopende stream wordt
/// afgebroken (de grant komt terug).
pub const OP_STOP_SLOT: u8 = 33;
/// De status van een slot: `off` = slot.
pub const OP_SLOT_STATUS: u8 = 34;
/// Schrijf image-bytes: `off` = slot, `n` = offset in de partitie, data = bytes.
pub const OP_STREAM_IMAGE: u8 = 35;
/// Zet de klok: `n` = Unix-nanoseconden.
pub const OP_SET_CLOCK: u8 = 36;
/// Flip de kern: data = de bundel-URL, `path` = de verwachte SHA-256.
pub const OP_FLIP: u8 = 37;
/// Wapen een geclaimd slot: `off` = slot, `n` = startadres.
pub const OP_ARM_SLOT: u8 = 38;

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
pub trait Hooks {
    /// Zet de klok (Unix-nanoseconden).
    fn set_clock(&mut self, unix_ns: u64);
    /// Start een kern-flip naar deze bundel.
    fn flip(&mut self, url: &[u8], sha256: &[u8]) -> Result;
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
}

/// Schrijft een antwoordkop in `out` gevolgd door `data`; geeft de lengte.
pub fn encode_resp(out: &mut [u8], op: u8, status: u16, seq: u32, size: u64, data: &[u8]) -> usize {
    let n = (REQ_HEADER + data.len()).min(out.len());
    if let Some(h) = out.get_mut(..REQ_HEADER) {
        h[0] = ABI_VERSION;
        h[1] = op;
        h[2..4].copy_from_slice(&status.to_le_bytes());
        h[4..8].copy_from_slice(&seq.to_le_bytes());
        h[8..16].copy_from_slice(&size.to_le_bytes());
        h[16..24].fill(0);
    }
    if let (Some(d), Some(s)) = (
        out.get_mut(REQ_HEADER..n),
        data.get(..n.saturating_sub(REQ_HEADER)),
    ) {
        d.copy_from_slice(s);
    }
    n
}

/// Codeert een core-vraag voor [`OP_START_SLOT`]: cores, klasse
/// (0 geen, 1 small, 2 mid, 3 big), poolgrootte, en de groepsnaam.
pub fn encode_placement(p: &Placement, out: &mut [u8]) -> usize {
    let class = match p.class {
        None => 0,
        Some(CoreClass::Small) => 1,
        Some(CoreClass::Mid) => 2,
        Some(CoreClass::Big) => 3,
    };
    let name = p.group.as_ref().map_or(&[][..], |g| g.as_slice());
    let n = (3 + name.len()).min(out.len());
    if let Some(h) = out.get_mut(..3) {
        h[0] = p.cores.min(255) as u8;
        h[1] = class;
        h[2] = p.pool_cores.min(255) as u8;
    }
    if let (Some(d), Some(s)) = (out.get_mut(3..n), name.get(..n.saturating_sub(3))) {
        d.copy_from_slice(s);
    }
    n
}

fn decode_placement(b: &[u8]) -> Result<Placement> {
    let (&cores, &class, &pool) = match b {
        [a, c, p, ..] => (a, c, p),
        _ => return Err(Error::Corrupt { at: b.len() }),
    };
    let class = match class {
        0 => None,
        1 => Some(CoreClass::Small),
        2 => Some(CoreClass::Mid),
        3 => Some(CoreClass::Big),
        _ => return Err(Error::Corrupt { at: 1 }),
    };
    let name = b.get(3..).unwrap_or(&[]);
    let group = if name.is_empty() {
        None
    } else {
        let mut g = GroupName::new();
        for x in name {
            g.push(*x).map_err(|_| Error::TooLarge {
                len: name.len(),
                max: crate::pool::MAX_GROUP_NAME,
            })?;
        }
        Some(g)
    };
    Ok(Placement {
        group,
        pool_cores: usize::from(pool),
        cores: usize::from(cores),
        class,
    })
}

/// De system-API: de bevoegdheid en de grants die tussen twee calls leven.
pub struct System<'i, 'r, const N: usize> {
    inbox: &'i Mailbox<Envelope<'r>, N>,
    svc: &'i Servicers,
    privilege: Option<Privilege>,
    grants: [Option<ImageGrant>; SLOT_CAP + 1],
    max_slots: usize,
}

impl<'i, 'r, const N: usize> System<'i, 'r, N> {
    /// Een system-API over deze actor en servicers, met de bevoegdheid van
    /// Hop (als die er al is).
    pub fn new(
        inbox: &'i Mailbox<Envelope<'r>, N>,
        svc: &'i Servicers,
        privilege: Option<Privilege>,
        max_slots: usize,
    ) -> Self {
        System {
            inbox,
            svc,
            privilege,
            grants: [const { None }; SLOT_CAP + 1],
            max_slots,
        }
    }

    /// Laat een verbinding toe: het slot uit het bron-IP, een levende
    /// servicer, en hooguit [`MAX_SYSTEM_CONNS`]. Het resultaat geeft de
    /// verbinding weer vrij in zijn `Drop`.
    pub fn admit(&self, ip: u32) -> Option<Admitted<'i>> {
        let slot = slot_from_remote(ip, self.max_slots)?;
        let generation = self.svc.current(slot)?;
        let ctl = self.svc.ctl(slot)?;
        ctl.try_conn(MAX_SYSTEM_CONNS).then_some(Admitted {
            slot,
            generation,
            svc: self.svc,
        })
    }

    /// Dient één verbinding: frames lezen tot de peer weggaat, de
    /// levensduur wisselt of een frame ongeldig is. `buf` is de callbuffer
    /// uit de pool van deze verbinding; `reply` zijn antwoordplek.
    #[expect(
        clippy::too_many_arguments,
        reason = "de verbinding brengt haar eigen buffers mee"
    )]
    pub async fn serve(
        &mut self,
        conn: &mut impl Conn,
        who: &Admitted<'_>,
        reply: &'r Reply,
        mem: &mut impl PhysMem,
        hooks: &mut impl Hooks,
        log: &impl Console,
        buf: &mut [u8],
        out: &mut [u8],
    ) -> Result {
        loop {
            let (kind, n) = read_header(conn).await?;
            if kind != KIND_CALL && kind != KIND_LOG {
                return Err(Error::Corrupt { at: 5 });
            }
            let payload = buf.get_mut(..n).ok_or(Error::TooLarge { len: n, max: 0 })?;
            read_full(conn, payload).await?;
            // Een verbinding hoort bij één levensduur: een oude app die na
            // een herstart nog bytes stuurt, krijgt de nieuwe eigenaar van
            // hetzelfde IP nooit cadeau.
            if self.svc.current(who.slot) != Some(who.generation) {
                return Err(Error::Conn);
            }
            if kind == KIND_LOG {
                log.app_line(who.slot, payload);
                continue;
            }
            let len = match Call::decode(payload) {
                Ok(call) => self.call(who.slot, &call, reply, mem, hooks, out).await,
                Err(_) => encode_resp(out, 0, STATUS_ERROR, 0, 0, b"bad request"),
            };
            write_frame(conn, KIND_RESULT, out.get(..len).unwrap_or(&[])).await?;
        }
    }

    async fn call(
        &mut self,
        slot: Slot,
        c: &Call<'_>,
        reply: &'r Reply,
        mem: &mut impl PhysMem,
        hooks: &mut impl Hooks,
        out: &mut [u8],
    ) -> usize {
        let privileged = (OP_START_SLOT..=OP_ARM_SLOT).contains(&c.op);
        let r = if !privileged {
            // De gewone calls (hopfs, store, codec) zijn nog niet geport.
            Err(Error::Kind)
        } else {
            match self.privilege.take() {
                Some(p) if p.slot == slot => {
                    let r = self.privileged(&p, c, reply, mem, hooks, out).await;
                    self.privilege = Some(p);
                    r
                }
                other => {
                    self.privilege = other;
                    Err(Error::Privilege { slot: slot.get() })
                }
            }
        };
        match r {
            Ok((size, data_len)) => {
                let mut tmp = [0u8; 64];
                let d = out.get(REQ_HEADER..REQ_HEADER + data_len).unwrap_or(&[]);
                let k = d.len().min(tmp.len());
                tmp[..k].copy_from_slice(d.get(..k).unwrap_or(&[]));
                encode_resp(out, c.op, STATUS_OK, c.seq, size, &tmp[..k])
            }
            Err(e) => {
                let status = match e {
                    Error::NoEnt => STATUS_NO_ENT,
                    Error::Privilege { .. } => STATUS_DENIED,
                    _ => STATUS_ERROR,
                };
                let mut msg = [0u8; 96];
                let n = fmt_into(&mut msg, e);
                encode_resp(out, c.op, status, c.seq, 0, &msg[..n])
            }
        }
    }

    /// De bevoegde operaties. `&Privilege` is het bewijs.
    async fn privileged(
        &mut self,
        _proof: &Privilege,
        c: &Call<'_>,
        reply: &'r Reply,
        mem: &mut impl PhysMem,
        hooks: &mut impl Hooks,
        out: &mut [u8],
    ) -> Result<(u64, usize)> {
        let target = || {
            Slot::new(c.off as usize).ok_or(Error::SlotRange {
                slot: c.off as usize,
                max: SLOT_CAP,
            })
        };
        match c.op {
            OP_START_SLOT => {
                let spec = StartSpec::new(target()?, c.n, decode_placement(c.data)?);
                match slots::call(self.inbox, reply, Request::Claim(spec)).await? {
                    Response::Granted(g) => {
                        let size = g.region().size;
                        if let Some(e) = self.grants.get_mut(g.slot().get()) {
                            *e = Some(g);
                        }
                        Ok((size, 0))
                    }
                    Response::Failed(e) => Err(e),
                    _ => Err(Error::Busy),
                }
            }
            OP_STREAM_IMAGE => {
                let g = self
                    .grants
                    .get_mut(target()?.get())
                    .and_then(Option::as_mut)
                    .ok_or(Error::NotOwned {
                        slot: c.off as usize,
                    })?;
                g.write(mem, c.n, c.data)?;
                Ok((c.data.len() as u64, 0))
            }
            OP_ARM_SLOT => {
                let slot = target()?;
                let grant = self
                    .grants
                    .get_mut(slot.get())
                    .and_then(Option::take)
                    .ok_or(Error::NotOwned { slot: slot.get() })?;
                done(slots::call(self.inbox, reply, Request::Arm { grant, entry: c.n }).await?)
            }
            OP_STOP_SLOT => {
                let slot = target()?;
                if let Some(grant) = self.grants.get_mut(slot.get()).and_then(Option::take) {
                    return done(slots::call(self.inbox, reply, Request::Abort(grant)).await?);
                }
                let timeout = Duration::from_millis(c.n);
                done(slots::call(self.inbox, reply, Request::Stop { slot, timeout }).await?)
            }
            OP_SLOT_STATUS => match slots::call(self.inbox, reply, Request::Status(target()?))
                .await?
            {
                Response::Status(st) => {
                    let occ = match st.occupancy {
                        Occupancy::Empty => 0u8,
                        Occupancy::Streaming => 1,
                        Occupancy::Running => 2,
                        Occupancy::Quarantined => 3,
                    };
                    let (core, span) = st.core.map_or((0, 0), |(c, s)| (c.get() as u16, s as u16));
                    let mut d = [0u8; 29];
                    d[0] = occ;
                    d[1..3].copy_from_slice(&core.to_le_bytes());
                    d[3..5].copy_from_slice(&span.to_le_bytes());
                    d[5..13].copy_from_slice(&st.cage.app.to_le_bytes());
                    d[13..21].copy_from_slice(&st.cage.exit_code.to_le_bytes());
                    d[21..29].copy_from_slice(&st.cage.heartbeat.to_le_bytes());
                    if let Some(o) = out.get_mut(REQ_HEADER..REQ_HEADER + d.len()) {
                        o.copy_from_slice(&d);
                    }
                    Ok((st.partition.map_or(0, |p| p.size), d.len()))
                }
                Response::Failed(e) => Err(e),
                _ => Err(Error::Busy),
            },
            OP_SET_CLOCK => {
                hooks.set_clock(c.n);
                Ok((0, 0))
            }
            OP_FLIP => hooks.flip(c.data, c.path).map(|()| (0, 0)),
            _ => Err(Error::Kind),
        }
    }
}

fn done(r: Response) -> Result<(u64, usize)> {
    match r {
        Response::Done => Ok((0, 0)),
        Response::Failed(e) => Err(e),
        _ => Err(Error::Busy),
    }
}

fn fmt_into(buf: &mut [u8], e: Error) -> usize {
    struct W<'b>(&'b mut [u8], usize);
    impl core::fmt::Write for W<'_> {
        fn write_str(&mut self, s: &str) -> core::fmt::Result {
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
    let _ = core::fmt::write(&mut w, format_args!("{e}"));
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
    use crate::slots::tests::{FakeConsole, Obey, actor, s};
    use crate::stage2::tests::SparseMem;
    use core::pin::pin;
    use core::task::{Context, Poll, Waker};
    use std::collections::VecDeque;
    use std::vec::Vec;

    /// Een verbinding in RAM die per `read` hooguit `chunk` bytes geeft.
    struct Pipe {
        rx: VecDeque<u8>,
        tx: Vec<u8>,
        chunk: usize,
        ip: u32,
    }

    impl Conn for Pipe {
        fn read(&mut self, buf: &mut [u8]) -> impl Future<Output = Result<usize>> {
            let n = buf.len().min(self.chunk).min(self.rx.len());
            for b in buf.iter_mut().take(n) {
                *b = self.rx.pop_front().unwrap();
            }
            core::future::ready(Ok(n))
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

    fn call(op: u8, seq: u32, off: u64, n: u64, path: &[u8], data: &[u8]) -> Vec<u8> {
        let mut v = std::vec![ABI_VERSION, op];
        v.extend_from_slice(&(path.len() as u16).to_le_bytes());
        v.extend_from_slice(&seq.to_le_bytes());
        v.extend_from_slice(&off.to_le_bytes());
        v.extend_from_slice(&n.to_le_bytes());
        v.extend_from_slice(path);
        v.extend_from_slice(data);
        v
    }

    /// De antwoorden uit de uitvoer: (op, status, seq, size, data).
    fn results(mut b: &[u8]) -> Vec<(u8, u16, u32, u64, Vec<u8>)> {
        let mut out = Vec::new();
        while b.len() >= HEADER_LEN {
            assert_eq!(b[5], KIND_RESULT);
            let n = u32::from_le_bytes(b[8..12].try_into().unwrap()) as usize;
            let p = &b[HEADER_LEN..HEADER_LEN + n];
            out.push((
                p[1],
                u16::from_le_bytes([p[2], p[3]]),
                u32::from_le_bytes(p[4..8].try_into().unwrap()),
                u64::from_le_bytes(p[8..16].try_into().unwrap()),
                p[REQ_HEADER..].to_vec(),
            ));
            b = &b[HEADER_LEN + n..];
        }
        out
    }

    struct NoHooks(u64);
    impl Hooks for NoHooks {
        fn set_clock(&mut self, unix_ns: u64) {
            self.0 = unix_ns;
        }
        fn flip(&mut self, _: &[u8], _: &[u8]) -> Result {
            Err(Error::Busy)
        }
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
        let big = std::vec![0xa5u8; MAX_IO_CHUNK];
        let mut p = Pipe {
            rx: frame(KIND_CALL, &big).into(),
            tx: Vec::new(),
            chunk: 1,
            ip: 0,
        };
        let (kind, n) = crate::testutil::block_on(read_header(&mut p)).unwrap();
        assert_eq!((kind, n), (KIND_CALL, MAX_IO_CHUNK));
        let mut w = Pipe {
            rx: VecDeque::new(),
            tx: Vec::new(),
            chunk: 1,
            ip: 0,
        };
        let too_big = std::vec![0u8; MAX_PAYLOAD + 1];
        assert!(crate::testutil::block_on(write_frame(&mut w, KIND_CALL, &too_big)).is_err());
        assert!(w.tx.is_empty(), "oversized frame half written");
    }

    /// Hop (slot 1, bevoegd) claimt slot 3, streamt, wapent, vraagt de
    /// status en stopt; een ander slot krijgt voor dezelfde call "denied".
    #[test]
    fn hop_drives_the_lifecycle_and_others_are_denied() {
        let (svc, con) = (Servicers::new(), FakeConsole::default());
        let mut a = actor(&svc, &con, Obey::Exit, 64, 4);
        // Hop zelf draait in slot 1 met een levende servicer.
        crate::slots::tests::start(&mut a, 1, 8, 1).unwrap();
        crate::slots::tests::start(&mut a, 2, 8, 1).unwrap();
        let reply = Reply::new();
        let inbox: Mailbox<Envelope<'_>, 8> = Mailbox::new();
        let mut sys = System::new(&inbox, &svc, Some(Privilege::for_test(s(1))), 8);
        let mut place = [0u8; 16];
        let pl = encode_placement(
            &Placement {
                group: None,
                pool_cores: 1,
                cores: 1,
                class: None,
            },
            &mut place,
        );
        let mut rx = Vec::new();
        rx.extend(frame(
            KIND_CALL,
            &call(OP_START_SLOT, 1, 3, 4 << 20, b"", &place[..pl]),
        ));
        rx.extend(frame(
            KIND_CALL,
            &call(OP_STREAM_IMAGE, 2, 3, 0x1000, b"", b"\x7fELF"),
        ));
        rx.extend(frame(
            KIND_CALL,
            &call(OP_STREAM_IMAGE, 3, 3, 8 << 20, b"", b"x"),
        ));
        rx.extend(frame(
            KIND_CALL,
            &call(OP_ARM_SLOT, 4, 3, 0x4001_0000, b"", b""),
        ));
        rx.extend(frame(KIND_CALL, &call(OP_SLOT_STATUS, 5, 3, 0, b"", b"")));
        rx.extend(frame(
            KIND_CALL,
            &call(OP_SET_CLOCK, 6, 0, 1_759_000_000, b"", b""),
        ));
        let mut hop = Pipe {
            rx: rx.into(),
            tx: Vec::new(),
            chunk: 7,
            ip: NET | 2,
        };
        let who = sys.admit(hop.remote_ip4()).unwrap();
        let (mut mem, mut hooks) = (SparseMem::default(), NoHooks(0));
        let (mut buf, mut out) = (std::vec![0u8; 4096], std::vec![0u8; 4096]);
        {
            let conr = &con;
            let mut serve = pin!(sys.serve(
                &mut hop, &who, &reply, &mut mem, &mut hooks, &conr, &mut buf, &mut out
            ));
            let mut run = pin!(a.run(&inbox));
            let mut cx = Context::from_waker(Waker::noop());
            let r = loop {
                let _ = run.as_mut().poll(&mut cx);
                if let Poll::Ready(r) = serve.as_mut().poll(&mut cx) {
                    break r;
                }
            };
            assert_eq!(r, Err(Error::Conn), "stream should end at EOF");
        }
        let res = results(&hop.tx);
        assert_eq!(res.len(), 6);
        assert_eq!((res[0].1, res[0].3), (STATUS_OK, 4 << 20), "claim");
        assert_eq!(res[1].1, STATUS_OK, "stream inside the partition");
        assert_eq!(res[2].1, STATUS_ERROR, "stream beyond the partition");
        assert_eq!(res[3].1, STATUS_OK, "arm");
        assert_eq!((res[4].1, res[4].4[0]), (STATUS_OK, 2), "status: running");
        assert_eq!(hooks.0, 1_759_000_000);
        let part = a.status(s(3)).partition.unwrap();
        assert_eq!(
            mem.read64(part.base + 0x1000) as u32,
            u32::from_le_bytes(*b"\x7fELF")
        );
        drop(who);

        // Slot 2 is niet bevoegd.
        let mut other = Pipe {
            rx: frame(KIND_CALL, &call(OP_STOP_SLOT, 9, 3, 10, b"", b"")).into(),
            tx: Vec::new(),
            chunk: 64,
            ip: NET | 3,
        };
        let who = sys.admit(other.remote_ip4()).unwrap();
        {
            let conr = &con;
            let mut serve = pin!(sys.serve(
                &mut other, &who, &reply, &mut mem, &mut hooks, &conr, &mut buf, &mut out
            ));
            let mut cx = Context::from_waker(Waker::noop());
            while serve.as_mut().poll(&mut cx).is_pending() {}
        }
        let res = results(&other.tx);
        assert_eq!(res[0].1, STATUS_DENIED);
        assert_eq!(
            a.status(s(3)).occupancy,
            Occupancy::Running,
            "unprivileged stop took effect"
        );
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
        drop(one);
        assert!(sys.admit(NET | 2).is_some());
        drop(two);
        assert!(
            sys.admit(NET | 3).is_none(),
            "slot without servicer admitted"
        );
    }
}
