//! RTKit: het gesprek met de coprocessoren van Apple silicon, en de SART.
//!
//! Een aantal randapparaten op deze SoC's is geen registerblok maar een eigen
//! processortje met firmware: de opslag (ANS), het beeld (DCP), de sensoren
//! (SMC). Ze delen één protocol, RTKit, over een mailbox van twee registers
//! (de ASC). Wie de SSD wil aanspreken moet eerst dit gesprek voeren, en pas
//! daarna het gewone NVMe praten dat erachter zit.
//!
//! Het gesprek is kort en vast: wek de coprocessor, ontvang HELLO met een
//! versiebereik, antwoord met de versie die je kiest, ontvang de kaart van
//! zijn endpoints, start de systeem-endpoints, en wacht tot hij "aan" meldt.
//! Onderweg vraagt hij om geheugen (syslog, crashlog, io-rapportage) en dat
//! moeten wij geven, want zonder antwoord komt hij zijn opstart niet door.
//!
//! Wat deze crate bezit: de mailbox van één coprocessor ([`Rtkit`], de
//! eigenaar houdt hem als `&mut`), de buffers die hij kreeg (bump-toewijzing
//! uit een regio die het board aanwijst, geen heap), en zijn power-staat.
//! Wat niet: firmware laden (die zit al in de coprocessor), zijn syslog lezen
//! (we bevestigen de regels en gooien ze weg), en de protocollen van de
//! applicatie-endpoints (0x20 en hoger): die gaan naar de driver erboven via
//! de `app`-haak van [`Rtkit::poll`]. De SART (het adresfilter voor de ANS)
//! staat in [`sart`].
//!
//! De Rust-vorm van `OLD/metal/driver/rtkit`; referentie m1n1 `src/rtkit.c`
//! en `src/asc.c`.
//!
//! De klok en de mailbox worden samen gelezen: elke wachtlus loopt over
//! [`Rtkit::poll`], dat per bericht eerst de klok en dan de inbox leest, en
//! na elk eigen bericht trekt de driver de inbox één keer leeg. Een
//! coprocessor met een volle uitgaande mailbox wacht op ons, en een
//! wachtende coprocessor neemt ook niets meer aan. Die vaste volgorde is
//! meteen wat de nep-coprocessor van de tests nodig heeft.

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

pub mod sart;

use core::fmt;
use core::mem::{offset_of, size_of};
use dev::{Pa, Reg};

/// Het CPU-blok vóór de mailbox (m1n1 `src/asc.c`).
#[repr(C)]
struct Cpu {
    _r0: [u32; 0x44 / 4],
    /// Bit 4 laat de coprocessor-core lopen.
    control: Reg<u32>,
}

/// De mailbox op `base + MBOX_OFF` (m1n1 `src/asc.c`). A2I is van ons naar
/// de coprocessor, I2A terug.
#[repr(C)]
struct Mbox {
    _r0: [u8; 0x110],
    a2i_control: Reg<u32>,
    i2a_control: Reg<u32>,
    _r1: [u8; 0x800 - 0x118],
    a2i_send0: Reg<u64>,
    a2i_send1: Reg<u64>,
    _r2: [u8; 0x830 - 0x810],
    i2a_recv0: Reg<u64>,
    i2a_recv1: Reg<u64>,
}

const MBOX_OFF: u64 = 0x8000;

const _: () = {
    assert!(offset_of!(Cpu, control) == 0x44);
    assert!(offset_of!(Mbox, a2i_control) == 0x110);
    assert!(offset_of!(Mbox, i2a_control) == 0x114);
    assert!(offset_of!(Mbox, a2i_send0) == 0x800);
    assert!(offset_of!(Mbox, a2i_send1) == 0x808);
    assert!(offset_of!(Mbox, i2a_recv0) == 0x830);
    assert!(offset_of!(Mbox, i2a_recv1) == 0x838);
    assert!(size_of::<Cpu>() as u64 <= MBOX_OFF);
};

/// Hoeveel van het ASC-blok het board moet mappen: het CPU-blok en de
/// mailbox.
pub const MMIO_LEN: u64 = MBOX_OFF + size_of::<Mbox>() as u64;

const CPU_START: u32 = 1 << 4;
const MBOX_FULL: u32 = 1 << 16;
const MBOX_EMPTY: u32 = 1 << 17;

/// Het management-endpoint: het opstartgesprek en de power-staten.
pub const EP_MGMT: u8 = 0;
/// De crashlog: een tweede buffervraag hier is een crashmelding.
pub const EP_CRASHLOG: u8 = 1;
/// De syslog: elke regel moet bevestigd worden.
pub const EP_SYSLOG: u8 = 2;
/// Debug.
pub const EP_DEBUG: u8 = 3;
/// De io-rapportage.
pub const EP_IOREPORT: u8 = 4;
/// De OS-log.
pub const EP_OSLOG: u8 = 8;
/// Het eerste applicatie-endpoint; daarboven praat de driver (de SMC op
/// 0x20), en deze crate weet van die protocollen niets.
pub const EP_APP: u8 = 0x20;

/// Het aantal systeem-endpoints waar we buffers voor bijhouden.
const SYS_EPS: usize = EP_OSLOG as usize + 1;

// Berichttypes op het management-endpoint (m1n1 `src/rtkit.c`).
const MSG_HELLO: u8 = 1;
const MSG_HELLO_ACK: u8 = 2;
const MSG_START_EP: u8 = 5;
const MSG_IOP_PWR_STATE: u8 = 6;
const MSG_IOP_PWR_ACK: u8 = 7;
const MSG_EPMAP: u8 = 8;
const MSG_AP_PWR_STATE: u8 = 0xb;

// Berichttypes op de systeem-endpoints.
const MSG_BUFFER_REQUEST: u8 = 1;
const MSG_SYSLOG_LOG: u8 = 5;
const MSG_SYSLOG_INIT: u8 = 8;

/// Power-staat: slaap.
pub const POWER_SLEEP: u16 = 0x01;
/// Power-staat van de AP-kant bij afsluiten: stil, niet verder (m1n1: anders
/// werkt herstarten niet).
pub const POWER_QUIESCED: u16 = 0x10;
/// Power-staat: aan.
pub const POWER_ON: u16 = 0x20;
/// Het wekbericht.
const POWER_INIT: u16 = 0x220;

const MIN_VERSION: u16 = 11;
const MAX_VERSION: u16 = 12;

/// De paginamaat van dit silicium: de coprocessor en de SART rekenen er
/// allebei mee, dus elke buffer is er een veelvoud van.
pub const BUF_ALIGN: u64 = 0x4000;

/// Hoe lang een volle uitgaande mailbox mag duren.
pub const SEND_TIMEOUT_NS: u64 = 200_000_000;
/// Hoe lang op HELLO en op de endpointkaart gewacht wordt.
pub const HANDSHAKE_TIMEOUT_NS: u64 = 1_000_000_000;
/// Hoe lang op een power-staat gewacht wordt.
pub const POWER_TIMEOUT_NS: u64 = 5_000_000_000;
/// Hoe lang [`Rtkit::start_ep`] op het eerste bericht van het endpoint
/// wacht.
pub const START_EP_WAIT_NS: u64 = 20_000_000;
/// Hoeveel berichten één [`Rtkit::poll`] hoogstens afhandelt: begrensd, zodat
/// een praatzieke coprocessor de aanroeper niet vasthoudt.
pub const POLL_BATCH: usize = 32;

/// Het tekstentry-maximum dat de crashlog toont, per entry.
const CRASH_TEXT_MAX: u64 = 400;

/// Waar een wachtende stap op wachtte.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Wait {
    /// HELLO, het eerste bericht na het wekken.
    Hello,
    /// Een stuk van de endpointkaart.
    EndpointMap,
    /// Power-staat aan.
    PowerOn,
    /// De AP-kant stil.
    Quiesced,
    /// De coprocessor in slaap.
    Asleep,
}

impl fmt::Display for Wait {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Self::Hello => "HELLO",
            Self::EndpointMap => "endpoint map",
            Self::PowerOn => "power state ON",
            Self::Quiesced => "AP quiesced",
            Self::Asleep => "coprocessor asleep",
        })
    }
}

/// Waarom het gesprek niet lukte. Elke variant draagt de naam van de
/// coprocessor: er hangen er meerdere aan deze bus.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    /// De bufferregio is niet op [`BUF_ALIGN`] of loopt om.
    Pool {
        /// De basis.
        base: u64,
        /// De maat.
        size: u64,
    },
    /// De uitgaande mailbox bleef vol.
    MailboxFull {
        /// De coprocessor.
        name: &'static str,
    },
    /// Geen bericht binnen de grens.
    Silent {
        /// De coprocessor.
        name: &'static str,
        /// Waar we op wachtten.
        waiting: Wait,
    },
    /// Een ander bericht dan het gesprek voorschrijft.
    Unexpected {
        /// De coprocessor.
        name: &'static str,
        /// Waar we op wachtten.
        waiting: Wait,
        /// Het endpoint van wat kwam.
        ep: u8,
        /// Het type van wat kwam.
        kind: u8,
    },
    /// Geen gemeenschappelijke protocolversie.
    Version {
        /// De coprocessor.
        name: &'static str,
        /// Zijn laagste.
        min: u16,
        /// Zijn hoogste.
        max: u16,
    },
    /// De bufferregio is op.
    NoRoom {
        /// De coprocessor.
        name: &'static str,
        /// Het endpoint dat vroeg.
        ep: u8,
        /// De gevraagde maat in bytes.
        size: u64,
    },
    /// De coprocessor vroeg een tweede crashlog-buffer: hij viel om. De
    /// melding staat in [`Rtkit::crashlog`].
    Crashed {
        /// De coprocessor.
        name: &'static str,
    },
    /// Een power-staat werd niet bereikt.
    Power {
        /// De coprocessor.
        name: &'static str,
        /// Waar we op wachtten.
        waiting: Wait,
        /// De staat na de grens.
        state: u16,
    },
    /// [`Rtkit::send`] op een systeem-endpoint: dat gesprek is van deze
    /// crate.
    Endpoint {
        /// De coprocessor.
        name: &'static str,
        /// Het endpoint.
        ep: u8,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::Pool { base, size } => write!(
                f,
                "rtkit: buffer region {size:#x} at {base:#x} invalid (need {BUF_ALIGN:#x} alignment)"
            ),
            Self::MailboxFull { name } => write!(
                f,
                "rtkit({name}): outgoing mailbox full for {} ms, coprocessor stuck?",
                SEND_TIMEOUT_NS / 1_000_000
            ),
            Self::Silent { name, waiting } => {
                write!(f, "rtkit({name}): no message while waiting for {waiting}")
            }
            Self::Unexpected {
                name,
                waiting,
                ep,
                kind,
            } => write!(
                f,
                "rtkit({name}): expected {waiting}, got type {kind:#x} on endpoint {ep:#x}"
            ),
            Self::Version { name, min, max } => write!(
                f,
                "rtkit({name}): coprocessor speaks versions [{min},{max}], we speak [{MIN_VERSION},{MAX_VERSION}]"
            ),
            Self::NoRoom { name, ep, size } => write!(
                f,
                "rtkit({name}): no room for a {} KB buffer for endpoint {ep}",
                size >> 10
            ),
            Self::Crashed { name } => write!(f, "rtkit({name}): coprocessor crashed"),
            Self::Power {
                name,
                waiting,
                state,
            } => write!(f, "rtkit({name}): no {waiting} (state {state:#x})"),
            Self::Endpoint { name, ep } => write!(
                f,
                "rtkit({name}): endpoint {ep:#x} is a system endpoint, not the driver's"
            ),
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Een haak die applicatieberichten weggooit, voor een coprocessor zonder
/// applicatie-endpoints (de ANS): `rt.poll(&mut driver_rtkit::ignore)`.
pub fn ignore(_ep: u8, _msg: u64) {}

/// Het type van een bericht: bits 59:52.
const fn kind(msg: u64) -> u8 {
    ((msg >> 52) & 0xff) as u8
}

/// Een bericht van type `t`.
const fn typed(t: u8) -> u64 {
    (t as u64) << 52
}

/// Eén buffer die de coprocessor kreeg of zelf noemde.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
struct Buf {
    pa: Pa,
    /// De maat die wíj toewezen; 0 voor een adres van de firmware zelf, dat
    /// we nooit aanraken.
    size: u64,
}

/// Eén RTKit-coprocessor. De eigenaar (de ANS- of SMC-driver) houdt hem als
/// `&mut`; er is geen tweede weg naar zijn mailbox.
pub struct Rtkit {
    base: Pa,
    name: &'static str,
    now: fn() -> u64,
    /// De bufferregio. Geen vrijgave: de coprocessor houdt zijn buffers
    /// vast zolang hij leeft.
    pool: dev::Bump,
    iop_power: u16,
    ap_power: u16,
    bufs: [Buf; SYS_EPS],
    /// Applicatie-endpoints waarop iets binnenkwam, één bit per endpoint.
    app_seen: [u64; 4],
}

impl Rtkit {
    /// Een coprocessor op ASC-blok `base`, zonder één registertoegang.
    /// `name` komt in foutmeldingen; buffers komen uit
    /// `[pool, pool + pool_size)`; `now` geeft monotone nanoseconden.
    ///
    /// # Safety
    ///
    /// `base` is het ASC-blok van een RTKit-coprocessor (ADT, `reg[0]`),
    /// gemapt als Device voor minstens [`MMIO_LEN`] bytes en voor altijd.
    /// `[pool, pool + pool_size)` is gemapt geheugen dat alleen deze
    /// coprocessor gebruikt, op een adres dat hij ziet zoals de CPU (voor de
    /// ANS: binnen een SART-venster), zolang het programma draait.
    pub unsafe fn new(
        base: Pa,
        name: &'static str,
        pool: Pa,
        pool_size: u64,
        now: fn() -> u64,
    ) -> Result<Self> {
        if !pool.is_aligned(BUF_ALIGN) || pool.0.checked_add(pool_size).is_none() {
            return Err(Error::Pool {
                base: pool.0,
                size: pool_size,
            });
        }
        Ok(Self {
            base,
            name,
            now,
            pool: dev::Bump::new(pool.0, pool_size),
            iop_power: 0,
            ap_power: 0,
            bufs: [Buf::default(); SYS_EPS],
            app_seen: [0; 4],
        })
    }

    /// De naam uit [`new`](Self::new).
    #[must_use]
    pub fn name(&self) -> &'static str {
        self.name
    }

    fn cpu(&self) -> &'static Cpu {
        // SAFETY: de voorwaarde van `new`: het ASC-blok is gemapt voor
        // MMIO_LEN bytes, en het CPU-blok ligt daarin.
        unsafe { dev::regs(self.base) }
    }

    fn mbox(&self) -> &'static Mbox {
        // SAFETY: zie `cpu`; de mailbox ligt op MBOX_OFF, binnen MMIO_LEN.
        unsafe { dev::regs(self.base.add(MBOX_OFF)) }
    }

    /// Zet één bericht in de uitgaande mailbox. De klok wordt alleen
    /// gelezen als de mailbox vol is: op de blije weg kost dit twee
    /// schrijfacties.
    fn post(&mut self, ep: u8, msg: u64) -> Result {
        let m = self.mbox();
        let room = || m.a2i_control.read() & MBOX_FULL == 0;
        if !room() && !dev::poll_until(self.now, SEND_TIMEOUT_NS, room) {
            return Err(Error::MailboxFull { name: self.name });
        }
        dev::mb();
        m.a2i_send0.write(msg);
        m.a2i_send1.write(u64::from(ep));
        Ok(())
    }

    /// Eén stap: eerst de klok, dan de inbox. Alle wachtlussen lopen
    /// hierover, zodat een tijdstempel altijd bij een blik in de inbox hoort.
    fn step(&mut self) -> (u64, Option<(u8, u64)>) {
        let t = (self.now)();
        let m = self.mbox();
        if m.i2a_control.read() & MBOX_EMPTY != 0 {
            return (t, None);
        }
        let msg = m.i2a_recv0.read();
        // Het endpoint is de lage byte van het tweede woord (m1n1: `u8 ep =
        // msg.msg1`).
        let ep = (m.i2a_recv1.read() & 0xff) as u8;
        dev::mb();
        (t, Some((ep, msg)))
    }

    /// Wacht op één bericht, hoogstens [`HANDSHAKE_TIMEOUT_NS`].
    fn recv(&mut self, waiting: Wait) -> Result<(u8, u64)> {
        let mut start = None;
        loop {
            let (t, m) = self.step();
            if let Some(m) = m {
                return Ok(m);
            }
            let s = *start.get_or_insert(t);
            if t.saturating_sub(s) >= HANDSHAKE_TIMEOUT_NS {
                return Err(Error::Silent {
                    name: self.name,
                    waiting,
                });
            }
            core::hint::spin_loop();
        }
    }

    /// Verwerkt wat er klaarstaat, hoogstens [`POLL_BATCH`] berichten, en
    /// geeft het tijdstip van de laatste blik op de klok. Berichten op
    /// applicatie-endpoints gaan naar `app(ep, msg)`.
    ///
    /// De driver erboven roept dit in zijn wachtlussen aan: een coprocessor
    /// met een volle uitgaande mailbox wacht, en een wachtende coprocessor
    /// doet geen DMA meer.
    pub fn poll(&mut self, app: &mut (impl FnMut(u8, u64) + ?Sized)) -> Result<u64> {
        let mut last = 0;
        for _ in 0..POLL_BATCH {
            let (t, m) = self.step();
            last = t;
            let Some((ep, msg)) = m else { break };
            self.handle(ep, msg, app)?;
        }
        Ok(last)
    }

    /// Pollt tot `done(self)` of tot `timeout_ns` verstreken is.
    fn wait(
        &mut self,
        timeout_ns: u64,
        app: &mut (impl FnMut(u8, u64) + ?Sized),
        done: impl Fn(&Self) -> bool,
    ) -> Result<bool> {
        let mut start = None;
        loop {
            let t = self.poll(app)?;
            if done(self) {
                return Ok(true);
            }
            let s = *start.get_or_insert(t);
            if t.saturating_sub(s) >= timeout_ns {
                return Ok(false);
            }
            core::hint::spin_loop();
        }
    }

    /// Stuurt één bericht en trekt daarna de inbox één keer leeg.
    fn say(&mut self, ep: u8, msg: u64, app: &mut (impl FnMut(u8, u64) + ?Sized)) -> Result {
        self.post(ep, msg)?;
        self.poll(app).map(drop)
    }

    /// Stuurt één bericht op een applicatie-endpoint (0x20 en hoger); de
    /// systeem-endpoints doet deze crate. Het antwoord komt via de haak van
    /// [`poll`](Self::poll).
    pub fn send(&mut self, ep: u8, msg: u64) -> Result {
        if ep < EP_APP {
            return Err(Error::Endpoint {
                name: self.name,
                ep,
            });
        }
        self.post(ep, msg)
    }

    /// Verwerkt één systeembericht; applicatieberichten gaan naar `app`.
    fn handle(&mut self, ep: u8, msg: u64, app: &mut (impl FnMut(u8, u64) + ?Sized)) -> Result {
        let t = kind(msg);
        match ep {
            EP_MGMT => match t {
                MSG_IOP_PWR_ACK => self.iop_power = (msg & 0xffff) as u16,
                MSG_AP_PWR_STATE => self.ap_power = (msg & 0xffff) as u16,
                _ => {}
            },
            EP_SYSLOG => match t {
                MSG_BUFFER_REQUEST => return self.give_buffer(ep, msg),
                // De afmetingen van zijn ringbuffer; wij lezen hem niet.
                MSG_SYSLOG_INIT => {}
                // Elke regel moet bevestigd worden, met precies hetzelfde
                // bericht terug, anders houdt hij op met loggen en
                // uiteindelijk met werken. De inhoud laten we liggen.
                MSG_SYSLOG_LOG => return self.post(ep, msg),
                _ => {}
            },
            EP_CRASHLOG if t == MSG_BUFFER_REQUEST => {
                // Een tweede crashlog-buffervraag is hoe de coprocessor
                // meldt dat hij omviel: de eerste kwam bij zijn opstart, de
                // tweede komt met de melding erin.
                if self.crash_buf() != Buf::default() {
                    return Err(Error::Crashed { name: self.name });
                }
                return self.give_buffer(ep, msg);
            }
            EP_IOREPORT => match t {
                MSG_BUFFER_REQUEST => return self.give_buffer(ep, msg),
                // Onbekend maar moet bevestigd worden (m1n1 doet hetzelfde).
                0x8 | 0xc => return self.post(ep, msg),
                _ => {}
            },
            ep if ep >= EP_APP => {
                // Van een driver: de SMC praat op 0x20. Zonder haak vielen
                // die berichten stil op de grond, en een coprocessor die op
                // antwoord wacht, houdt op met werken.
                if let Some(w) = self.app_seen.get_mut(usize::from(ep / 64)) {
                    *w |= 1 << (ep % 64);
                }
                app(ep, msg);
            }
            _ => {}
        }
        Ok(())
    }

    fn crash_buf(&self) -> Buf {
        self.bufs
            .get(usize::from(EP_CRASHLOG))
            .copied()
            .unwrap_or_default()
    }

    /// Heeft applicatie-endpoint `ep` al iets gestuurd?
    #[must_use]
    pub fn has_heard(&self, ep: u8) -> bool {
        self.app_seen
            .get(usize::from(ep / 64))
            .is_some_and(|w| w & (1 << (ep % 64)) != 0)
    }

    /// Beantwoordt een geheugenverzoek. Het verzoek draagt een maat in
    /// pagina's van 4 KB en soms een adres: dat laatste betekent "ik heb er
    /// zelf al een" en dan geven wij niets.
    fn give_buffer(&mut self, ep: u8, msg: u64) -> Result {
        let pages = (msg >> 44) & 0xff;
        let iova = msg & ((1 << 42) - 1);
        let Some(slot) = self.bufs.get_mut(usize::from(ep)) else {
            return Ok(());
        };
        if iova != 0 {
            // Van de firmware: geen grens van ons, dus nooit gelezen.
            *slot = Buf {
                pa: Pa(iova),
                size: 0,
            };
            return Ok(());
        }
        let want = match pages << 12 {
            0 => BUF_ALIGN,
            n => n,
        };
        let size = want.next_multiple_of(BUF_ALIGN);
        let pa = Pa(self.pool.take(size, BUF_ALIGN).ok_or(Error::NoRoom {
            name: self.name,
            ep,
            size,
        })?);
        dev::clear(pa, size as usize);
        dev::push(pa, size as usize);
        if let Some(slot) = self.bufs.get_mut(usize::from(ep)) {
            *slot = Buf { pa, size };
        }
        self.post(ep, typed(MSG_BUFFER_REQUEST) | (pages << 44) | pa.0)
    }

    /// Voert het opstartgesprek. Mag ook op een coprocessor die al draait:
    /// het wekbericht start het gesprek hoe dan ook opnieuw, en zonder dat
    /// wekbericht blijft hij in de slaapstand die iBoot achterliet (gemeten
    /// 29-08 op de ANS: CSTS.RDY werd nooit 1).
    pub fn boot(&mut self, app: &mut (impl FnMut(u8, u64) + ?Sized)) -> Result {
        // Een nieuw gesprek begint schoon: de buffers van een vorig leven
        // zijn niet meer van hem, en een oude crashlog-buffer zou de eerste
        // vraag om een nieuwe als crash laten lezen. De bump-wijzer loopt
        // wél door: geheugen dat hij misschien nog vasthoudt, geven we niet
        // opnieuw uit.
        self.bufs = [Buf::default(); SYS_EPS];
        self.app_seen = [0; 4];
        self.iop_power = 0;
        self.ap_power = 0;
        self.cpu().control.update(|c| c | CPU_START);
        self.post(EP_MGMT, typed(MSG_IOP_PWR_STATE) | u64::from(POWER_INIT))?;

        let (ep, msg) = self.recv(Wait::Hello)?;
        if ep != EP_MGMT || kind(msg) != MSG_HELLO {
            return Err(self.unexpected(Wait::Hello, ep, msg));
        }
        let min = (msg & 0xffff) as u16;
        let max = ((msg >> 16) & 0xffff) as u16;
        if min > MAX_VERSION || max < MIN_VERSION {
            return Err(Error::Version {
                name: self.name,
                min,
                max,
            });
        }
        let want = u64::from(max.min(MAX_VERSION));
        self.post(EP_MGMT, typed(MSG_HELLO_ACK) | (want << 16) | want)?;

        let have = self.endpoint_map()?;
        // Na de laatste bevestiging de inbox leegtrekken: daar staan zijn
        // eerste geheugenverzoeken al.
        self.poll(app)?;
        for e in [EP_DEBUG, EP_CRASHLOG, EP_SYSLOG, EP_IOREPORT, EP_OSLOG] {
            if have & (1 << e) != 0 {
                let m = typed(MSG_START_EP) | (u64::from(e) << 32) | (1 << 1);
                self.say(EP_MGMT, m, app)?;
            }
        }

        // Wachten tot hij "aan" meldt; onderweg beantwoordt `poll` zijn
        // geheugenverzoeken.
        if !self.wait(POWER_TIMEOUT_NS, app, |r| r.iop_power == POWER_ON)? {
            return Err(Error::Power {
                name: self.name,
                waiting: Wait::PowerOn,
                state: self.iop_power,
            });
        }
        // En melden dat wij er ook zijn: dit zet zijn syslog aan, en de
        // eerste regels komen meteen.
        self.say(EP_MGMT, typed(MSG_AP_PWR_STATE) | u64::from(POWER_ON), app)
    }

    /// Ontvangt de endpointkaart en geeft de systeem-endpoints als bits. De
    /// kaart komt in stukken van 32 bits; elk stuk moet bevestigd worden en
    /// het laatste draagt een klaar-vlag.
    fn endpoint_map(&mut self) -> Result<u64> {
        let mut have = 0u64;
        loop {
            let (ep, msg) = self.recv(Wait::EndpointMap)?;
            if ep != EP_MGMT || kind(msg) != MSG_EPMAP {
                return Err(self.unexpected(Wait::EndpointMap, ep, msg));
            }
            let bitmap = msg & 0xffff_ffff;
            let base = (msg >> 32) & 0x7;
            if base == 0 {
                // Alleen het eerste stuk draagt systeem-endpoints (0..32).
                have |= bitmap;
            }
            let done = msg & (1 << 51) != 0;
            let reply = typed(MSG_EPMAP) | (base << 32) | if done { 1 << 51 } else { 1 };
            self.post(EP_MGMT, reply)?;
            if done {
                return Ok(have);
            }
        }
    }

    fn unexpected(&self, waiting: Wait, ep: u8, msg: u64) -> Error {
        Error::Unexpected {
            name: self.name,
            waiting,
            ep,
            kind: kind(msg),
        }
    }

    /// Vraagt de coprocessor een applicatie-endpoint te openen. Nodig vóór
    /// het eerste bericht erop: een endpoint dat niet gestart is, slikt
    /// alles zonder te antwoorden.
    ///
    /// De coprocessor bevestigt niet apart dat een applicatie-endpoint
    /// openging; wat je krijgt is zijn eerste bericht erop. Dus: even
    /// pollen ([`START_EP_WAIT_NS`]) zodat een vroeg bericht niet verloren
    /// gaat, en dan door; de aanroeper wacht toch op zijn eigen antwoord.
    pub fn start_ep(&mut self, ep: u8, app: &mut (impl FnMut(u8, u64) + ?Sized)) -> Result {
        let m = typed(MSG_START_EP) | (u64::from(ep) << 32) | (1 << 1);
        self.say(EP_MGMT, m, app)?;
        self.wait(START_EP_WAIT_NS, app, |r| r.has_heard(ep))
            .map(drop)
    }

    /// Zet de coprocessor terug zoals we hem aantroffen: eerst melden dat
    /// wij weggaan, dan hem in slaap, dan zijn kern stilzetten.
    ///
    /// Dit is geen netheid maar noodzaak: laat je hem draaien, dan kan de
    /// vólgende boot hem niet meer overnemen (gemeten 29-08: de
    /// NVMe-controller wordt dan nooit meer ready) en helpt alleen nog een
    /// power-reset van zijn hele domein. De AP-kant gaat naar QUIESCED en
    /// niet verder; m1n1 zegt erbij dat herstarten anders niet werkt.
    pub fn sleep(&mut self, app: &mut (impl FnMut(u8, u64) + ?Sized)) -> Result {
        let q = typed(MSG_AP_PWR_STATE) | u64::from(POWER_QUIESCED);
        self.say(EP_MGMT, q, app)?;
        if !self.wait(POWER_TIMEOUT_NS, app, |r| r.ap_power == POWER_QUIESCED)? {
            return Err(Error::Power {
                name: self.name,
                waiting: Wait::Quiesced,
                state: self.ap_power,
            });
        }
        let s = typed(MSG_IOP_PWR_STATE) | u64::from(POWER_SLEEP);
        self.say(EP_MGMT, s, app)?;
        if !self.wait(POWER_TIMEOUT_NS, app, |r| r.iop_power == POWER_SLEEP)? {
            return Err(Error::Power {
                name: self.name,
                waiting: Wait::Asleep,
                state: self.iop_power,
            });
        }
        self.cpu().control.update(|c| c & !CPU_START);
        Ok(())
    }

    /// De melding die de coprocessor in zijn crashbuffer achterliet, als
    /// iets dat je kunt printen. Dit is het enige kanaal waarlangs firmware
    /// die wij niet hebben ons vertelt wat er misging (29-08:
    /// `NVME_PERM_ERR F2H 1 SL[0] ERR 0x8 ... OP 60`).
    #[must_use]
    pub fn crashlog(&self) -> Crashlog {
        Crashlog {
            buf: self.crash_buf(),
        }
    }
}

/// De crashlog van een coprocessor; [`fmt::Display`] leest hem uit het
/// buffer, begrensd, zonder heap.
///
/// De vorm is een header met magic `CLHE` en daarachter entries van
/// `(type, _, _, lengte)`; alleen de tekstentries (`Cstr`) zeggen iets tegen
/// een mens.
#[derive(Clone, Copy, Debug)]
pub struct Crashlog {
    buf: Buf,
}

const MAGIC_CLHE: u32 = 0x434c_4845;
const MAGIC_CSTR: u32 = 0x4373_7472;
const CRASH_HDR: u64 = 32;
const CRASH_ENTRY_HDR: u64 = 16;
const CRASH_ENTRIES: usize = 32;

impl fmt::Display for Crashlog {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let Buf { pa, size } = self.buf;
        if size < CRASH_HDR {
            return f.write_str("no crashlog buffer");
        }
        let m = dev::read32(pa);
        if m != MAGIC_CLHE {
            return write!(f, "crashlog header {m:#x} (expected {MAGIC_CLHE:#x})");
        }
        let mut off = CRASH_HDR;
        let mut texts = 0;
        for _ in 0..CRASH_ENTRIES {
            if size - off < CRASH_ENTRY_HDR {
                break;
            }
            let p = pa.add(off);
            let t = dev::read32(p);
            let l = u64::from(dev::read32(p.add(12)));
            if t == MAGIC_CLHE || !(CRASH_ENTRY_HDR..=1 << 16).contains(&l) || l > size - off {
                break;
            }
            if t == MAGIC_CSTR {
                if texts > 0 {
                    f.write_str(" | ")?;
                }
                texts += 1;
                text(f, p, l)?;
            }
            off += l;
        }
        if texts == 0 {
            f.write_str("crashlog without a text entry")?;
        }
        Ok(())
    }
}

/// De tekst van één `Cstr`-entry van `l` bytes op `p`: vanaf byte 20 tot de
/// eerste nul, alleen printbare ASCII.
fn text(f: &mut fmt::Formatter<'_>, p: Pa, l: u64) -> fmt::Result {
    let end = l.min(CRASH_ENTRY_HDR + 4 + CRASH_TEXT_MAX);
    for j in CRASH_ENTRY_HDR + 4..end {
        let c = dev::read8(p.add(j));
        if c == 0 {
            break;
        }
        let c = if c.is_ascii_graphic() || c == b' ' {
            char::from(c)
        } else {
            '?'
        };
        fmt::Write::write_char(f, c)?;
    }
    Ok(())
}

#[cfg(test)]
mod fake;
#[cfg(test)]
mod tests;
