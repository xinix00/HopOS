//! De losse instructies van een RISC-V-hart in machine mode: CSR's lezen en
//! zetten, `rdtime`, `wfi`. Eén functie per instructie, zonder beslissingen.
//!
//! Op de host staan hier stubs met dezelfde signatuur (handboek §7): de
//! logica erboven (de slaap, de PLIC, de kooi-rekenkunde) test zo op de
//! ontwikkelmachine, het ijzer bewijst de instructies.

/// `mstatus.MIE`: de globale interrupt-enable van machine mode.
pub const MSTATUS_MIE: u64 = 1 << 3;
/// `mstatus.FS = Initial`: de FPU aan. Zonder dit is elke FP-instructie een
/// illegal instruction, en het doel `riscv64gc` mag ze overal gebruiken.
pub const MSTATUS_FS_INITIAL: u64 = 1 << 13;

/// `mie`/`mip`-bit van de machine software interrupt (MSIP, de kick).
pub const MIP_MSIP: u64 = 1 << 3;
/// `mie`/`mip`-bit van de machine timer interrupt (MTIP).
pub const MIP_MTIP: u64 = 1 << 7;
/// `mie`/`mip`-bit van de machine external interrupt (MEIP, de PLIC).
pub const MIP_MEIP: u64 = 1 << 11;

pub use imp::*;

#[cfg(all(target_arch = "riscv64", target_os = "none"))]
mod imp {
    use core::arch::asm;

    /// Het hart-id (`mhartid`).
    #[inline]
    #[must_use]
    pub fn mhartid() -> u64 {
        let v: u64;
        // SAFETY: een lees van een alleen-lezen-CSR zonder bijwerkingen.
        unsafe { asm!("csrr {}, mhartid", out(reg) v, options(nomem, nostack)) };
        v
    }

    /// De TIME-CSR (`rdtime`). Op de C906 de enige tijdbron: de
    /// c900-CLINT heeft geen `mtime`-register (gemeten 30-07: elke lees op
    /// 0xbff8 is een bus-fout, mcause 5). Op QEMU levert de ACLINT dezelfde
    /// teller aan de CSR, dus één pad voor beide.
    #[inline]
    #[must_use]
    pub fn rdtime() -> u64 {
        let v: u64;
        // SAFETY: `rdtime` leest een teller; in machine mode trapt hij niet.
        unsafe { asm!("rdtime {}", out(reg) v, options(nomem, nostack)) };
        v
    }

    /// Wist `mstatus.MIE` en geeft `mstatus` zoals hij stond.
    #[inline]
    pub fn mask() -> u64 {
        let v: u64;
        // SAFETY: raakt alleen de interrupt-enable van dit hart. Geen
        // `nomem`: geheugentoegang mag niet over het maskeren heen schuiven.
        unsafe { asm!("csrrci {}, mstatus, 8", out(reg) v, options(nostack)) };
        v
    }

    /// Zet `mstatus.MIE` terug zoals `mask` hem las.
    #[inline]
    pub fn restore(prev: u64) {
        if prev & super::MSTATUS_MIE != 0 {
            // SAFETY: zet alleen de interrupt-enable van dit hart terug.
            unsafe { asm!("csrsi mstatus, 8", options(nostack)) };
        }
    }

    /// Zet bits in `mie`.
    #[inline]
    pub fn mie_set(bits: u64) {
        // SAFETY: alleen de interrupt-enables van dit hart.
        unsafe { asm!("csrs mie, {}", in(reg) bits, options(nostack)) };
    }

    /// Wist bits in `mie`.
    #[inline]
    pub fn mie_clear(bits: u64) {
        // SAFETY: alleen de interrupt-enables van dit hart.
        unsafe { asm!("csrc mie, {}", in(reg) bits, options(nostack)) };
    }

    /// `mie` lezen.
    #[inline]
    #[must_use]
    pub fn mie() -> u64 {
        let v: u64;
        // SAFETY: een lees zonder bijwerkingen.
        unsafe { asm!("csrr {}, mie", out(reg) v, options(nomem, nostack)) };
        v
    }

    /// `mip` lezen: wat er pending staat, ook zonder enable.
    #[inline]
    #[must_use]
    pub fn mip() -> u64 {
        let v: u64;
        // SAFETY: een lees zonder bijwerkingen.
        unsafe { asm!("csrr {}, mip", out(reg) v, options(nomem, nostack)) };
        v
    }

    /// Eén `wfi`. Wekt op elke pending interrupt waarvan de `mie`-bit staat,
    /// óók met `mstatus.MIE` uit (privileged spec, 3.3.3): daarop rust de
    /// slaap van de kern, die de trap zelf niet wil nemen.
    #[inline]
    pub fn wfi() {
        // SAFETY: `wfi` wacht en heeft geen geheugeneffect.
        unsafe { asm!("wfi", options(nomem, nostack)) };
    }
}

#[cfg(not(all(target_arch = "riscv64", target_os = "none")))]
mod imp {
    //! Host-stubs: hart 0, een stilstaande teller, geen interrupts.
    use core::sync::atomic::{AtomicU64, Ordering::Relaxed};

    /// De nep-teller van de host, door de tests gezet.
    pub static FAKE_TIME: AtomicU64 = AtomicU64::new(0);
    /// De nep-`mie` van de host.
    pub static FAKE_MIE: AtomicU64 = AtomicU64::new(0);

    /// Host: hart 0.
    #[must_use]
    pub fn mhartid() -> u64 {
        0
    }
    /// Host: de nep-teller.
    #[must_use]
    pub fn rdtime() -> u64 {
        FAKE_TIME.load(Relaxed)
    }
    /// Host: geen masker.
    pub fn mask() -> u64 {
        0
    }
    /// Host: geen masker.
    pub fn restore(_prev: u64) {}
    /// Host: de nep-`mie`.
    pub fn mie_set(bits: u64) {
        FAKE_MIE.fetch_or(bits, Relaxed);
    }
    /// Host: de nep-`mie`.
    pub fn mie_clear(bits: u64) {
        FAKE_MIE.fetch_and(!bits, Relaxed);
    }
    /// Host: de nep-`mie`.
    #[must_use]
    pub fn mie() -> u64 {
        FAKE_MIE.load(Relaxed)
    }
    /// Host: niets pending.
    #[must_use]
    pub fn mip() -> u64 {
        0
    }
    /// Host: slapen duurt niets.
    pub fn wfi() {}
}
