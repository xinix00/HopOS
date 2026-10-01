//! De GICv3-CPU-interface als systeemregisters (ICC_*, ARM IHI 0069,
//! §12.2): de instructies achter `driver_gicv3::SysRegIcc`, plus de peek
//! (HPPIR1) en de SGI (SGI1R) van de kick. De namen staan als generieke
//! encodering (`S3_...`) zodat elke assembler ze kent: ICC_SRE_EL2 =
//! S3_4_C12_C9_5, ICC_SRE_EL1 = S3_0_C12_C12_5, ICC_PMR_EL1 = S3_0_C4_C6_0,
//! ICC_IGRPEN1_EL1 = S3_0_C12_C12_7, ICC_IAR1_EL1 = S3_0_C12_C12_0,
//! ICC_EOIR1_EL1 = S3_0_C12_C12_1, ICC_HPPIR1_EL1 = S3_0_C12_C12_2,
//! ICC_SGI1R_EL1 = S3_0_C12_C11_5.
//!
//! Op de host staan hier stubs met dezelfde signatuur (handboek §7).

/// Zet de systeemregister-interface aan: SRE en Enable in ICC_SRE_EL2, dan
/// SRE in ICC_SRE_EL1.
pub fn enable_sre() {
    imp::enable_sre();
}

/// Het prioriteitsmasker (ICC_PMR_EL1).
pub fn set_pmr(pmr: u8) {
    imp::set_pmr(u64::from(pmr));
}

/// Group 1 aan of uit (ICC_IGRPEN1_EL1).
pub fn set_grp1(on: bool) {
    imp::set_grp1(u64::from(on));
}

/// De claim (ICC_IAR1_EL1): de GIC markeert de lijn actief.
#[must_use]
pub fn iar1() -> u32 {
    (imp::iar1() & 0xffff_ffff) as u32
}

/// De EOI van een geclaimde lijn (ICC_EOIR1_EL1).
pub fn eoir1(intid: u32) {
    imp::eoir1(u64::from(intid));
}

/// De hoogste pending Group-1-INTID (ICC_HPPIR1_EL1), zonder claim: de
/// peek waarmee de OS-core na een terugkeer ziet of het de kick was.
#[must_use]
pub fn hppir1() -> u32 {
    (imp::hppir1() & 0xff_ffff) as u32
}

/// Schrijft ICC_SGI1R_EL1: een SGI volgens `v` (zie `driver_gicv3::sgi1r`).
pub fn sgi1r(v: u64) {
    imp::sgi1r(v);
}

#[cfg(all(target_arch = "aarch64", target_os = "none"))]
mod imp {
    use core::arch::asm;

    pub(super) fn enable_sre() {
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

    pub(super) fn set_pmr(v: u64) {
        // SAFETY: het prioriteitsmasker; geen geheugen.
        unsafe { asm!("msr S3_0_C4_C6_0, {}", "isb", in(reg) v, options(nomem, nostack)) };
    }

    pub(super) fn set_grp1(v: u64) {
        // SAFETY: Group 1 aan of uit; geen geheugen.
        unsafe { asm!("msr S3_0_C12_C12_7, {}", "isb", in(reg) v, options(nomem, nostack)) };
    }

    pub(super) fn iar1() -> u64 {
        let v: u64;
        // SAFETY: de claim: de GIC markeert de lijn actief; geen geheugen.
        unsafe { asm!("isb", "mrs {}, S3_0_C12_C12_0", out(reg) v, options(nomem, nostack)) };
        v
    }

    pub(super) fn eoir1(v: u64) {
        // SAFETY: de EOI van een geclaimde lijn; geen geheugen.
        unsafe { asm!("msr S3_0_C12_C12_1, {}", "isb", in(reg) v, options(nomem, nostack)) };
    }

    pub(super) fn hppir1() -> u64 {
        let v: u64;
        // SAFETY: ICC_HPPIR1_EL1 lezen claimt niets en heeft geen
        // neveneffect.
        unsafe { asm!("mrs {}, S3_0_C12_C12_2", out(reg) v, options(nomem, nostack)) };
        v
    }

    pub(super) fn sgi1r(v: u64) {
        // SAFETY: ICC_SGI1R_EL1 stuurt een SGI; geen geheugen.
        unsafe { asm!("msr S3_0_C12_C11_5, {}", "isb", in(reg) v, options(nomem, nostack)) };
    }
}

#[cfg(not(all(target_arch = "aarch64", target_os = "none")))]
mod imp {
    //! Host-stubs: er is geen GIC; een claim of peek ziet de spurious 1023.
    pub(super) fn enable_sre() {}
    pub(super) fn set_pmr(_v: u64) {}
    pub(super) fn set_grp1(_v: u64) {}
    pub(super) fn iar1() -> u64 {
        1023
    }
    pub(super) fn eoir1(_v: u64) {}
    pub(super) fn hppir1() -> u64 {
        1023
    }
    pub(super) fn sgi1r(_v: u64) {}
}
