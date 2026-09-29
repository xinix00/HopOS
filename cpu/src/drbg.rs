//! De gedeelde entropielaag van de kern: een Hash-DRBG op SHA-256
//! (`out_i = H(state ‖ ctr ‖ 0)`, `state' = H(state ‖ ctr ‖ 1)`), geseed uit
//! een hardware-TRNG ([`crate::trng`]) en anders uit timing-jitter.
//!
//! Het recept stond in de Go-kern byte-identiek dubbel (board/uefi en
//! board/hopslot) en is daar in één pakket gezet; hier staat het één keer,
//! met zijn SHA-256 erbij ([`sha256`]), zodat de kern geen crate van buiten
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

pub mod sha256 {
    //! SHA-256 (FIPS 180-4), getest tegen de vectoren van RFC 6234.
    //!
    //! Klein en zonder tabellen buiten de 64 rondeconstanten: de DRBG hasht
    //! per 32 uitvoerbytes twee blokken van 48 bytes, en dit is geen heet
    //! pad (een TLS-handshake trekt er een paar honderd bytes uit).

    /// De rondeconstanten: de eerste 32 bits van de breukdelen van de
    /// derdemachtswortels van de eerste 64 priemgetallen.
    const K: [u32; 64] = [
        0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4,
        0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe,
        0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f,
        0x4a7484aa, 0x5cb0a9dc, 0x76f988da, 0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7,
        0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc,
        0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b,
        0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070, 0x19a4c116,
        0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
        0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7,
        0xc67178f2,
    ];

    /// De beginwaarde: de breukdelen van de vierkantswortels van de eerste
    /// acht priemgetallen.
    const H0: [u32; 8] = [
        0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab,
        0x5be0cd19,
    ];

    /// Een lopende SHA-256.
    #[derive(Clone)]
    pub struct Sha256 {
        h: [u32; 8],
        buf: [u8; 64],
        fill: usize,
        len: u64,
    }

    impl Default for Sha256 {
        fn default() -> Self {
            Self::new()
        }
    }

    impl Sha256 {
        /// Een lege hash.
        #[must_use]
        pub const fn new() -> Self {
            Self {
                h: H0,
                buf: [0; 64],
                fill: 0,
                len: 0,
            }
        }

        /// Voegt `data` toe.
        pub fn update(&mut self, mut data: &[u8]) {
            self.len = self.len.wrapping_add(data.len() as u64);
            if self.fill > 0 {
                let take = (64 - self.fill).min(data.len());
                self.buf[self.fill..self.fill + take].copy_from_slice(&data[..take]);
                self.fill += take;
                data = &data[take..];
                if self.fill < 64 {
                    return;
                }
                let block = self.buf;
                compress(&mut self.h, &block);
                self.fill = 0;
            }
            let mut blocks = data.chunks_exact(64);
            for block in &mut blocks {
                let mut b = [0u8; 64];
                b.copy_from_slice(block);
                compress(&mut self.h, &b);
            }
            let rest = blocks.remainder();
            self.buf[..rest.len()].copy_from_slice(rest);
            self.fill = rest.len();
        }

        /// De digest; de hash is daarna op.
        #[must_use]
        pub fn finish(mut self) -> [u8; 32] {
            let bits = self.len.wrapping_mul(8);
            let mut pad = [0u8; 72];
            pad[0] = 0x80;
            // Opvullen tot 56 mod 64, dan de lengte in bits (big-endian).
            let n = if self.fill < 56 {
                56 - self.fill
            } else {
                120 - self.fill
            };
            pad[n..n + 8].copy_from_slice(&bits.to_be_bytes());
            let len = self.len;
            self.update(&pad[..n + 8]);
            self.len = len;
            let mut out = [0u8; 32];
            for (o, h) in out.chunks_exact_mut(4).zip(self.h) {
                o.copy_from_slice(&h.to_be_bytes());
            }
            out
        }
    }

    /// De hash van `data` in één keer.
    #[must_use]
    pub fn digest(data: &[u8]) -> [u8; 32] {
        let mut s = Sha256::new();
        s.update(data);
        s.finish()
    }

    /// De compressiefunctie over één blok van 64 bytes.
    fn compress(h: &mut [u32; 8], block: &[u8; 64]) {
        let mut w = [0u32; 64];
        for (wi, c) in w.iter_mut().zip(block.chunks_exact(4)) {
            *wi = u32::from_be_bytes([c[0], c[1], c[2], c[3]]);
        }
        for i in 16..64 {
            let s0 = w[i - 15].rotate_right(7) ^ w[i - 15].rotate_right(18) ^ (w[i - 15] >> 3);
            let s1 = w[i - 2].rotate_right(17) ^ w[i - 2].rotate_right(19) ^ (w[i - 2] >> 10);
            w[i] = w[i - 16]
                .wrapping_add(s0)
                .wrapping_add(w[i - 7])
                .wrapping_add(s1);
        }
        let [mut a, mut b, mut c, mut d, mut e, mut f, mut g, mut hh] = *h;
        for (k, wi) in K.iter().zip(w) {
            let s1 = e.rotate_right(6) ^ e.rotate_right(11) ^ e.rotate_right(25);
            let ch = (e & f) ^ (!e & g);
            let t1 = hh
                .wrapping_add(s1)
                .wrapping_add(ch)
                .wrapping_add(*k)
                .wrapping_add(wi);
            let s0 = a.rotate_right(2) ^ a.rotate_right(13) ^ a.rotate_right(22);
            let maj = (a & b) ^ (a & c) ^ (b & c);
            let t2 = s0.wrapping_add(maj);
            hh = g;
            g = f;
            f = e;
            e = d.wrapping_add(t1);
            d = c;
            c = b;
            b = a;
            a = t1.wrapping_add(t2);
        }
        for (x, v) in h.iter_mut().zip([a, b, c, d, e, f, g, hh]) {
            *x = x.wrapping_add(v);
        }
    }

    #[cfg(test)]
    mod tests {
        use super::*;

        fn hex(b: &[u8]) -> String {
            b.iter().map(|x| format!("{x:02x}")).collect()
        }

        /// RFC 6234 §8.5 TEST1, TEST2_1, TEST3 en TEST4 voor SHA-256, plus
        /// de lege invoer en FIPS 180-4's 896-bit-bericht.
        #[test]
        fn rfc6234_vectors() {
            let cases: [(&[u8], usize, &str); 5] = [
                (
                    b"",
                    1,
                    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
                ),
                (
                    b"abc",
                    1,
                    "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
                ),
                (
                    b"abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq",
                    1,
                    "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1",
                ),
                (
                    b"a",
                    1_000_000,
                    "cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0",
                ),
                (
                    b"01234567012345670123456701234567",
                    20,
                    "594847328451bdfa85056225462cc1d867d877fb388df0ce35f25ab5562bfbb5",
                ),
            ];
            for (msg, repeat, want) in cases {
                let mut s = Sha256::new();
                for _ in 0..repeat {
                    s.update(msg);
                }
                assert_eq!(hex(&s.finish()), want, "{repeat} x {msg:?}");
            }
            assert_eq!(
                hex(&digest(
                    b"abcdefghbcdefghicdefghijdefghijkefghijklfghijklmghijklmnhijklmnoijklmnopjklmnopqklmnopqrlmnopqrsmnopqrstnopqrstu"
                )),
                "cf5b16a778af8380036ce59e7b0492370b249b11e8f07a51afac45037afee9d1"
            );
        }

        #[test]
        fn split_updates_match_one_shot() {
            let data: Vec<u8> = (0..300u32).map(|i| (i * 7) as u8).collect();
            let want = digest(&data);
            for cut in [0, 1, 55, 56, 63, 64, 65, 128, 299] {
                let mut s = Sha256::new();
                s.update(&data[..cut]);
                s.update(&data[cut..]);
                assert_eq!(s.finish(), want, "cut at {cut}");
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
    fn the_core_drbg_lives_in_a_local() {
        let mut b = [0u8; 8];
        init(no_fill, ticks);
        assert_eq!(source(), Source::Jitter);
        read(&mut b).unwrap();
    }
}
