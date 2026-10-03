//! De gedeelde entropielaag van de kern: een Hash-DRBG op SHA-256
//! (`out_i = H(state ‖ ctr ‖ 0)`, `state' = H(state ‖ ctr ‖ 1)`), geseed uit
//! een hardware-TRNG ([`crate::trng`]) en anders uit timing-jitter.
//!
//! Het recept stond in de Go-kern byte-identiek dubbel (board/uefi en
//! board/hopslot) en is daar in één pakket gezet; hier staat het één keer,
//! op de SHA-256 van [`abi::sha256`], zodat de kern geen crate van buiten
//! nodig heeft (handboek §8). Het board kiest de bron:
//!
//! - de kern: [`trng::fill`] (FEAT_RNG, anders SMCCC TRNG via EL3);
//! - een gekooide app: [`trng::fill_cpu`] (alleen RNDR: HCR_EL2.TSC trapt
//!   elke SMC uit de kooi);
//! - QEMU-TCG heeft geen FEAT_RNG en geen DEN 0098: jitter.
//!
//! Jitter is op echt silicium een serieuze bron (cache-, branch- en
//! DRAM-variatie in de meetlussen, het jitterentropy-principe); op QEMU/TCG
//! is hij zwakker, en daar draait ook geen productie-TLS. Is er een
//! hardwarebron, dan herzaait de DRBG zich elke [`RESEED_INTERVAL`] bytes
//! met een verse draw (voorwaartse onvoorspelbaarheid); jitter herzaait
//! niet (te duur, de boot-seed volstaat).
//!
//! # Eigendom
//!
//! In Go stond de staat onder een `sync.Mutex`, en de doc legde uit waarom
//! geen atomics: de sectie moet ATOMAIR zijn (lees state, hash, schrijf
//! state), niet alleen per woord coherent. Twee aanroepers die dezelfde
//! `(state, ctr)` zien, krijgen dezelfde output, en een DRBG die zijn
//! keystream herhaalt is geen DRBG meer. Hier is die sectie een lening uit
//! een [`LocalCell`] (PORT §3, de rij `drbg.mu`): op één core draait tussen
//! twee `.await`-punten precies één taak, en [`read`] heeft geen `.await`,
//! dus de executor ís het slot. Een ISR leest nooit uit de DRBG.
//!
//! Bij meer HOP-cores komt er één DRBG per core (Linux' per-cpu), elk met
//! een eigen seed; nu is er één, op de executor van core 0.

use crate::trng;
use abi::sha256;
use core::fmt;
use sync::LocalCell;

/// Bytes tussen twee TRNG-herzaaiingen.
pub const RESEED_INTERVAL: u64 = 1 << 20;

/// Het aantal hash-rondes van de jitter-seed.
const JITTER_ROUNDS: u64 = 512;

/// Waar de seed vandaan kwam.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Source {
    /// Een hardwarebron; de DRBG herzaait zich eruit.
    Hardware(trng::Kind),
    /// Timing-jitter uit de teller: geen hardware-entropie.
    Jitter,
}

impl fmt::Display for Source {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Hardware(k) => f.write_str(k.name()),
            Self::Jitter => f.write_str("jitter"),
        }
    }
}

/// Waarom de DRBG niet leverde.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum Error {
    /// [`init`] is nog niet geroepen: een ongeseede DRBG levert een
    /// voorspelbare stroom, en dat is erger dan geen stroom.
    Unseeded,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Unseeded => f.write_str("drbg: read before init (unseeded)"),
        }
    }
}

/// Het resultaat van deze module.
pub type Result<T = (), E = Error> = core::result::Result<T, E>;

/// De staat van één Hash-DRBG.
pub struct Drbg {
    state: [u8; 32],
    ctr: u64,
    source: Source,
    since_reseed: u64,
    fill: Option<trng::Fill>,
    seeded: bool,
}

impl Default for Drbg {
    fn default() -> Self {
        Self::new()
    }
}

impl Drbg {
    /// Een ongeseede DRBG; [`read`](Self::read) weigert tot
    /// [`init`](Self::init).
    #[must_use]
    pub const fn new() -> Self {
        Self {
            state: [0; 32],
            ctr: 0,
            source: Source::Jitter,
            since_reseed: 0,
            fill: None,
            seeded: false,
        }
    }

    /// Seedt de DRBG: uit `fill` (de hardwarebron van het board), en als die
    /// faalt uit timing-jitter op `counter` (de arch-teller, CNTPCT).
    ///
    /// De seed-werkruimte ligt op de stack en wordt na gebruik gewist; de
    /// vulfunctie krijgt hem leeg (de Go-test `TestInitWorkspaceIsCleared`).
    pub fn init(&mut self, fill: trng::Fill, counter: fn() -> u64) {
        let mut seed = [0u8; 48];
        self.fill = Some(fill);
        self.source = match fill(&mut seed) {
            Ok(k) => Source::Hardware(k),
            Err(_) => {
                jitter_seed(&mut seed, counter);
                Source::Jitter
            }
        };
        self.state = sha256::digest(&seed);
        wipe(&mut seed);
        self.ctr = 0;
        self.since_reseed = 0;
        self.seeded = true;
    }

    /// De bron van de seed, voor de boot-log.
    #[must_use]
    pub fn source(&self) -> Source {
        self.source
    }

    /// Vult `b` uit de DRBG.
    pub fn read(&mut self, b: &mut [u8]) -> Result {
        if !self.seeded {
            return Err(Error::Unseeded);
        }
        if self.source != Source::Jitter && self.since_reseed >= RESEED_INTERVAL {
            self.reseed();
        }
        let mut input = [0u8; 48];
        for chunk in b.chunks_mut(32) {
            self.ctr = self.ctr.wrapping_add(1);
            input[..32].copy_from_slice(&self.state);
            input[32..40].copy_from_slice(&self.ctr.to_le_bytes());
            input[40] = 0;
            let out = sha256::digest(&input);
            input[40] = 1;
            self.state = sha256::digest(&input);
            chunk.copy_from_slice(out.get(..chunk.len()).unwrap_or(&out));
            self.since_reseed = self.since_reseed.saturating_add(chunk.len() as u64);
        }
        wipe(&mut input);
        Ok(())
    }

    /// Mengt een verse hardware-draw in de staat: `state' = H(state ‖
    /// fresh)`. Faalt de bron even, dan blijft de oude staat staan (nog
    /// steeds veilig) en proberen we bij de volgende drempel opnieuw.
    fn reseed(&mut self) {
        let mut fresh = [0u8; 24];
        if let Some(fill) = self.fill
            && fill(&mut fresh).is_ok()
        {
            let mut input = [0u8; 56];
            input[..32].copy_from_slice(&self.state);
            input[32..].copy_from_slice(&fresh);
            self.state = sha256::digest(&input);
            wipe(&mut input);
        }
        wipe(&mut fresh);
        self.since_reseed = 0;
    }
}

/// Vult `dst` uit timing-jitter: [`JITTER_ROUNDS`] hash-rondes waarvan de
/// individuele DUUR (de CNTPCT-delta per ronde) de entropie levert; de
/// teller zelf gaat als monotone basis mee. De terugvaller als er geen
/// hardware-TRNG is (QEMU virt, de Pi's in de kooi, de Neoverse N1 van de
/// Altra zonder DEN 0098).
fn jitter_seed(dst: &mut [u8], counter: fn() -> u64) {
    let mut pool = [0u8; 48];
    let mut st = [0u8; 32];
    for i in 0..JITTER_ROUNDS {
        pool[32..40].copy_from_slice(&counter().to_le_bytes());
        pool[40..48].copy_from_slice(&i.to_le_bytes());
        pool[..32].copy_from_slice(&st);
        st = sha256::digest(&pool);
    }
    for chunk in dst.chunks_mut(32) {
        chunk.copy_from_slice(st.get(..chunk.len()).unwrap_or(&st));
        st = sha256::digest(&st);
    }
    wipe(&mut pool);
    wipe(&mut st);
}

/// Wist seed-materiaal. `black_box` houdt de compiler ervan af de schrijf
/// als dode code weg te halen: de buffer gaat daarna immers niet meer in
/// gebruik.
fn wipe(b: &mut [u8]) {
    b.fill(0);
    core::hint::black_box(b);
}

/// De DRBG van deze core (nu: de enige, op core 0).
static DRBG: LocalCell<Drbg> = LocalCell::cell(Drbg::new());

/// Seedt de DRBG van de kern; het board roept dit bij boot, vóór de eerste
/// `spawn` (de runtime-once uit Go bestaat niet, handboek §2).
pub fn init(fill: trng::Fill, counter: fn() -> u64) {
    DRBG.borrow_mut().init(fill, counter);
}

/// Vult `b` uit de DRBG van de kern.
pub fn read(b: &mut [u8]) -> Result {
    DRBG.borrow_mut().read(b)
}

/// De gekozen entropiebron ("rndr", "smccc-trng" of "jitter"), voor de
/// discovery-regel en de boot-log.
#[must_use]
pub fn source() -> Source {
    DRBG.borrow().source()
}

/// Is de DRBG van de kern geseed? Een board dat in `discover` niets
/// zaaide, krijgt van de binary alsnog [`seed_from_cpu`]: een slot hoort
/// zijn zaad te krijgen (`CTRL_RNG_SEED`), op elk board.
#[must_use]
pub fn is_seeded() -> bool {
    DRBG.borrow().seeded
}

/// Zaait de DRBG van de kern uit de standaardbronnen van de CPU
/// ([`trng::fill`]: RNDR, anders de SMCCC TRNG van de firmware), en anders
/// uit jitter. Voor de boards zonder eigen TRNG-blok: UEFI (EDK2, de O6N
/// met FEAT_RNG, de Altra met TF-A), QEMU virt en de Mac mini. De Pi's en
/// de Radxa zaaien uit hun SoC-blok ([`trng::Kind::Soc`]).
///
/// Geeft de consoleregel terug (met marker); het board drukt hem af. Tot
/// 30-09 zaaide geen van deze boards: de O6N bootte zonder één `trng:`-regel
/// en de DRBG bleef ongeseed.
pub fn seed_from_cpu(counter: fn() -> u64) -> Seeded {
    init(trng::fill, counter);
    let source = source();
    // De reden van een jitter-zaad: nog één poging, alleen voor de regel.
    let why = match source {
        Source::Jitter => trng::fill(&mut [0u8; 8]).err(),
        Source::Hardware(_) => None,
    };
    Seeded {
        source,
        why,
        monitor: trng::has_monitor(),
    }
}

/// De uitkomst van [`seed_from_cpu`], als consoleregel.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub struct Seeded {
    /// Waar de seed vandaan kwam.
    pub source: Source,
    /// Waarom het geen hardware werd (alleen bij jitter).
    pub why: Option<trng::Error>,
    /// Zit er een EL3-monitor onder de kern (de weg naar de SMCCC TRNG)?
    pub monitor: bool,
}

impl fmt::Display for Seeded {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self.source {
            Source::Hardware(trng::Kind::Rndr) => f.write_str(
                "trng: rndr online, the kernel DRBG is seeded from rndr (FEAT_RNG) HOPOS_RNG_RNDR_UP",
            ),
            Source::Hardware(trng::Kind::SmcccTrng) => f.write_str(
                "trng: smccc-trng online, the kernel DRBG is seeded from the SMCCC TRNG (DEN 0098) HOPOS_RNG_SMCCC_UP",
            ),
            // Nooit uit `trng::fill`: een SoC-blok zaait zijn board zelf.
            Source::Hardware(k @ trng::Kind::Soc(_)) => {
                write!(f, "trng: {k} online, the kernel DRBG is seeded from {k}")
            }
            Source::Jitter => {
                write!(
                    f,
                    "trng: WARNING the kernel DRBG is seeded from timer jitter, not hardware entropy: "
                )?;
                match self.why {
                    Some(e) => write!(f, "{e}")?,
                    None => f.write_str("no answer")?,
                }
                write!(
                    f,
                    " (EL3 monitor: {}); slots get jitter seed, avoid high-value secrets on this node HOPOS_RNG_INSECURE",
                    if self.monitor { "yes" } else { "no" }
                )
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicBool, AtomicU64, Ordering::SeqCst};

    fn hex(b: &[u8]) -> String {
        b.iter().map(|x| format!("{x:02x}")).collect()
    }

    fn fixed_fill(b: &mut [u8]) -> trng::Result<trng::Kind> {
        for (i, x) in b.iter_mut().enumerate() {
            *x = i as u8;
        }
        Ok(trng::Kind::Rndr)
    }

    fn no_fill(_: &mut [u8]) -> trng::Result<trng::Kind> {
        Err(trng::Error::NoSource)
    }

    static TICKS: AtomicU64 = AtomicU64::new(0);
    fn ticks() -> u64 {
        TICKS.fetch_add(1, SeqCst) + 1
    }
    /// De teller van de Go-test, met een eigen stand: de andere tests
    /// draaien parallel op [`ticks`].
    static GO_TICKS: AtomicU64 = AtomicU64::new(0);
    fn go_counter() -> u64 {
        (GO_TICKS.fetch_add(1, SeqCst) + 1) * 101 + 19
    }

    /// De vectoren van `drbg_test.go` (onafhankelijk berekend met Python's
    /// hashlib, 48-byte-seed, `Hash(state ‖ le-ctr ‖ suffix)` over 48 bytes).
    #[test]
    fn go_generator_vectors() {
        let mut d = Drbg::new();
        d.init(fixed_fill, go_counter);
        let mut got = [0u8; 80];
        d.read(&mut got).unwrap();
        assert_eq!(
            hex(&got),
            "608c25ca8a54d6b4968d01a847e7f07a373bef7e6dccb225f74be64a6f8109599e9dec39542d75a9a201e2985ea2f8d784531d4b3ff71d517ccf9745a44159fe305a5424184b6f0f3ab0a60dd683296e"
        );
        assert_eq!(d.source(), Source::Hardware(trng::Kind::Rndr));

        GO_TICKS.store(0, SeqCst);
        let mut d = Drbg::new();
        d.init(no_fill, go_counter);
        let mut got = [0u8; 32];
        d.read(&mut got).unwrap();
        assert_eq!(
            hex(&got),
            "d2945313752ac54b559edd5bde8340381a746e32670dec5c84ef506d5ac14712"
        );
        assert_eq!(d.source(), Source::Jitter);
        assert_eq!(d.source().to_string(), "jitter");
    }

    #[test]
    fn fill_callback_gets_a_clean_workspace() {
        static DIRTY: AtomicBool = AtomicBool::new(false);
        fn check(b: &mut [u8]) -> trng::Result<trng::Kind> {
            if b.iter().any(|&v| v != 0) {
                DIRTY.store(true, SeqCst);
            }
            b[0] = 0x37;
            Ok(trng::Kind::SmcccTrng)
        }
        let mut d = Drbg::new();
        d.init(check, ticks);
        d.init(check, ticks);
        assert!(!DIRTY.load(SeqCst));
    }

    #[test]
    fn unseeded_refuses() {
        let mut d = Drbg::new();
        let mut b = [0u8; 4];
        assert_eq!(d.read(&mut b), Err(Error::Unseeded));
    }

    #[test]
    fn reseed_after_the_interval_changes_the_stream() {
        let mut a = Drbg::new();
        let mut b = Drbg::new();
        a.init(fixed_fill, ticks);
        b.init(fixed_fill, ticks);
        // Beide tot vlak onder de drempel: identiek.
        let mut x = [0u8; 32];
        let mut y = [0u8; 32];
        a.since_reseed = RESEED_INTERVAL - 1;
        b.since_reseed = RESEED_INTERVAL - 1;
        a.read(&mut x).unwrap();
        b.read(&mut y).unwrap();
        assert_eq!(x, y);
        // Nu over de drempel: a herzaait, b niet (jitter herzaait nooit).
        b.source = Source::Jitter;
        a.read(&mut x).unwrap();
        b.read(&mut y).unwrap();
        assert_ne!(x, y);
        assert!(a.since_reseed <= 32);
    }

    #[test]
    fn no_repeats_across_reads() {
        let mut d = Drbg::new();
        d.init(fixed_fill, ticks);
        let mut x = [0u8; 32];
        let mut y = [0u8; 32];
        d.read(&mut x).unwrap();
        d.read(&mut y).unwrap();
        assert_ne!(x, y);
    }

    #[test]
    fn seeded_lines_carry_their_marker() {
        let line = |source, why| {
            Seeded {
                source,
                why,
                monitor: false,
            }
            .to_string()
        };
        assert!(line(Source::Hardware(trng::Kind::Rndr), None).ends_with("HOPOS_RNG_RNDR_UP"));
        assert!(
            line(Source::Hardware(trng::Kind::SmcccTrng), None).ends_with("HOPOS_RNG_SMCCC_UP")
        );
        let j = line(Source::Jitter, Some(trng::Error::NoSource));
        assert!(j.contains("no FEAT_RNG") && j.contains("EL3 monitor: no"));
        assert!(j.ends_with("HOPOS_RNG_INSECURE"));
    }

    #[test]
    fn the_core_drbg_lives_in_a_local() {
        let mut b = [0u8; 8];
        init(no_fill, ticks);
        assert!(is_seeded());
        assert_eq!(source(), Source::Jitter);
        read(&mut b).unwrap();
        // Op de host is er geen RNDR en geen EL3: jitter, met de reden.
        let s = seed_from_cpu(ticks);
        assert_eq!(s.source, Source::Jitter);
        assert_eq!(s.why, Some(trng::Error::NoSource));
        assert!(is_seeded());
    }
}
