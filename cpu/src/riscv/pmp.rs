//! De begrenzende helft van de RISC-V-kooi: de PMP-whitelist, als TOR.
//!
//! De Rust-vorm van `kern/cage/cage.go` (`Encode`). De C906 heeft geen
//! H-extensie, dus geen stage-2; de kooi is een PMP-whitelist die machine
//! mode programmeert vóór hij een slot binnenlaat:
//!
//! - PMP matcht op de laagste index eerst: de vensters staan vóór de
//!   afsluitende deny-all.
//! - **Eén adresseringsmodus: TOR.** NAPOT kan alleen een macht van twee op
//!   zijn eigen maat; een job van 124 MB werd 128 MB en paste op de LicheeRV
//!   nergens uitgelijnd. TOR beschrijft elk bereik met twee entries (onder-
//!   en bovengrens). Eén vorm is er één om te snappen en één om te testen.
//! - **De entries zijn NIET gelockt**, bewust: PMP bindt S- en U-mode altijd;
//!   de L-bit bindt daarbovenop machine mode, en dan zou de switcher die
//!   twee bewoners afwisselt niet buiten hun partities kunnen wonen. De
//!   invariant zit in de privilege-grens: een app in S-mode kan PMP niet
//!   aanraken.
//! - **Teruglezen vóór de sprong**: de switcher schrijft `pmpcfg0` en leest
//!   hem terug ([`Encoded::cfg`] is de referentie); een kooi die niet
//!   aantoonbaar staat, wordt niet betreden.
//!
//! Op silicium bewezen (LicheeRV, 30-07, toen nog gelockt en in M-mode): de
//! verboden store trapt met mcause 7. TOR zelf is op dat silicium NOOIT
//! geldig gemeten (de eerste "TOR matcht niet" leunde op niet-gedrainde
//! writes); het faalpad is veilig: matcht een TOR-entry niet, dan valt de
//! toegang in de deny-all en faultt de app meteen.
//!
//! Wat deze module over het silicium aanneemt (entries, adresbreedte,
//! korrel) staat in [`Profile`]: implementation-defined in de spec, dus een
//! getal van de CPU en niet van de codering.

use core::fmt;

/// De implementation-defined eigenschappen van de PMP van een CPU.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Profile {
    /// Hoeveel entries er zeker zijn.
    pub entries: usize,
    /// De fysieke adresbreedte die de deny-all moet dekken.
    pub pa_bits: u32,
    /// De korrel van een TOR-grens in bytes. Bij een korrel > 4 leest het
    /// silicium de lage bits van `pmpaddr` als nul, dus een grens die daar
    /// niet op valt dekt STIL een ander bereik: dat eisen we af.
    pub grain: u64,
}

/// De XuanTie C906: 8 entries (T-Head levert er mogelijk 16, niet gemeten;
/// we rekenen met wat vaststaat), 40 PA-bits, korrel 4 KB (Go, `cpu/thead`).
pub const C906: Profile = Profile {
    entries: 8,
    pa_bits: 40,
    grain: 0x1000,
};

/// QEMU virt (`rv64`): 16 entries. We rekenen met de acht van de C906,
/// zodat een kooi die op QEMU past ook op het ijzer past. De deny-all dekt
/// 2^54 bytes: `pmpaddr` draagt op RV64 adresbits 55..2 in 54 bits, dus een
/// grens van 2^56 zou als nul terugkomen en de deny-all zou niets dekken.
pub const QEMU: Profile = Profile {
    entries: 8,
    pa_bits: 54,
    grain: 0x1000,
};

/// Het maximum aantal entries dat [`Encoded`] draagt: zoveel woorden heeft
/// het regime in het ctx-blok (`pmpaddr0..7`, `CTX_REGIME_RV_WORDS`).
pub const MAX_ENTRIES: usize = 8;

/// Eén toegestaan venster: basis, maat en rechten.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Window {
    /// De basis (op de korrel).
    pub base: u64,
    /// De maat (een veelvoud van de korrel, niet nul).
    pub size: u64,
    /// Lezen.
    pub r: bool,
    /// Schrijven.
    pub w: bool,
    /// Uitvoeren.
    pub x: bool,
}

const CFG_R: u64 = 1 << 0;
const CFG_W: u64 = 1 << 1;
const CFG_X: u64 = 1 << 2;
/// A = OFF: de ondergrens van een TOR-paar (een `pmpaddr` zonder match).
const CFG_A_OFF: u64 = 0;
/// A = TOR: `[pmpaddr[k-1], pmpaddr[k])`.
const CFG_A_TOR: u64 = 1 << 3;

/// Wat de switcher wegschrijft: `pmpaddr0..7` en `pmpcfg0`, plus hoeveel
/// entries er echt gebruikt zijn. De ongebruikte adressen zijn nul en hun
/// cfg-bytes OFF.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub struct Encoded {
    /// `pmpaddr0..7`.
    pub addr: [u64; MAX_ENTRIES],
    /// `pmpcfg0`: acht cfg-bytes, en ook de referentie voor het teruglezen.
    pub cfg: u64,
    /// Gebruikte entries.
    pub used: usize,
}

/// Waarom een plan niet te coderen is.
#[derive(Copy, Clone, Debug, PartialEq, Eq)]
pub enum Error {
    /// Een venster zonder maat.
    Empty {
        /// Het venster.
        index: usize,
    },
    /// Een grens buiten de korrel.
    Grain {
        /// Het venster.
        index: usize,
        /// De basis.
        base: u64,
        /// Het einde.
        end: u64,
    },
    /// Het einde loopt over.
    Overflow {
        /// Het venster.
        index: usize,
    },
    /// Meer entries nodig dan er zijn.
    TooMany {
        /// Nodig, inclusief de deny-all.
        need: usize,
        /// Beschikbaar.
        have: usize,
    },
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match *self {
            Self::Empty { index } => write!(f, "cage: window {index}: size 0"),
            Self::Grain { index, base, end } => write!(
                f,
                "cage: window {index}: TOR bounds {base:#x}..{end:#x} are not on the PMP grain"
            ),
            Self::Overflow { index } => write!(f, "cage: window {index}: end overflows"),
            Self::TooMany { need, have } => write!(
                f,
                "cage: windows need {need} PMP entries (incl. deny-all), there are {have}"
            ),
        }
    }
}

fn perm(w: &Window) -> u64 {
    let mut b = 0;
    if w.r {
        b |= CFG_R;
    }
    if w.w {
        b |= CFG_W;
    }
    if w.x {
        b |= CFG_X;
    }
    b
}

/// Codeert de vensters als TOR-paren plus de deny-all, voor `profile`.
///
/// De rekenkunde staat hier en is op de host getest; de switcher schrijft
/// alleen weg wat hier staat en leest `pmpcfg0` terug. Dezelfde
/// arbeidsdeling als in Go (`cage.Encode`).
pub fn encode(windows: &[Window], profile: Profile) -> Result<Encoded, Error> {
    let have = profile.entries.min(MAX_ENTRIES);
    let need = 2 * windows.len() + 2;
    if need > have {
        return Err(Error::TooMany { need, have });
    }
    let mut out = Encoded {
        addr: [0; MAX_ENTRIES],
        cfg: 0,
        used: need,
    };
    let mut put = |k: usize, addr: u64, cfg: u64| {
        if let Some(a) = out.addr.get_mut(k) {
            *a = addr >> 2;
        }
        out.cfg |= cfg << (8 * k);
    };
    for (i, w) in windows.iter().enumerate() {
        if w.size == 0 {
            return Err(Error::Empty { index: i });
        }
        let end = w
            .base
            .checked_add(w.size)
            .ok_or(Error::Overflow { index: i })?;
        if !w.base.is_multiple_of(profile.grain) || !w.size.is_multiple_of(profile.grain) {
            return Err(Error::Grain {
                index: i,
                base: w.base,
                end,
            });
        }
        put(2 * i, w.base, CFG_A_OFF);
        put(2 * i + 1, end, CFG_A_TOR | perm(w));
    }
    // De deny-all: TOR van 0 tot de hele PA-ruimte, zonder rechten. Ook
    // ongelockt: voor S-mode is een entry zonder rechten een harde weigering,
    // en machine mode blijft er vrij van.
    let k = 2 * windows.len();
    put(k, 0, CFG_A_OFF);
    put(k + 1, 1u64 << profile.pa_bits, CFG_A_TOR);
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    const PART: Window = Window {
        base: 0x8800_0000,
        size: 0x0400_0000,
        r: true,
        w: true,
        x: true,
    };

    #[test]
    fn one_partition_is_four_entries() {
        let e = encode(&[PART], C906).unwrap();
        assert_eq!(e.used, 4);
        // Ondergrens (OFF), bovengrens (TOR|RWX), deny-all 0..2^40 (TOR, niets).
        assert_eq!(e.addr[0], 0x8800_0000 >> 2);
        assert_eq!(e.addr[1], 0x8c00_0000 >> 2);
        assert_eq!(e.addr[2], 0);
        assert_eq!(e.addr[3], (1u64 << 40) >> 2);
        assert_eq!(e.cfg, 0x08_00_0f_00);
    }

    #[test]
    fn a_grant_adds_a_read_write_pair() {
        let grant = Window {
            base: 0x0a00_0000,
            size: 0x1000,
            r: true,
            w: true,
            x: false,
        };
        let e = encode(&[PART, grant], C906).unwrap();
        assert_eq!(e.used, 6);
        assert_eq!((e.cfg >> 24) & 0xff, 0x0b); // TOR | R | W
        assert_eq!((e.cfg >> 40) & 0xff, 0x08); // deny-all
    }

    #[test]
    fn tor_takes_any_grain_multiple() {
        // 124 MB: geen macht van twee, en toch één venster (de reden voor TOR).
        let w = Window {
            size: 124 << 20,
            ..PART
        };
        assert!(encode(&[w], C906).is_ok());
    }

    #[test]
    fn refusals_are_loud() {
        let off = Window {
            base: 0x8800_0800,
            ..PART
        };
        assert!(matches!(
            encode(&[off], C906),
            Err(Error::Grain { index: 0, .. })
        ));
        let empty = Window { size: 0, ..PART };
        assert_eq!(encode(&[empty], C906), Err(Error::Empty { index: 0 }));
        let four = [PART; 4];
        assert_eq!(
            encode(&four, C906),
            Err(Error::TooMany { need: 10, have: 8 })
        );
        let wrap = Window {
            base: !0xfff,
            size: 0x2000,
            ..PART
        };
        assert_eq!(encode(&[wrap], C906), Err(Error::Overflow { index: 0 }));
    }

    #[test]
    fn qemu_covers_the_wider_space() {
        let e = encode(&[PART], QEMU).unwrap();
        assert_eq!(e.addr[3], (1u64 << 54) >> 2);
        assert!(e.addr[3] < 1 << 54);
    }
}
