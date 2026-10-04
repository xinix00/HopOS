//! De RNG200 van de BCM2711 (Pi 4) en de BCM2712 (Pi 5): het on-chip
//! TRNG-blok, DT `brcm,bcm2711-rng200`.
//!
//! De specificatie is `OLD/metal/board/raspi/rng.go`, en daarachter Linux
//! `drivers/char/hw_random/iproc-rng200.c`: RBGEN aan in RNG_CTRL, de
//! warm-up (RBG uit, IRQ-status wissen, RBG en RNG soft-resetten, RBG weer
//! aan), en dan per 32-bit-woord uit FIFO_DATA zodra FIFO_COUNT[7:0] niet
//! nul is. LET OP: dit is de iproc-variant (teller in FIFO_COUNT op 0x24,
//! data op 0x20), niet het oudere `bcm2835-rng`-blok (teller in RNG_STATUS
//! op 0x4).
//!
//! Dit bezit het registermodel en één trekking. Niet van hier: het adres
//! (het board, uit zijn DTB), en wat er gebeurt als het blok niets levert.
//! Dat is de DRBG van de kern (`cpu::drbg`), die dan op timing-jitter zaait;
//! de PRNG-terugval die Go per trekking had, bestaat hier dus niet meer.
//!
//! Sans-I/O: de driver praat via [`dev::Io`], op ijzer [`dev::Mmio`] en op
//! de host een nep-blok met een FIFO (tests.rs). Eén eigenaar (de DRBG op de
//! executor van core 0), dus `&mut self` en geen slot; Go had hier een
//! `sync.Mutex` omdat de Go-runtime vanaf elke core trok.
//!
//! Twee controles die Go niet had, allebei goedkoop:
//!
//! - RNG_CTRL moet na de start RBGEN teruglezen. Een blok dat dat niet doet,
//!   is er niet (QEMU `raspi4b` modelleert geen RNG200) en dan wachten we
//!   niet eerst de hele warm-up af.
//! - De continue toets van FIPS 140-2 §4.9.2: twee gelijke opeenvolgende
//!   woorden is een vastgelopen bron (of een bus die steeds hetzelfde
//!   antwoordt), geen entropie. De kans op een vals alarm is 2^-32 per
//!   woord.
//!
//! En één die van Linux komt (`iproc_rng200_read`): meldt RNG_INT_STATUS
//! een NIST-fout of een master-lockout, dan eerst één herstart, en pas bij
//! een tweede fout opgeven.

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
use dev::Io;

#[cfg(test)]
mod tests;

/// RNG_CTRL: RBGEN in [12:0].
pub const RNG_CTRL: u64 = 0x00;
/// RNG_SOFT_RESET: bit 0.
pub const RNG_SOFT_RESET: u64 = 0x04;
/// RBG_SOFT_RESET: bit 0.
pub const RBG_SOFT_RESET: u64 = 0x08;
/// RNG_INT_STATUS: schrijf alle enen om te wissen.
pub const RNG_INT_STATUS: u64 = 0x18;
/// RNG_FIFO_DATA: één 32-bit-woord per lezing.
pub const RNG_FIFO_DATA: u64 = 0x20;
/// RNG_FIFO_COUNT: het aantal klaarstaande woorden in [7:0].
pub const RNG_FIFO_COUNT: u64 = 0x24;
/// De grootte van het registerblok (DT `reg = <... 0x28>`).
pub const SIZE: u64 = 0x28;

/// Het RBGEN-veld van RNG_CTRL.
const RBGEN_MASK: u32 = 0x0000_1FFF;
/// RBGEN aan.
const RBGEN_ENABLE: u32 = 0x0000_0001;
/// De soft-reset-bit van beide reset-registers.
const SOFT_RESET: u32 = 0x0000_0001;
/// Het tellerveld van RNG_FIFO_COUNT.
const FIFO_COUNT_MASK: u32 = 0x0000_00FF;
/// RNG_INT_STATUS: de master-fail-lockout (Linux
/// `RNG_INT_STATUS_MASTER_FAIL_LOCKOUT_IRQ_MASK`).
const STATUS_MASTER_FAIL: u32 = 0x8000_0000;
/// RNG_INT_STATUS: een gefaalde NIST-toets (Linux
/// `RNG_INT_STATUS_NIST_FAIL_IRQ_MASK`).
const STATUS_NIST_FAIL: u32 = 0x0000_0020;

/// Hoe lang het ALLEREERSTE woord na de start mag duren. De
/// ring-oscillatoren bouwen eerst entropie op, en dat duurt op de BCM2711
/// aantoonbaar langer dan op de BCM2712: in Go (gemeten 11-07) viel de Pi 4
/// bij de eerste trekking terug met een grens van 200 000 polls, de Pi 5
/// niet, en de grens ging naar 5 000 000 polls. Hier een tijd in plaats van
/// een telling, zodat de grens niet van de snelheid van een MMIO-lezing
/// afhangt; Linux wacht per trekking hoogstens een seconde
/// (`MAX_IDLE_TIME`), wij bij de start twee.
pub const WARMUP_NS: u64 = 2_000_000_000;
/// Hoe lang elk volgend woord mag duren. Na de warm-up levert de RBG
/// continu; dit raakt alleen bij een storing.
pub const WORD_NS: u64 = 20_000_000;

/// Waarom de RNG200 niets leverde.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Error {
    /// RNG_CTRL las RBGEN niet terug na de start: hier antwoordt geen
    /// RNG200 (de gelezen waarde staat erbij).
    Absent(u32),
    /// De FIFO bleef leeg binnen de grens; `warmup` = het was het eerste
    /// woord na de start.
    Timeout {
        /// Was dit het eerste woord na de start?
        warmup: bool,
    },
    /// Twee gelijke opeenvolgende woorden (de continue toets).
    Stuck(u32),
    /// RNG_INT_STATUS bleef na een herstart een fout melden.
    Health(u32),
    /// De doelbuffer was leeg.
    Empty,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Absent(v) => write!(
                f,
                "RNG_CTRL reads {v:#x} after enabling RBGEN, no RNG200 answers here"
            ),
            Self::Timeout { warmup: true } => write!(
                f,
                "FIFO stayed empty for {} ms after the warm-up",
                WARMUP_NS / 1_000_000
            ),
            Self::Timeout { warmup: false } => write!(
                f,
                "FIFO stayed empty for {} ms between words",
                WORD_NS / 1_000_000
            ),
            Self::Stuck(w) => write!(f, "two equal words in a row ({w:#010x}), source stuck"),
            Self::Health(s) => write!(f, "RNG_INT_STATUS {s:#x} after a restart (NIST or lockout)"),
            Self::Empty => f.write_str("empty destination buffer"),
        }
    }
}

/// Het resultaat van deze driver.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// Eén RNG200: de registers, de klok en of de warm-up al gedaan is.
pub struct Rng200<R: Io> {
    regs: R,
    now: fn() -> u64,
    started: bool,
    last: Option<u32>,
}

impl<R: Io> Rng200<R> {
    /// Een RNG200 die nog niet gestart is; de eerste [`fill`](Self::fill)
    /// start hem. `now` geeft monotone nanoseconden.
    pub const fn new(regs: R, now: fn() -> u64) -> Self {
        Self {
            regs,
            now,
            started: false,
            last: None,
        }
    }

    /// De warm-up van iproc-rng200: RBG uit, IRQ-status wissen, RBG en RNG
    /// soft-resetten (in die volgorde aan, in omgekeerde volgorde uit), RBG
    /// weer aan. Daarna vult de FIFO zich. Faalt als RBGEN niet terugleest.
    pub fn restart(&mut self) -> Result {
        self.enable(false);
        self.regs.write(RNG_INT_STATUS, 0xFFFF_FFFF);
        self.set(RBG_SOFT_RESET, true);
        self.set(RNG_SOFT_RESET, true);
        self.set(RNG_SOFT_RESET, false);
        self.set(RBG_SOFT_RESET, false);
        self.enable(true);
        self.last = None;
        let ctrl = self.regs.read(RNG_CTRL);
        if ctrl & RBGEN_MASK != RBGEN_ENABLE {
            self.started = false;
            return Err(Error::Absent(ctrl));
        }
        self.started = true;
        Ok(())
    }

    /// Vult `dst` volledig uit de FIFO, per woord little-endian (zoals Go).
    /// Start het blok bij de eerste aanroep. Een fout laat `dst` half
    /// gevuld achter; de aanroeper gebruikt hem dan niet.
    pub fn fill(&mut self, dst: &mut [u8]) -> Result {
        if dst.is_empty() {
            return Err(Error::Empty);
        }
        let mut warmup = false;
        if !self.started {
            self.restart()?;
            warmup = true;
        }
        self.health()?;
        for chunk in dst.chunks_mut(4) {
            let w = self.word(warmup)?;
            warmup = false;
            let b = w.to_le_bytes();
            chunk.copy_from_slice(b.get(..chunk.len()).unwrap_or(&b));
        }
        Ok(())
    }

    /// Eén woord uit de FIFO binnen [`WARMUP_NS`] (het eerste na de start)
    /// of [`WORD_NS`], door de continue toets.
    fn word(&mut self, warmup: bool) -> Result<u32> {
        let budget = if warmup { WARMUP_NS } else { WORD_NS };
        if !dev::poll_until(self.now, budget, || {
            self.regs.read(RNG_FIFO_COUNT) & FIFO_COUNT_MASK != 0
        }) {
            return Err(Error::Timeout { warmup });
        }
        let w = self.regs.read(RNG_FIFO_DATA);
        if self.last == Some(w) {
            return Err(Error::Stuck(w));
        }
        self.last = Some(w);
        Ok(w)
    }

    /// De toets van Linux: een NIST-fout of lockout krijgt één herstart;
    /// blijft hij, dan is de bron stuk.
    fn health(&mut self) -> Result {
        let bad = STATUS_MASTER_FAIL | STATUS_NIST_FAIL;
        if self.regs.read(RNG_INT_STATUS) & bad == 0 {
            return Ok(());
        }
        self.restart()?;
        let s = self.regs.read(RNG_INT_STATUS);
        if s & bad != 0 {
            return Err(Error::Health(s));
        }
        Ok(())
    }

    /// Zet RBGEN aan of uit; de andere RBGEN-bits op 0.
    fn enable(&mut self, on: bool) {
        let mut v = self.regs.read(RNG_CTRL) & !RBGEN_MASK;
        if on {
            v |= RBGEN_ENABLE;
        }
        self.regs.write(RNG_CTRL, v);
    }

    /// Zet of wist de soft-reset-bit van `reg`.
    fn set(&mut self, reg: u64, on: bool) {
        let v = self.regs.read(reg);
        let v = if on { v | SOFT_RESET } else { v & !SOFT_RESET };
        self.regs.write(reg, v);
    }
}
