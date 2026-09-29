//! De losse instructies van dit board: de EL1-fysieke timer (CNTP), het
//! I-masker en de GICv3-CPU-interface; dezelfde als op QEMU virt (het
//! silicium is dezelfde familie), zonder de heap-grenzen van het
//! linkscript: hier komt de heap uit de EFI-allocatie. De klok en de slaap
//! zijn van `cpu::idle`.
//!
//! Op de host staan hier stubs met dezelfde signatuur, zodat de logica
//! erboven test (handboek §7: `cfg` op module-niveau).

/// De timer uit: zijn lijn valt, tot de slaap hem weer zet.
pub(crate) fn timer_off() {
    imp::timer_off();
}

/// De EL2-timer (CNTHP) uit: zijn lijn valt.
pub(crate) fn hyp_timer_off() {
    imp::hyp_timer_off();
}

/// De hoogste pending Group-1-INTID (ICC_HPPIR1_EL1), zonder claim: de
/// peek waarmee de OS-core na een terugkeer ziet of het de kick was.
pub(crate) fn hppir1() -> u32 {
    (imp::icc_hppir1() & 0xff_ffff) as u32
}

/// Schrijft ICC_SGI1R_EL1: een SGI volgens `v` (zie
/// `driver_gicv3::sgi1r`).
pub(crate) fn sgi1r(v: u64) {
    imp::icc_sgi1r(v);
}

/// Opent I: het begin van interrupt-afhandeling, en het einde van elke
/// dispatch-ronde (de vector keert gemaskeerd terug).
pub(crate) fn irq_unmask() {
    imp::irq_unmask();
}

/// De GICv3-CPU-interface als systeemregisters: de [`driver_gicv3::Icc`]
/// van dit board. De namen staan als generieke encodering (`S3_...`) zodat
/// elke assembler ze kent.
pub(crate) struct SysRegIcc;

impl driver_gicv3::Icc for SysRegIcc {
    fn enable_sre(&self) {
        imp::icc_enable_sre();
    }
    fn set_pmr(&self, pmr: u8) {
        imp::icc_set_pmr(u64::from(pmr));
    }
    fn set_grp1(&self, on: bool) {
        imp::icc_set_grp1(u64::from(on));
    }
    fn iar1(&self) -> u32 {
        (imp::icc_iar1() & 0xffff_ffff) as u32
    }
    fn eoir1(&self, intid: u32) {
        imp::icc_eoir1(u64::from(intid));
    }
}

/// De MPIDR-affiniteit van deze core, voor de GIC-route.
pub(crate) fn mpidr() -> u64 {
    imp::mpidr()
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

    pub(super) fn mpidr() -> u64 {
        let v: u64;
        // SAFETY: MPIDR_EL1 lezen heeft geen neveneffect.
        unsafe { asm!("mrs {}, mpidr_el1", out(reg) v, options(nomem, nostack)) };
        v
    }

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

    pub(super) fn hyp_timer_off() {
        // SAFETY: CNTHP_CTL_EL2 = 0 zet de EL2-timer van deze core uit; geen
        // geheugeneffect.
        unsafe { asm!("msr cnthp_ctl_el2, xzr", "isb", options(nomem, nostack)) };
    }

    pub(super) fn icc_hppir1() -> u64 {
        let v: u64;
        // SAFETY: ICC_HPPIR1_EL1 (S3_0_C12_C12_2) lezen claimt niets en
        // heeft geen neveneffect.
        unsafe { asm!("mrs {}, S3_0_C12_C12_2", out(reg) v, options(nomem, nostack)) };
        v
    }

    pub(super) fn icc_sgi1r(v: u64) {
        // SAFETY: ICC_SGI1R_EL1 (S3_0_C12_C11_5) stuurt een SGI; geen
        // geheugen.
        unsafe { asm!("msr S3_0_C12_C11_5, {}", "isb", in(reg) v, options(nomem, nostack)) };
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
        // SAFETY: CNTP_CTL_EL0 = 0 zet de eigen fysieke timer van deze core
        // uit; geen geheugeneffect.
        unsafe { asm!("msr cntp_ctl_el0, xzr", "isb", options(nomem, nostack)) };
    }

    pub(super) fn irq_unmask() {
        // SAFETY: alleen PSTATE.I van deze core; geen `nomem`, zodat geen
        // geheugentoegang over het openen heen schuift.
        unsafe { asm!("msr daifclr, #2", options(nostack)) };
    }

    // De ICC-registers (ARM IHI 0069, §12.2): ICC_SRE_EL2 = S3_4_C12_C9_5,
    // ICC_SRE_EL1 = S3_0_C12_C12_5, ICC_PMR_EL1 = S3_0_C4_C6_0,
    // ICC_IGRPEN1_EL1 = S3_0_C12_C12_7, ICC_IAR1_EL1 = S3_0_C12_C12_0,
    // ICC_EOIR1_EL1 = S3_0_C12_C12_1.

    pub(super) fn icc_enable_sre() {
        // SAFETY: SRE (bit 0) en Enable (bit 3) in ICC_SRE_EL2, dan SRE in
        // ICC_SRE_EL1; alleen de CPU-interface, geen geheugen. Op QEMU zijn
        // ze RAO (alleen de systeemregister-interface bestaat).
        unsafe {
            asm!(
                "mrs {t}, S3_4_C12_C9_5",
                "orr {t}, {t}, #0x1",
                "orr {t}, {t}, #0x8",
                "msr S3_4_C12_C9_5, {t}",
                "isb",
                "mrs {t}, S3_0_C12_C12_5",
                "orr {t}, {t}, #0x1",
                "msr S3_0_C12_C12_5, {t}",
                "isb",
                t = out(reg) _,
                options(nomem, nostack),
            );
        }
    }

    pub(super) fn icc_set_pmr(v: u64) {
        // SAFETY: het prioriteitsmasker; geen geheugen.
        unsafe { asm!("msr S3_0_C4_C6_0, {}", "isb", in(reg) v, options(nomem, nostack)) };
    }

    pub(super) fn icc_set_grp1(v: u64) {
        // SAFETY: Group 1 aan of uit; geen geheugen.
        unsafe { asm!("msr S3_0_C12_C12_7, {}", "isb", in(reg) v, options(nomem, nostack)) };
    }

    pub(super) fn icc_iar1() -> u64 {
        let v: u64;
        // SAFETY: de claim: de GIC markeert de lijn actief; geen geheugen.
        unsafe { asm!("isb", "mrs {}, S3_0_C12_C12_0", out(reg) v, options(nomem, nostack)) };
        v
    }

    pub(super) fn icc_eoir1(v: u64) {
        // SAFETY: de EOI van een geclaimde lijn; geen geheugen.
        unsafe { asm!("msr S3_0_C12_C12_1, {}", "isb", in(reg) v, options(nomem, nostack)) };
    }
}

#[cfg(not(all(target_arch = "aarch64", target_os = "none")))]
mod imp {
    //! Host-stubs.
    pub(super) fn mpidr() -> u64 {
        0
    }
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
    pub(super) fn hyp_timer_off() {}
    pub(super) fn icc_hppir1() -> u64 {
        1023
    }
    pub(super) fn icc_sgi1r(_v: u64) {}
    pub(super) fn timer_off() {}
    pub(super) fn irq_unmask() {}
    pub(super) fn icc_enable_sre() {}
    pub(super) fn icc_set_pmr(_v: u64) {}
    pub(super) fn icc_set_grp1(_v: u64) {}
    pub(super) fn icc_iar1() -> u64 {
        1023
    }
    pub(super) fn icc_eoir1(_v: u64) {}
}
