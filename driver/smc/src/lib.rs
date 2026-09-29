//! Apples System Management Controller: sleutels lezen, de temperaturen van
//! de die.
//!
//! Temperatuur is op elk ander HopOS-board een register (Radxa: TSADC) of
//! een firmware-mailbox (Pi: vcmail, O6N: SCMI). Op Apple silicon is het een
//! coprocessor: de SMC hangt aan dezelfde RTKit-bus als de opslag, praat op
//! endpoint 0x20, en beantwoordt vragen over sleutels van vier tekens:
//! hetzelfde model als de SMC in Intel-Macs, waar "TC0P" de CPU-temperatuur
//! was.
//!
//! Klein, en dat is hier geen ambitie maar een feit: de zware laag (het
//! opstartgesprek, syslog, crashlog, geheugenverzoeken) is `driver-rtkit`.
//! Wat hier bijkomt is één endpoint en vier opdrachten.
//!
//! Geen DART, geen SART: de SMC deelt geheugen via een adres dat hij zelf in
//! zijn eerste bericht noemt. Dat is meteen de opstartvolgorde: endpoint
//! openen, INITIALIZE sturen, en wachten tot dat adres binnenkomt. Het adres
//! moet in zijn SRAM liggen (ADT `/arm-io/smc`, het tweede `reg`); ligt het
//! erbuiten, dan weigeren we het, zoals Linux' `macsmc-rtkit`.
//!
//! Eén aanroeper tegelijk: de SMC is van wie `&mut` heeft (de thermiek-taak
//! van het board). De Rust-vorm van `OLD/metal/driver/smc`; referentie m1n1
//! `src/smc.c`.

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

use core::fmt;
use dev::Pa;
use driver_rtkit::Rtkit;

/// Het endpoint van de SMC.
pub const ENDPOINT: u8 = 0x20;

// Het protocol (m1n1 `src/smc.c`).
const CMD_READ_KEY: u8 = 0x10;
const CMD_GET_KEY_BY_INDEX: u8 = 0x12;
const CMD_INITIALIZE: u8 = 0x17;
const CMD_NOTIFICATION: u8 = 0x18;

/// De maat van het gedeelde geheugen (Linux: `SMC_SHMEM_SIZE`).
pub const SHMEM_SIZE: u64 = 0x4000;
/// Hoe lang op het adres van het gedeelde geheugen gewacht wordt.
pub const INIT_TIMEOUT_NS: u64 = 2_000_000_000;
/// Hoe lang één opdracht mag duren.
pub const CMD_TIMEOUT_NS: u64 = 1_000_000_000;
/// De grootste sleutellijst die [`Smc::sensors`] doorloopt; een SMC die er
/// meer meldt, meldt onzin.
pub const MAX_KEYS: u32 = 4096;

/// Een sleutel van vier tekens, big-endian zoals de SMC hem verwacht.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub struct Key(pub u32);

/// Maakt van (hoogstens) vier tekens de sleutel: `key("TC0P")`.
#[must_use]
pub const fn key(s: &str) -> Key {
    let mut b = s.as_bytes();
    let mut k = 0u32;
    let mut n = 0;
    while let [c, rest @ ..] = b {
        if n == 4 {
            break;
        }
        k = (k << 8) | *c as u32;
        b = rest;
        n += 1;
    }
    Key(k)
}

impl fmt::Display for Key {
    /// De vier tekens, voor foutmeldingen en dumps; een onprintbaar teken
    /// wordt `?`.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        for c in self.0.to_be_bytes() {
            let c = if c.is_ascii_graphic() || c == b' ' {
                char::from(c)
            } else {
                '?'
            };
            fmt::Write::write_char(f, c)?;
        }
        Ok(())
    }
}

/// Waarom de SMC niet antwoordde.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    /// Het gesprek met de coprocessor liep mis.
    Rtkit(driver_rtkit::Error),
    /// Geen adres van het gedeelde geheugen binnen [`INIT_TIMEOUT_NS`].
    NoShmem,
    /// Het gedeelde geheugen ligt buiten de SRAM die het board aanwees.
    BadShmem {
        /// Wat de SMC noemde.
        pa: u64,
    },
    /// Geen antwoord binnen [`CMD_TIMEOUT_NS`]. Blijvend: een opdracht die
    /// niet bevestigd is, mag zijn id en buffer niet aan een volgende geven.
    Timeout {
        /// De opdracht.
        cmd: u8,
        /// De sleutel of index.
        key: Key,
    },
    /// De SMC antwoordde met een foutcode.
    Failed {
        /// De opdracht.
        cmd: u8,
        /// De sleutel.
        key: Key,
        /// De code.
        code: u8,
    },
    /// De waarde is nul bytes, groter dan het gedeelde geheugen, of groter
    /// dan de buffer van de aanroeper.
    Size {
        /// De sleutel.
        key: Key,
        /// De gemelde maat.
        size: u16,
    },
    /// De sleutel is geen float van vier bytes.
    NotFloat {
        /// De sleutel.
        key: Key,
        /// De maat.
        size: usize,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::Rtkit(e) => e.fmt(f),
            Self::NoShmem => write!(
                f,
                "smc: no shared-memory address within {} ms",
                INIT_TIMEOUT_NS / 1_000_000
            ),
            Self::BadShmem { pa } => {
                write!(f, "smc: shared memory at {pa:#x} lies outside its SRAM")
            }
            Self::Timeout { cmd, key } => write!(
                f,
                "smc: command {cmd:#x} (key {key}) got no answer; no further commands until coprocessor restart"
            ),
            Self::Failed { cmd, key, code } => {
                write!(f, "smc: command {cmd:#x} (key {key}) failed with {code}")
            }
            Self::Size { key, size } => write!(f, "smc: key {key} reports {size} bytes"),
            Self::NotFloat { key, size } => {
                write!(f, "smc: key {key} is {size} bytes, not a float")
            }
        }
    }
}

impl From<driver_rtkit::Error> for Error {
    fn from(e: driver_rtkit::Error) -> Self {
        Self::Rtkit(e)
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Eén gevonden temperatuursleutel met zijn waarde.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct Sensor {
    /// De sleutel.
    pub key: Key,
    /// Graden Celsius.
    pub celsius: f32,
}

/// Wat de berichten van de SMC bijhouden; los van de [`Rtkit`], zodat de
/// haak van `poll` hem kan lenen terwijl de mailbox geleend is.
struct State {
    /// De SRAM van de SMC (ADT), waar het gedeelde geheugen in moet liggen.
    sram: (Pa, u64),
    shmem: Option<Pa>,
    bad_shmem: Option<u64>,
    /// Eén bit per id: wacht op antwoord.
    pending: u16,
    results: [u64; 16],
    msgid: u64,
    /// Een opdracht zonder bevestiging: blijvend, want zijn id en het
    /// gedeelde geheugen zijn nog van hem.
    failed: Option<Error>,
    /// Meetlat: meldingen van de SMC zelf, die wij niet gebruiken.
    notifications: u64,
}

impl State {
    fn handle(&mut self, ep: u8, msg: u64) {
        if ep != ENDPOINT {
            return;
        }
        if self.shmem.is_none() && self.bad_shmem.is_none() {
            // Het eerste bericht is het adres, niet een antwoord.
            let (base, size) = self.sram;
            let fits = msg >= base.0
                && msg
                    .checked_add(SHMEM_SIZE)
                    .is_some_and(|end| end <= base.0.saturating_add(size));
            if fits {
                self.shmem = Some(Pa(msg));
            } else {
                self.bad_shmem = Some(msg);
            }
            return;
        }
        if (msg & 0xff) as u8 == CMD_NOTIFICATION {
            self.notifications += 1;
            return;
        }
        let id = ((msg >> 12) & 0xf) as usize;
        if let Some(r) = self.results.get_mut(id) {
            *r = msg;
        }
        self.pending &= !(1 << id);
    }
}

/// Een geopende SMC.
pub struct Smc {
    rt: Rtkit,
    st: State,
}

impl Smc {
    /// Praat de SMC wakker: het opstartgesprek, endpoint 0x20, INITIALIZE,
    /// en wachten op het adres van het gedeelde geheugen. Faalt iets ná het
    /// opstarten, dan gaat de coprocessor eerst weer in slaap.
    ///
    /// Waarom die slaap: een half opgestarte RTKit-coprocessor die niemand
    /// meer pollt loopt vol (zijn syslog wil elke regel bevestigd zien). Bij
    /// de ANS is gemeten wat dat kost (de opslag tot de volgende
    /// power-reset); wat het bij dít blok kost weten we niet, en dat is
    /// precies de reden om het niet uit te proberen. (We dachten 31-08 dat
    /// we het wisten, een node die na ~2 minuten ophield, maar dat was de
    /// watchdog van de firmware, met of zonder SMC.)
    ///
    /// # Safety
    ///
    /// `[sram, sram + sram_size)` is de SRAM van de SMC (ADT
    /// `/arm-io/smc`), gemapt voor altijd; de driver leest daaruit.
    pub unsafe fn open(rt: Rtkit, sram: Pa, sram_size: u64) -> Result<Self> {
        let mut s = Self {
            rt,
            st: State {
                sram: (sram, sram_size),
                shmem: None,
                bad_shmem: None,
                pending: 0,
                results: [0; 16],
                msgid: 0,
                failed: None,
                notifications: 0,
            },
        };
        s.with_rt(|rt, app| rt.boot(app))?;
        match s.initialize() {
            Ok(()) => Ok(s),
            Err(e) => {
                // Beste inspanning; de fout hierboven is het verhaal.
                let _ = s.sleep();
                Err(e)
            }
        }
    }

    /// Roept `f` op de mailbox met de berichtenhaak van de SMC.
    fn with_rt<T>(
        &mut self,
        f: impl FnOnce(&mut Rtkit, &mut dyn FnMut(u8, u64)) -> driver_rtkit::Result<T>,
    ) -> Result<T> {
        let Self { rt, st } = self;
        f(rt, &mut |ep, m| st.handle(ep, m)).map_err(Error::Rtkit)
    }

    /// Het endpoint openen, INITIALIZE, en wachten op het adres.
    fn initialize(&mut self) -> Result {
        self.with_rt(|rt, app| rt.start_ep(ENDPOINT, app))?;
        self.send(CMD_INITIALIZE, 0, 0)?;
        let mut start = None;
        while self.st.shmem.is_none() {
            if let Some(pa) = self.st.bad_shmem {
                return Err(Error::BadShmem { pa });
            }
            let t = self.with_rt(|rt, app| rt.poll(app))?;
            let s = *start.get_or_insert(t);
            if t.saturating_sub(s) >= INIT_TIMEOUT_NS {
                return Err(Error::NoShmem);
            }
        }
        Ok(())
    }

    /// Zet de SMC in slaap (zie [`Rtkit::sleep`]).
    pub fn sleep(&mut self) -> Result {
        self.with_rt(|rt, app| rt.sleep(app))
    }

    /// De coprocessor eronder, voor zijn crashlog en meetlatten.
    #[must_use]
    pub fn rtkit(&self) -> &Rtkit {
        &self.rt
    }

    /// Meetlat: meldingen van de SMC zelf.
    #[must_use]
    pub fn notifications(&self) -> u64 {
        self.st.notifications
    }

    fn send(&mut self, cmd: u8, size: u8, key: u32) -> Result<usize> {
        let id = (self.st.msgid & 0xf) as usize;
        self.st.msgid = self.st.msgid.wrapping_add(1);
        let msg =
            u64::from(cmd) | ((id as u64) << 12) | (u64::from(size) << 16) | (u64::from(key) << 32);
        self.rt.send(ENDPOINT, msg)?;
        Ok(id)
    }

    /// Stuurt een opdracht en wacht op het antwoord met hetzelfde id.
    fn cmd(&mut self, cmd: u8, key: u32) -> Result<u64> {
        if let Some(e) = self.st.failed {
            return Err(e);
        }
        let k = Key(key);
        let id = match self.send(cmd, 0, key) {
            Ok(id) => id,
            Err(e) => return Err(*self.st.failed.insert(e)),
        };
        self.st.pending |= 1 << id;
        let mut start = None;
        while self.st.pending & (1 << id) != 0 {
            let t = match self.with_rt(|rt, app| rt.poll(app)) {
                Ok(t) => t,
                Err(e) => return Err(*self.st.failed.insert(e)),
            };
            let s = *start.get_or_insert(t);
            if t.saturating_sub(s) >= CMD_TIMEOUT_NS && self.st.pending & (1 << id) != 0 {
                return Err(*self.st.failed.insert(Error::Timeout { cmd, key: k }));
            }
        }
        let r = self.st.results.get(id).copied().unwrap_or_default();
        match (r & 0xff) as u8 {
            0 => Ok(r),
            code => Err(Error::Failed { cmd, key: k, code }),
        }
    }

    /// Leest sleutel `key` in `out` en geeft de maat. Waarden tot vier
    /// bytes komen in het antwoord zelf mee; grotere staan in het gedeelde
    /// geheugen.
    pub fn read(&mut self, key: Key, out: &mut [u8]) -> Result<usize> {
        let r = self.cmd(CMD_READ_KEY, key.0)?;
        let size = ((r >> 16) & 0xffff) as u16;
        let n = usize::from(size);
        let bad = Error::Size { key, size };
        if n == 0 || n as u64 > SHMEM_SIZE {
            return Err(bad);
        }
        let dst = out.get_mut(..n).ok_or(bad)?;
        if n <= 4 {
            let v = ((r >> 32) as u32).to_le_bytes();
            dst.copy_from_slice(v.get(..n).ok_or(bad)?);
        } else {
            let shmem = self.st.shmem.ok_or(Error::NoShmem)?;
            dev::pull(shmem, n);
            dev::copy_out(dst, shmem);
        }
        Ok(n)
    }

    /// De sleutel op index `i`: zo is de sleutellijst van dit silicium te
    /// doorlopen zonder hem te kennen. Welke sensoren een machine heeft,
    /// staat nergens gedocumenteerd; dit is de enige eerlijke manier om het
    /// te weten te komen.
    pub fn key_at(&mut self, i: u32) -> Result<Key> {
        let r = self.cmd(CMD_GET_KEY_BY_INDEX, i)?;
        Ok(Key((r >> 32) as u32))
    }

    /// Hoeveel sleutels deze SMC kent (sleutel `#KEY`, big-endian zoals
    /// alle SMC-tellers).
    pub fn count(&mut self) -> Result<u32> {
        let k = key("#KEY");
        let mut b = [0u8; 4];
        let n = self.read(k, &mut b)?;
        if n != 4 {
            return Err(Error::Size {
                key: k,
                size: n as u16,
            });
        }
        Ok(u32::from_be_bytes(b))
    }

    /// Leest een sleutel als IEEE-754 float32 (het `flt `-type dat Apple
    /// voor temperaturen gebruikt), in graden.
    pub fn float(&mut self, key: Key) -> Result<f32> {
        let mut b = [0u8; 8];
        let n = self.read(key, &mut b)?;
        match b.first_chunk::<4>() {
            Some(v) if n == 4 => Ok(f32::from_le_bytes(*v)),
            _ => Err(Error::NotFloat { key, size: n }),
        }
    }

    /// Loopt de sleutellijst langs en geeft elke sleutel die met `T` begint
    /// en als float leesbaar is aan `f`; geeft hoeveel dat er waren.
    ///
    /// Dit bestaat omdat de sensoren van een machine nergens staan
    /// opgeschreven: Apple kiest per generatie andere namen. Voor de
    /// meetbank en de eerste boot, niet per meting: het is één ronde over
    /// honderden sleutels. Een sleutel die weigert wordt overgeslagen; een
    /// opdracht zonder antwoord stopt de ronde (dan staat de SMC stil).
    pub fn sensors(&mut self, mut f: impl FnMut(Sensor)) -> Result<u32> {
        let n = self.count()?;
        if n > MAX_KEYS {
            return Err(Error::Size {
                key: key("#KEY"),
                size: u16::MAX,
            });
        }
        let mut found = 0;
        for i in 0..n {
            let k = match self.key_at(i) {
                Ok(k) => k,
                Err(Error::Failed { .. }) => continue,
                Err(e) => return Err(e),
            };
            if k.0 >> 24 != u32::from(b'T') {
                continue;
            }
            match self.float(k) {
                Ok(c) => {
                    found += 1;
                    f(Sensor { key: k, celsius: c });
                }
                Err(Error::Failed { .. } | Error::NotFloat { .. } | Error::Size { .. }) => {}
                Err(e) => return Err(e),
            }
        }
        Ok(found)
    }

    /// De warmste temperatuursleutel van deze machine. De terugval als geen
    /// van de bekende namen bestaat: een node die zijn hitte niet kent kan
    /// er ook niet op ingrijpen, en dan is de warmste sensor een beter
    /// antwoord dan géén antwoord.
    pub fn hottest(&mut self) -> Result<Option<Sensor>> {
        let mut best: Option<Sensor> = None;
        self.sensors(|s| {
            if best.is_none_or(|b| s.celsius > b.celsius) {
                best = Some(s);
            }
        })?;
        Ok(best)
    }
}

#[cfg(test)]
#[path = "../../rtkit/src/fake.rs"]
mod fake;
#[cfg(test)]
mod tests;
