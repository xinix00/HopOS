//! De losse instructies van dit board: de EL1-fysieke timer (CNTP, met de
//! VHE-vorm), het EL, de MMU-feiten, de TLB en de sprong naar de kern. Het
//! I-masker, de EL2-timer en de GICv3-CPU-interface zijn van `cpu`, de klok
//! en de slaap van `cpu::idle`.
//!
//! Op de host staan hier stubs met dezelfde signatuur, zodat de logica
//! erboven test (handboek §7: `cfg` op module-niveau).

/// De EL1-timer (CNTP, PPI 30) uit: zijn lijn valt, tot de slaap hem weer
/// zet. Onder VHE slaapt de kern op de CNTHP en is dit de timer van een
/// bewoner.
pub(crate) fn timer_off() {
    imp::timer_off();
}

/// Het exception level waarop we draaien.
pub(crate) fn current_el() -> u8 {
    imp::current_el()
}

/// ID_AA64MMFR0_EL1: PARange (3:0) voor de PS van TCR.
pub(crate) fn mmfr0() -> u64 {
    imp::mmfr0()
}

/// Wist de TLB van EL2 na een nieuwe mapping.
pub(crate) fn tlbi_all() {
    imp::tlbi_all();
}

/// De sprong van de stub naar de kern: MMU uit, EL2 saneren (met
/// `cnthctl` in CNTHCTL_EL2), onze map (`ttbr0`, `tcr`) aan, de eigen
/// stack en vectoren, `kmain(0, el)`. Op een ander EL dan 2 blijft de map
/// van de firmware staan en meldt `kmain` de EL-eis.
pub(crate) fn enter_kernel(ttbr0: u64, tcr: u64, el: u8, cnthctl: u64) -> ! {
    imp::enter_kernel(ttbr0, tcr, el, cnthctl)
}

#[cfg(all(target_arch = "aarch64", target_os = "none"))]
mod imp {
    use core::arch::asm;

    pub(super) fn current_el() -> u8 {
        let v: u64;
        // SAFETY: CurrentEL lezen heeft geen neveneffect.
        unsafe { asm!("mrs {}, CurrentEL", out(reg) v, options(nomem, nostack)) };
        ((v >> 2) & 3) as u8
    }

    pub(super) fn mmfr0() -> u64 {
        let v: u64;
        // SAFETY: een ID-register lezen heeft geen neveneffect.
        unsafe { asm!("mrs {}, id_aa64mmfr0_el1", out(reg) v, options(nomem, nostack)) };
        v
    }

    unsafe extern "C" {
        /// De sprong naar de kern (de `global_asm!` in `crate::boot`s
        /// assembly hieronder).
        fn uefi_enter_kernel(ttbr0: u64, tcr: u64, el: u64, cnthctl: u64) -> !;
    }

    pub(super) fn enter_kernel(ttbr0: u64, tcr: u64, el: u8, cnthctl: u64) -> ! {
        // SAFETY: de stub roept dit één keer aan, na een geslaagde
        // `ExitBootServices`, met een identity map die de image, de stack,
        // de tabellen en de console dekt en die naar het geheugen geveegd
        // is (`crate::boot::go`).
        unsafe { uefi_enter_kernel(ttbr0, tcr, u64::from(el), cnthctl) }
    }

    pub(super) fn tlbi_all() {
        // SAFETY: de tabellen zijn al geschreven (dev::write64, vluchtig);
        // `dsb ishst` maakt ze zichtbaar voor de walker, dan de TLB weg.
        unsafe {
            asm!(
                "dsb ishst",
                "tlbi alle2",
                "dsb ish",
                "isb",
                options(nostack)
            )
        };
    }

    pub(super) fn timer_off() {
        // Onder VHE is `cntp_ctl_el0` vanaf EL2 de CNTHP (die zet
        // `cpu::idle::hyp_timer_off` al uit); de EL1-timer van PPI 30 heet dan
        // CNTP_CTL_EL02 (S3_5_C14_C2_1). De nVHE-tak is de instructie van
        // vóór VHE.
        // SAFETY: de fysieke EL1-timer van deze core op 0 zetten; geen
        // geheugeneffect. De EL02-encodering staat alleen in de VHE-build,
        // waar de kern onder E2H = 1 draait en hij bestaat.
        unsafe {
            asm!(
                ".if {vhe}",
                "msr S3_5_C14_C2_1, xzr",
                ".else",
                "msr cntp_ctl_el0, xzr",
                ".endif",
                "isb",
                vhe = const crate::el2::VHE as u8,
                options(nomem, nostack)
            )
        };
    }
}

#[cfg(not(all(target_arch = "aarch64", target_os = "none")))]
mod imp {
    //! Host-stubs.
    pub(super) fn current_el() -> u8 {
        2
    }
    pub(super) fn mmfr0() -> u64 {
        5
    }
    pub(super) fn enter_kernel(_ttbr0: u64, _tcr: u64, _el: u8, _cnthctl: u64) -> ! {
        loop {
            core::hint::spin_loop();
        }
    }
    pub(super) fn tlbi_all() {}
    pub(super) fn timer_off() {}
}
