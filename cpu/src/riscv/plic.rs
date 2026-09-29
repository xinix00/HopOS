//! De PLIC (RISC-V Platform-Level Interrupt Controller) als
//! [`crate::irq::Controller`]: dezelfde vier werkwoorden als de GIC.
//!
//! Het registerplan is dat van de spec (en van QEMU virt en de SG2002):
//!
//! ```text
//! 0x000000 + 4*src              priority[src]      (0 = nooit)
//! 0x001000                      pending-bits
//! 0x002000 + 0x80*ctx           enable-bits van context ctx
//! 0x200000 + 0x1000*ctx         threshold van context ctx
//! 0x200004 + 0x1000*ctx         claim/complete van context ctx
//! ```
//!
//! Een context is een (hart, modus)-paar. Op QEMU virt en de SG2002 is de
//! machine-mode-context van hart h `2h` en de supervisor-context `2h+1`; de
//! kern draait in machine mode en neemt dus `2h` ([`machine_context`]).
//!
//! Claim is tegelijk de bevestiging bij de controller (de lijn gaat van
//! pending naar in-behandeling); complete geeft hem vrij. Dat is precies
//! `claim`/`complete` van het contract.

use crate::irq::{Controller, Error, Line};
use core::sync::atomic::{AtomicU32, Ordering::Relaxed};
use dev::Pa;

const PRIORITY: u64 = 0x0;
const ENABLE: u64 = 0x2000;
const ENABLE_STRIDE: u64 = 0x80;
const CONTEXT: u64 = 0x20_0000;
const CONTEXT_STRIDE: u64 = 0x1000;
const THRESHOLD: u64 = 0x0;
const CLAIM: u64 = 0x4;

/// De machine-mode-context van `hart` op een PLIC met twee contexten per
/// hart (M, S): QEMU virt en de SG2002.
#[must_use]
pub const fn machine_context(hart: usize) -> u32 {
    2 * hart as u32
}

/// Een PLIC op een vaste basis.
pub struct Plic {
    base: Pa,
    sources: u32,
    ctx: AtomicU32,
}

impl Plic {
    /// De PLIC op `base` met bronnen `1..sources`, context 0 tot
    /// [`set_context`](Self::set_context).
    ///
    /// # Safety
    ///
    /// `base` is een PLIC volgens de spec die zolang het programma draait
    /// bereikbaar blijft.
    #[must_use]
    pub const unsafe fn new(base: Pa, sources: u32) -> Self {
        Self {
            base,
            sources,
            ctx: AtomicU32::new(0),
        }
    }

    /// Kies de context van het hart waarop de kern draait, en zet zijn
    /// drempel op 0 (alles met prioriteit > 0 komt door).
    pub fn set_context(&self, ctx: u32) {
        self.ctx.store(ctx, Relaxed);
        dev::write32(self.context(THRESHOLD), 0);
    }

    fn context(&self, reg: u64) -> Pa {
        let ctx = u64::from(self.ctx.load(Relaxed));
        self.base.add(CONTEXT + CONTEXT_STRIDE * ctx + reg)
    }

    fn enable_word(&self, l: Line) -> Pa {
        let ctx = u64::from(self.ctx.load(Relaxed));
        self.base
            .add(ENABLE + ENABLE_STRIDE * ctx + 4 * u64::from(l.0 / 32))
    }

    fn valid(&self, l: Line) -> bool {
        l.0 != 0 && l.0 < self.sources
    }

    /// Eén regel voor de bootlog.
    #[must_use]
    pub fn describe(&self) -> PlicInfo {
        PlicInfo {
            base: self.base.0,
            ctx: self.ctx.load(Relaxed),
            sources: self.sources,
        }
    }
}

/// Wat [`Plic::describe`] meldt.
#[derive(Copy, Clone, Debug)]
pub struct PlicInfo {
    base: u64,
    ctx: u32,
    sources: u32,
}

impl core::fmt::Display for PlicInfo {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        write!(
            f,
            "PLIC at {:#x}, context {}, {} sources",
            self.base, self.ctx, self.sources
        )
    }
}

impl Controller for Plic {
    fn enable(&self, l: Line) -> Result<(), Error> {
        if !self.valid(l) {
            return Err(Error::Rejected { line: l.0 });
        }
        dev::write32(self.base.add(PRIORITY + 4 * u64::from(l.0)), 1);
        let w = self.enable_word(l);
        dev::write32(w, dev::read32(w) | (1 << (l.0 % 32)));
        Ok(())
    }

    fn disable(&self, l: Line) {
        if !self.valid(l) {
            return;
        }
        let w = self.enable_word(l);
        dev::write32(w, dev::read32(w) & !(1 << (l.0 % 32)));
    }

    fn claim(&self) -> Option<Line> {
        match dev::read32(self.context(CLAIM)) {
            0 => None,
            id => Some(Line(id)),
        }
    }

    fn complete(&self, l: Line) {
        dev::write32(self.context(CLAIM), l.0);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn enable_claim_complete_on_the_register_plan() {
        // Een nep-PLIC: 0x20_2000 bytes host-geheugen.
        let mut regs = vec![0u32; 0x20_2000 / 4];
        let base = Pa(regs.as_mut_ptr() as u64);
        // SAFETY: `regs` is een nep-PLIC in host-geheugen.
        let p = unsafe { Plic::new(base, 64) };
        p.set_context(machine_context(0));
        p.enable(Line(10)).unwrap();
        p.enable(Line(33)).unwrap();
        assert_eq!(regs[10], 1); // priority[10]
        assert_eq!(regs[0x2000 / 4], 1 << 10); // enable, ctx 0, woord 0
        assert_eq!(regs[0x2004 / 4], 1 << 1); // bron 33
        p.disable(Line(10));
        assert_eq!(regs[0x2000 / 4], 0);
        assert_eq!(p.claim(), None);
        regs[0x20_0004 / 4] = 33;
        assert_eq!(p.claim(), Some(Line(33)));
        p.complete(Line(33));
        assert_eq!(p.enable(Line(0)).err(), Some(Error::Rejected { line: 0 }));
        assert_eq!(p.enable(Line(64)).err(), Some(Error::Rejected { line: 64 }));
    }

    #[test]
    fn contexts_are_per_hart_and_mode() {
        assert_eq!(machine_context(0), 0);
        assert_eq!(machine_context(1), 2);
        let mut regs = vec![0u32; 0x20_3000 / 4];
        // SAFETY: nep-PLIC in host-geheugen.
        let p = unsafe { Plic::new(Pa(regs.as_mut_ptr() as u64), 64) };
        p.set_context(machine_context(1));
        p.enable(Line(3)).unwrap();
        assert_eq!(regs[(0x2000 + 2 * 0x80) / 4], 1 << 3);
        regs[(0x20_0004 + 2 * 0x1000) / 4] = 3;
        assert_eq!(p.claim(), Some(Line(3)));
    }
}
