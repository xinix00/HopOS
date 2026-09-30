//! Host-tests over een nep-TRNG: een RNG_CTL met hiword-masker, een START
//! die na een paar lezingen valt, en een DOUT die per ronde nieuwe woorden
//! geeft.

use super::*;
use std::cell::Cell;

thread_local! {
    /// De nep-klok: elke lezing 1 ms verder.
    static NOW: Cell<u64> = const { Cell::new(0) };
}

fn ticking() -> u64 {
    NOW.with(|n| {
        let v = n.get();
        n.set(v + 1_000_000);
        v
    })
}

/// Een nep-TRNG.
#[derive(Default)]
struct Fake {
    ctl: u32,
    samples: u32,
    /// Lezingen van RNG_CTL tot START valt (per ronde).
    busy: u32,
    left: u32,
    /// Het rondenummer; de woorden hangen ervan af.
    round: u32,
    /// Elke ronde dezelfde woorden (een vastgelopen bron).
    stuck: bool,
    /// Een blok dat niets onthoudt en nul leest (geen klok).
    dead: bool,
    /// START valt nooit.
    hang: bool,
    /// Elke schrijf naar RNG_CTL, in volgorde.
    ctl_log: Vec<u32>,
    /// RNG_CTL op het moment van elke START, na het masker.
    at_start: Vec<u32>,
}

impl Regs for Fake {
    fn read(&mut self, off: u64) -> u32 {
        if self.dead {
            return 0;
        }
        match off {
            RNG_CTL => {
                if self.ctl & CTL_START != 0 && !self.hang {
                    if self.left == 0 {
                        self.ctl &= !CTL_START;
                    } else {
                        self.left -= 1;
                    }
                }
                self.ctl
            }
            RNG_SAMPLE_CNT => self.samples,
            o if (RNG_DOUT..RNG_DOUT + 32).contains(&o) => {
                let i = ((o - RNG_DOUT) / 4) as u32;
                let r = if self.stuck { 1 } else { self.round };
                0x9E37_79B9u32
                    .wrapping_mul(r.wrapping_add(1))
                    .rotate_left(i)
                    ^ i
            }
            _ => 0,
        }
    }

    fn write(&mut self, off: u64, v: u32) {
        if self.dead {
            return;
        }
        match off {
            RNG_CTL => {
                self.ctl_log.push(v);
                let mask = v >> 16;
                let was = self.ctl;
                self.ctl = (self.ctl & !mask) | (v & mask & 0xFFFF);
                if was & CTL_START == 0 && self.ctl & CTL_START != 0 {
                    self.round += 1;
                    self.left = self.busy;
                    self.at_start.push(self.ctl);
                }
            }
            RNG_SAMPLE_CNT => self.samples = v,
            _ => {}
        }
    }
}

fn trng(f: Fake) -> Trng<Fake> {
    Trng::new(f, ticking)
}

#[test]
fn a_fill_runs_whole_rounds_with_the_ring_enabled() {
    let mut t = trng(Fake {
        busy: 3,
        ..Fake::default()
    });
    let mut a = [0u8; 48];
    t.fill(&mut a).unwrap();
    let f = t.regs();
    // 48 bytes = twee rondes; de sample-telling van Linux.
    assert_eq!(f.round, 2);
    assert_eq!(f.samples, SAMPLES);
    // Bij elke START stonden ring en lengte nog aan: de START-schrijf
    // maskeert alleen zijn eigen bit.
    for c in &f.at_start {
        assert_eq!(c & 0x3E, CTL_LEN_256 | CTL_ENABLE, "ctl {c:#x}");
    }
    // Aan met het volle masker, dan de STARTs, dan uit.
    assert_eq!(f.ctl_log.first(), Some(&0xFFFF_0032));
    assert_eq!(f.ctl_log.last(), Some(&0xFFFF_0000));
    assert_eq!(f.ctl & 0xFFFF, 0, "the ring is still on after the fill");
    assert!(a.iter().any(|&b| b != 0));
}

#[test]
fn two_fills_give_different_bytes() {
    let mut t = trng(Fake::default());
    let (mut a, mut b) = ([0u8; 32], [0u8; 32]);
    t.fill(&mut a).unwrap();
    t.fill(&mut b).unwrap();
    assert_ne!(a, b);
}

#[test]
fn a_short_buffer_takes_the_head_of_a_round() {
    let mut t = trng(Fake::default());
    let mut a = [0u8; 5];
    t.fill(&mut a).unwrap();
    assert_eq!(t.regs().round, 1);
}

#[test]
fn a_dead_block_is_zero_not_entropy() {
    let mut t = trng(Fake {
        dead: true,
        ..Fake::default()
    });
    let mut a = [0u8; 32];
    assert_eq!(t.fill(&mut a), Err(Error::Zero));
}

#[test]
fn a_stuck_source_fails_the_continuous_test() {
    let mut t = trng(Fake {
        stuck: true,
        ..Fake::default()
    });
    let mut a = [0u8; 64];
    assert!(matches!(t.fill(&mut a), Err(Error::Stuck(_))));
    // Ook over twee losse fills heen.
    let mut t = trng(Fake {
        stuck: true,
        ..Fake::default()
    });
    let mut b = [0u8; 32];
    t.fill(&mut b).unwrap();
    assert!(matches!(t.fill(&mut b), Err(Error::Stuck(_))));
}

#[test]
fn a_start_that_never_falls_times_out_and_stops_the_ring() {
    let mut t = trng(Fake {
        hang: true,
        ..Fake::default()
    });
    let mut a = [0u8; 32];
    match t.fill(&mut a) {
        Err(Error::Timeout(ctl)) => assert_ne!(ctl & CTL_START, 0),
        other => panic!("{other:?}"),
    }
    assert_eq!(t.regs().ctl_log.last(), Some(&0xFFFF_0000));
}

#[test]
fn an_empty_buffer_is_refused_without_touching_the_block() {
    let mut t = trng(Fake::default());
    assert_eq!(t.fill(&mut []), Err(Error::Empty));
    assert!(t.regs().ctl_log.is_empty());
}

#[test]
fn the_fingerprint_separates_rounds() {
    assert_ne!(fingerprint(&[1; 32]), fingerprint(&[2; 32]));
    assert_eq!(fingerprint(&[]), 0xcbf2_9ce4_8422_2325);
}

#[test]
fn errors_say_what_happened() {
    assert!(Error::Timeout(1).to_string().contains("10 ms"));
    assert!(Error::Zero.to_string().contains("not clocked"));
}
