//! Host-tests over een nep-RNG200: een registerblok met een FIFO dat de
//! warm-up, de reset-bits en de IRQ-status naspeelt.

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

/// Een nep-RNG200. `delay` lezingen van FIFO_COUNT na de start blijft de
/// FIFO leeg (de warm-up); daarna levert hij `words` in volgorde.
#[derive(Default)]
struct Fake {
    ctrl: u32,
    rng_reset: u32,
    rbg_reset: u32,
    status: u32,
    delay: u32,
    words: Vec<u32>,
    next: usize,
    /// Elke schrijf, in volgorde: (offset, waarde).
    log: Vec<(u64, u32)>,
    /// Hoeveel keer RBGEN van 0 naar 1 ging.
    starts: u32,
    /// Een blok dat niets onthoudt (QEMU: geen RNG200 op dit adres).
    absent: bool,
    /// Blijft de IRQ-status na een herstart staan?
    sticky_status: bool,
}

impl Regs for Fake {
    fn read(&mut self, off: u64) -> u32 {
        if self.absent {
            return 0;
        }
        match off {
            RNG_CTRL => self.ctrl,
            RNG_SOFT_RESET => self.rng_reset,
            RBG_SOFT_RESET => self.rbg_reset,
            RNG_INT_STATUS => self.status,
            RNG_FIFO_COUNT => {
                if self.ctrl & RBGEN_ENABLE == 0 {
                    return 0;
                }
                if self.delay > 0 {
                    self.delay -= 1;
                    return 0;
                }
                (self.words.len() - self.next).min(0xFF) as u32 | 0x1000
            }
            RNG_FIFO_DATA => {
                let w = self.words.get(self.next).copied().unwrap_or(0);
                self.next += 1;
                w
            }
            _ => 0,
        }
    }

    fn write(&mut self, off: u64, v: u32) {
        self.log.push((off, v));
        if self.absent {
            return;
        }
        match off {
            RNG_CTRL => {
                if self.ctrl & RBGEN_ENABLE == 0 && v & RBGEN_ENABLE != 0 {
                    self.starts += 1;
                }
                self.ctrl = v;
            }
            RNG_SOFT_RESET => self.rng_reset = v,
            RBG_SOFT_RESET => self.rbg_reset = v,
            RNG_INT_STATUS if !self.sticky_status => self.status &= !v,
            _ => {}
        }
    }
}

fn rng(fake: Fake) -> Rng200<Fake> {
    Rng200::new(fake, ticking)
}

#[test]
fn the_start_follows_iproc_rng200() {
    let mut r = rng(Fake {
        ctrl: 0xA000 | 0x1FFF,
        words: vec![0x0403_0201, 0x0807_0605],
        ..Fake::default()
    });
    let mut b = [0u8; 6];
    r.fill(&mut b).unwrap();
    // Little-endian per woord, de staart afgekapt.
    assert_eq!(b, [1, 2, 3, 4, 5, 6]);
    // RBG uit (de bits buiten RBGEN blijven), status wissen, RBG en RNG
    // aan, RNG en RBG uit, RBG aan.
    let f = &r.regs;
    assert_eq!(
        f.log,
        vec![
            (RNG_CTRL, 0xA000),
            (RNG_INT_STATUS, 0xFFFF_FFFF),
            (RBG_SOFT_RESET, 1),
            (RNG_SOFT_RESET, 1),
            (RNG_SOFT_RESET, 0),
            (RBG_SOFT_RESET, 0),
            (RNG_CTRL, 0xA001),
        ]
    );
    assert_eq!(f.starts, 1);
}

#[test]
fn the_warm_up_may_take_long_the_next_words_not() {
    // De eerste lezing wacht 1,5 s (de BCM2711): binnen de warm-up.
    let mut r = rng(Fake {
        delay: 1_500,
        words: vec![1, 2, 3],
        ..Fake::default()
    });
    let mut b = [0u8; 8];
    r.fill(&mut b).unwrap();
    assert_eq!(b, [1, 0, 0, 0, 2, 0, 0, 0]);
    // Een tweede trekking start niet opnieuw.
    let mut c = [0u8; 4];
    r.fill(&mut c).unwrap();
    assert_eq!(c, [3, 0, 0, 0]);
    assert_eq!(r.regs.starts, 1);

    // Een lege FIFO na de warm-up: de korte grens.
    r.regs.delay = 1_000;
    assert_eq!(r.fill(&mut c), Err(Error::Timeout { warmup: false }));

    // Een warm-up die de grens overschrijdt.
    let mut r = rng(Fake {
        delay: 10_000,
        words: vec![1],
        ..Fake::default()
    });
    assert_eq!(r.fill(&mut c), Err(Error::Timeout { warmup: true }));
}

#[test]
fn an_absent_block_is_said_at_once() {
    let mut r = rng(Fake {
        absent: true,
        ..Fake::default()
    });
    let t0 = NOW.with(Cell::get);
    let mut b = [0u8; 4];
    assert_eq!(r.fill(&mut b), Err(Error::Absent(0)));
    // Geen warm-up afgewacht: geen klok gelezen.
    assert_eq!(NOW.with(Cell::get), t0);
    assert!(Error::Absent(0).to_string().contains("no RNG200"));
}

#[test]
fn equal_words_in_a_row_are_a_stuck_source() {
    let mut r = rng(Fake {
        words: vec![7, 0xFFFF_FFFF, 0xFFFF_FFFF],
        ..Fake::default()
    });
    let mut b = [0u8; 12];
    assert_eq!(r.fill(&mut b), Err(Error::Stuck(0xFFFF_FFFF)));
}

#[test]
fn a_health_fault_gets_one_restart() {
    // NIST-fout die de herstart wist: gewoon door, met een tweede start.
    let mut r = rng(Fake {
        words: vec![1, 2],
        ..Fake::default()
    });
    let mut b = [0u8; 4];
    r.fill(&mut b).unwrap();
    r.regs.status = 0x20;
    r.fill(&mut b).unwrap();
    assert_eq!(b, [2, 0, 0, 0]);
    assert_eq!(r.regs.starts, 2);

    // Een lockout die blijft: opgeven.
    r.regs.status = 0x8000_0000;
    r.regs.sticky_status = true;
    assert_eq!(r.fill(&mut b), Err(Error::Health(0x8000_0000)));
}

#[test]
fn an_empty_buffer_is_refused() {
    let mut r = rng(Fake::default());
    assert_eq!(r.fill(&mut []), Err(Error::Empty));
    assert_eq!(r.regs.starts, 0);
}
