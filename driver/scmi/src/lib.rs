//! Een minimale SCMI-client (Arm System Control and Management Interface,
//! DEN 0056) over het shared-memory-transport: één kanaal is een blok met
//! de standaard SCMI-shmem-indeling plus een doorbell.
//!
//! Dat is precies wat de Cix P1 (Orion O6N) zijn SCP aanbiedt. De ACPI-DSDT
//! van het board doet zijn temperatuurlezingen (`_TMP`) via dezelfde bytes
//! (device PMMX: OperationRegion op 0x065d0000, doorbell BEEL op +0x80); dit
//! is dus het bewezen pad dat Linux onder ACPI ook loopt, alleen zonder
//! AML-interpreter ertussen (`OLD/metal/driver/scmi`).
//!
//! Alleen wat HopOS nodig heeft: het generieke [`Channel::call`], de
//! sensoren (beschrijving en lezing) voor de thermometer, en de power-,
//! perf- en klok-berichten die de media-kant straks vraagt. DVFS loopt op de
//! O6N niet hierlangs maar via de SCMI-fastchannels uit de `_CPC` (een
//! MMIO-woord per domein; `board-o6n`); op de Radxa wel: de klok van de
//! A55's is daar `SCMI_CLK_CPU` van de TF-A (`board-rk3566::clock`).
//!
//! Eén aanroeper tegelijk: het kanaal is van wie `&mut` heeft (de
//! thermiek-taak van het board). De Go-`thermMu` verdwijnt daarmee.

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
use core::mem::offset_of;
use dev::{Pa, Reg};

/// De grootste payload: de AML werkt met buffers van 96 bytes, ruim voor
/// onze berichten.
pub const MAX_WORDS: usize = 24;

/// De shmem-indeling (SCMI §5.1.2 "Shared memory based transport") plus de
/// Cix-doorbell op +0x80.
#[repr(C)]
struct Shmem {
    _reserved0: u32,
    /// Bit 0 = vrij, bit 1 = fout.
    status: Reg<u32>,
    _reserved1: u32,
    /// Gereserveerd; Cix' AML zet hier een signatuur.
    sign: Reg<u32>,
    /// Bit 0 = completion-interrupt; wij pollen: 0.
    flags: Reg<u32>,
    /// Header plus payload in bytes.
    length: Reg<u32>,
    header: Reg<u32>,
    payload: [Reg<u32>; MAX_WORDS],
    _reserved2: u32,
    /// De Cix-mailbox-doorbell (AML: BEEL), bit 0.
    bell: Reg<u32>,
}

const _: () = {
    assert!(offset_of!(Shmem, status) == 0x04);
    assert!(offset_of!(Shmem, sign) == 0x0c);
    assert!(offset_of!(Shmem, flags) == 0x10);
    assert!(offset_of!(Shmem, length) == 0x14);
    assert!(offset_of!(Shmem, header) == 0x18);
    assert!(offset_of!(Shmem, payload) == 0x1c);
    assert!(offset_of!(Shmem, bell) == 0x80);
};

/// Hoeveel het board moet mappen.
pub const SHMEM_LEN: u64 = core::mem::size_of::<Shmem>() as u64;

const STATUS_FREE: u32 = 1 << 0;
const STATUS_ERROR: u32 = 1 << 1;
/// Wat Cix' AML in +0x0c zet (`MAILBOX_SCMI_BEGIN`).
const CIX_SIGNATURE: u32 = 0x5043_4303;

/// Protocollen.
pub mod proto {
    /// Base.
    pub const BASE: u8 = 0x10;
    /// Power domain.
    pub const POWER: u8 = 0x11;
    /// Performance.
    pub const PERF: u8 = 0x13;
    /// Clock.
    pub const CLOCK: u8 = 0x14;
    /// Sensor.
    pub const SENSOR: u8 = 0x15;
}

const MSG_VERSION: u8 = 0x00;
const BASE_LIST_PROTOCOLS: u8 = 0x06;
const POWER_STATE_SET: u8 = 0x04;
const POWER_STATE_GET: u8 = 0x05;
const PERF_LEVEL_SET: u8 = 0x07;
const PERF_LEVEL_GET: u8 = 0x08;
const CLOCK_RATE_SET: u8 = 0x05;
const CLOCK_RATE_GET: u8 = 0x06;
const CLOCK_CONFIG_SET: u8 = 0x07;
const SENSOR_DESCRIPTION_GET: u8 = 0x03;
const SENSOR_READING_GET: u8 = 0x06;

/// De stand "aan" die de Cix-TF-A verwacht.
pub const POWER_ON: u32 = 0;
/// De stand "uit" (SCMI bit 30).
pub const POWER_OFF: u32 = 1 << 30;

/// Hoe lang één bericht mag duren: de AML-waarde.
pub const TIMEOUT_NS: u64 = 400_000_000;

/// Het sensortype graden Celsius.
pub const CELSIUS: u8 = 2;

/// Waarom een bericht mislukte.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Error {
    /// Het verzoek past niet in de payload.
    TooLarge(usize),
    /// Het kanaal werd niet vrij, vóór of na het bericht.
    Busy {
        /// Het protocol.
        proto: u8,
        /// Het bericht.
        msg: u8,
        /// Status na de grens.
        status: u32,
    },
    /// Het platform zette de foutbit van het kanaal.
    Channel {
        /// Het protocol.
        proto: u8,
        /// Het bericht.
        msg: u8,
    },
    /// Een antwoordlengte buiten het bereik.
    Length(u32),
    /// Het platform antwoordde met een SCMI-status ongelijk aan nul.
    Status {
        /// Het protocol.
        proto: u8,
        /// Het bericht.
        msg: u8,
        /// De status (negatief: NOT_SUPPORTED -1, INVALID_PARAMETERS -2, ...).
        status: i32,
    },
    /// Een antwoord korter dan het bericht belooft. Zonder deze toets gaf een
    /// antwoord met alleen een status nul terug, en nul is precies
    /// [`POWER_ON`]: een zwijgende firmware bevestigde dan dat een domein aan
    /// stond.
    Short {
        /// Wat er kwam.
        got: usize,
        /// Wat nodig was.
        want: usize,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::TooLarge(n) => write!(f, "scmi: request of {n} words too large"),
            Self::Busy { proto, msg, status } => {
                write!(
                    f,
                    "scmi: channel busy on {proto:#x}/{msg:#x} (status {status:#x})"
                )
            }
            Self::Channel { proto, msg } => write!(f, "scmi: channel error on {proto:#x}/{msg:#x}"),
            Self::Length(n) => write!(f, "scmi: reply length {n} out of range"),
            Self::Status { proto, msg, status } => {
                write!(f, "scmi: {proto:#x}/{msg:#x} status {status}")
            }
            Self::Short { got, want } => write!(f, "scmi: reply of {got} words, want {want}"),
        }
    }
}

/// De `Result` van deze crate.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Een antwoord: de payload-woorden na de header; woord 0 is de status.
#[derive(Clone, Copy)]
pub struct Reply {
    words: [u32; MAX_WORDS],
    len: usize,
}

impl Reply {
    /// De woorden.
    #[must_use]
    pub fn words(&self) -> &[u32] {
        self.words.get(..self.len).unwrap_or(&[])
    }

    fn need(&self, want: usize) -> Result<&[u32]> {
        if self.len < want {
            return Err(Error::Short {
                got: self.len,
                want,
            });
        }
        Ok(self.words())
    }

    fn word(&self, i: usize) -> u32 {
        self.words.get(i).copied().unwrap_or(0)
    }
}

/// Eén sensorbeschrijving (SENSOR_DESCRIPTION_GET).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub struct Sensor {
    /// Het id.
    pub id: u32,
    /// `attributes_high[7:0]`: [`CELSIUS`] = graden Celsius.
    pub kind: u8,
    /// `attributes_high[15:11]`, vijf bits met teken: waarde maal
    /// 10^exponent.
    pub exponent: i8,
    /// De naam, 16 bytes, NUL-getermineerd.
    pub name: [u8; 16],
}

impl Sensor {
    /// De naam als tekst.
    #[must_use]
    pub fn name(&self) -> &str {
        let n = self.name.iter().position(|&b| b == 0).unwrap_or(16);
        self.name
            .get(..n)
            .and_then(|s| core::str::from_utf8(s).ok())
            .unwrap_or("?")
    }

    /// Een lezing van een Celsius-sensor in milligraden.
    #[must_use]
    pub fn milli_c(&self, v: i64) -> i64 {
        let mut v = v;
        let mut e = i32::from(self.exponent) + 3;
        while e > 0 {
            v = v.saturating_mul(10);
            e -= 1;
        }
        while e < 0 {
            v /= 10;
            e += 1;
        }
        v
    }
}

/// Eén SCMI-shmem-kanaal.
pub struct Channel {
    base: Pa,
    /// De doorbell als functie in plaats van het MMIO-woord. De Cix P1 heeft
    /// twéé kanalen: de mailbox die de AML gebruikt (bel op +0x80) en een
    /// kanaal naar de TF-A waarvan de bel een SMC is. Dat tweede schakelt de
    /// stroomdomeinen van GPU, VPU en NPU, en tegelijk de
    /// interconnect-permissies zonder welke elke registerlees op die blokken
    /// een SError geeft.
    ring: Option<fn()>,
    now: fn() -> u64,
    token: u32,
}

impl Channel {
    /// Een kanaal met de MMIO-doorbell op +0x80.
    ///
    /// # Safety
    ///
    /// `base` is een gemapt SCMI-shmem-blok van minstens [`SHMEM_LEN`] bytes
    /// dat blijft bestaan, en niemand anders dan deze client en het platform
    /// schrijft erin.
    #[must_use]
    pub unsafe fn new(base: Pa, now: fn() -> u64) -> Self {
        Self {
            base,
            ring: None,
            now,
            token: 0,
        }
    }

    /// Een kanaal waarvan de doorbell een functie is (de SMC naar de
    /// TF-A); die keert pas terug als het antwoord er staat.
    ///
    /// # Safety
    ///
    /// Als [`new`](Self::new); `ring` mag alleen de bel luiden.
    #[must_use]
    pub unsafe fn with_ring(base: Pa, now: fn() -> u64, ring: fn()) -> Self {
        Self {
            base,
            ring: Some(ring),
            now,
            token: 0,
        }
    }

    fn shm(&self) -> &'static Shmem {
        // SAFETY: de voorwaarde van `new`.
        unsafe { dev::regs(self.base) }
    }

    fn wait_free(&self) -> core::result::Result<(), u32> {
        let s = self.shm();
        let mut st = 0;
        if dev::poll_until(self.now, TIMEOUT_NS, || {
            st = s.status.read();
            st & STATUS_FREE != 0
        }) {
            return Ok(());
        }
        Err(st)
    }

    /// Stuurt één commando en wacht op het antwoord. De volgorde is die van
    /// de AML (`MAILBOX_SCMI_BEGIN`/`PROCESS`): wachten tot het kanaal vrij
    /// is, signatuur, flags, payload, lengte, header, bezet markeren, bel,
    /// wachten tot vrij, payload teruglezen.
    pub fn call(&mut self, proto: u8, msg: u8, req: &[u32]) -> Result<Reply> {
        if req.len() > MAX_WORDS {
            return Err(Error::TooLarge(req.len()));
        }
        let busy = |status| Error::Busy { proto, msg, status };
        self.wait_free().map_err(busy)?;
        let s = self.shm();
        if self.ring.is_none() {
            // De signatuur is een gewoonte van Cix' AML; het TF-A-kanaal kent
            // hem niet, dus daar blijft het gereserveerde woord met rust.
            s.sign.write(CIX_SIGNATURE);
        }
        s.flags.write(0);
        for (reg, &w) in s.payload.iter().zip(req) {
            reg.write(w);
        }
        s.length.write(4 + 4 * req.len() as u32);
        self.token = (self.token + 1) & 0x3ff;
        s.header.write(header(proto, msg, self.token));
        s.status.update(|v| v & !STATUS_FREE);
        dev::mb();
        match self.ring {
            Some(ring) => ring(),
            None => s.bell.write(1),
        }
        dev::mb();
        self.wait_free().map_err(busy)?;
        if s.status.read() & STATUS_ERROR != 0 {
            return Err(Error::Channel { proto, msg });
        }
        let n = s.length.read();
        if n < 8 || n > 4 + 4 * MAX_WORDS as u32 {
            return Err(Error::Length(n));
        }
        let mut r = Reply {
            words: [0; MAX_WORDS],
            len: (n as usize - 4) / 4,
        };
        for (w, reg) in r.words.iter_mut().zip(&s.payload).take(r.len) {
            *w = reg.read();
        }
        let status = r.word(0) as i32;
        if status != 0 {
            return Err(Error::Status { proto, msg, status });
        }
        Ok(r)
    }

    /// De versie (major << 16 | minor) van `proto`.
    pub fn version(&mut self, proto: u8) -> Result<u32> {
        let r = self.call(proto, MSG_VERSION, &[])?;
        Ok(r.need(2)?.get(1).copied().unwrap_or(0))
    }

    /// De protocol-id's die dit kanaal aanbiedt, in `out`; geeft het aantal.
    /// Op een board met meer SCMI-kanalen is dat de enige manier om te weten
    /// welk kanaal welke dienst draait.
    pub fn protocols(&mut self, out: &mut [u8]) -> Result<usize> {
        let r = self.call(proto::BASE, BASE_LIST_PROTOCOLS, &[0])?;
        let w = r.need(2)?;
        let n = w.get(1).copied().unwrap_or(0) as usize;
        let mut k = 0;
        for &word in w.get(2..).unwrap_or(&[]) {
            for b in word.to_le_bytes() {
                if k >= n.min(out.len()) {
                    return Ok(k);
                }
                if b != 0
                    && let Some(slot) = out.get_mut(k)
                {
                    *slot = b;
                    k += 1;
                }
            }
        }
        Ok(k)
    }

    /// Zet een stroomdomein in `state` ([`POWER_ON`], [`POWER_OFF`]). Op de
    /// Cix P1 opent de TF-A hierbij ook de interconnect voor de registers
    /// van dat blok: de eerste stap van elke bring-up, niet een
    /// energiebesparing achteraf.
    pub fn power_set(&mut self, domain: u32, state: u32) -> Result {
        self.call(proto::POWER, POWER_STATE_SET, &[0, domain, state])
            .map(|_| ())
    }

    /// De stand van een stroomdomein.
    pub fn power_state(&mut self, domain: u32) -> Result<u32> {
        let r = self.call(proto::POWER, POWER_STATE_GET, &[domain])?;
        Ok(r.need(2)?.get(1).copied().unwrap_or(0))
    }

    /// Het prestatieniveau van een domein.
    pub fn perf_level(&mut self, domain: u32) -> Result<u32> {
        let r = self.call(proto::PERF, PERF_LEVEL_GET, &[domain])?;
        Ok(r.need(2)?.get(1).copied().unwrap_or(0))
    }

    /// Zet het prestatieniveau van een domein.
    pub fn set_perf_level(&mut self, domain: u32, level: u32) -> Result {
        self.call(proto::PERF, PERF_LEVEL_SET, &[domain, level])
            .map(|_| ())
    }

    /// Zet een klok aan of uit.
    pub fn clock_enable(&mut self, id: u32, on: bool) -> Result {
        self.call(proto::CLOCK, CLOCK_CONFIG_SET, &[id, u32::from(on)])
            .map(|_| ())
    }

    /// De frequentie van een klok in hertz (64 bits, laag woord eerst).
    pub fn clock_rate(&mut self, id: u32) -> Result<u64> {
        let r = self.call(proto::CLOCK, CLOCK_RATE_GET, &[id])?;
        let w = r.need(3)?;
        let lo = u64::from(w.get(1).copied().unwrap_or(0));
        let hi = u64::from(w.get(2).copied().unwrap_or(0));
        Ok(lo | (hi << 32))
    }

    /// Zet een klok op `hz`, synchroon (flags 0: het platform antwoordt pas
    /// als de klok staat, en rondt af naar beneden), zoals Linux'
    /// `scmi_clock_rate_set`.
    pub fn set_clock_rate(&mut self, id: u32, hz: u64) -> Result {
        let (lo, hi) = (hz as u32, (hz >> 32) as u32);
        self.call(proto::CLOCK, CLOCK_RATE_SET, &[0, id, lo, hi])
            .map(|_| ())
    }

    /// Somt de sensoren op in `out`: per aanroep vanaf index `i` één
    /// beschrijving (het platform mag er meer sturen; we nemen de eerste en
    /// lopen door op het "remaining"-veld). Onbekende uitbreidingen van de
    /// descriptor (SCMI 3.x) raken we zo niet aan. Geeft het aantal.
    pub fn sensors(&mut self, out: &mut [Sensor]) -> Result<usize> {
        let mut k = 0;
        for i in 0..out.len() as u32 {
            let r = self.call(proto::SENSOR, SENSOR_DESCRIPTION_GET, &[i])?;
            let w = r.words();
            // status, num_flags (returned [15:0], remaining [31:16]), dan per
            // sensor: id, attr_low, attr_high, name[16].
            if w.len() < 9 || r.word(1) & 0xffff == 0 {
                return Ok(k);
            }
            let Some(slot) = out.get_mut(k) else {
                return Ok(k);
            };
            *slot = parse_sensor(&r);
            k += 1;
            if r.word(1) >> 16 == 0 {
                return Ok(k);
            }
        }
        Ok(k)
    }

    /// De scalaire waarde van sensor `id` (synchroon): 64 bits, in de
    /// eenheid maal 10^exponent van de beschrijving.
    pub fn reading(&mut self, id: u32) -> Result<i64> {
        let r = self.call(proto::SENSOR, SENSOR_READING_GET, &[id, 0])?;
        let w = r.need(3)?;
        let lo = u64::from(w.get(1).copied().unwrap_or(0));
        let hi = u64::from(w.get(2).copied().unwrap_or(0));
        Ok((lo | (hi << 32)) as i64)
    }
}

/// De message-header: msg [7:0], type [9:8] = 0 (command), protocol
/// [17:10], token [27:18].
fn header(proto: u8, msg: u8, token: u32) -> u32 {
    u32::from(msg) | (u32::from(proto) << 10) | ((token & 0x3ff) << 18)
}

fn parse_sensor(r: &Reply) -> Sensor {
    let hi = r.word(4);
    // Vijf bits met teken in [15:11]: naar boven schuiven en terug.
    let exponent = ((((hi >> 11) & 0x1f) as u8) << 3) as i8 >> 3;
    let mut name = [0u8; 16];
    for (k, chunk) in name.chunks_exact_mut(4).enumerate() {
        chunk.copy_from_slice(&r.word(5 + k).to_le_bytes());
    }
    Sensor {
        id: r.word(2),
        kind: (hi & 0xff) as u8,
        exponent,
        name,
    }
}

#[cfg(test)]
mod tests;
