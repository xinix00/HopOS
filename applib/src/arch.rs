//! De losse instructies van een app-core: de teller, de slaap, de yield en
//! de exit. Op ARM64 echte instructies; elders een stub-module met dezelfde
//! signaturen, zodat de logica eromheen op de host test (handboek §7: `cfg`
//! op module-niveau).
//!
//! Waarom dit in applib staat en niet in `cpu`: een app-image is board-loos
//! (de kooi ís het board, `board/hopslot` in de Go-boom), en deze vijf
//! instructies zijn alles wat een gekooide core van zijn silicium ziet.

pub(crate) use imp::*;

#[cfg(all(target_os = "none", target_arch = "aarch64"))]
mod imp {
    use core::arch::asm;

    /// De vrijlopende teller (CNTVCT_EL0: virtueel, trapt nooit op EL1 en
    /// telt door in WFE).
    #[inline]
    pub(crate) fn counter() -> u64 {
        let v: u64;
        // SAFETY: een lees van een systeemregister zonder bijwerkingen.
        unsafe {
            asm!("isb", "mrs {}, cntvct_el0", out(reg) v, options(nomem, nostack, preserves_flags))
        };
        v
    }

    /// De frequentie van de teller (CNTFRQ_EL0, gezet door de firmware).
    #[inline]
    pub(crate) fn counter_hz() -> u64 {
        let v: u64;
        // SAFETY: een lees van een systeemregister zonder bijwerkingen.
        unsafe { asm!("mrs {}, cntfrq_el0", out(reg) v, options(nomem, nostack, preserves_flags)) };
        v
    }

    /// ID_AA64MMFR0_EL1: bits 63:60 zijn FEAT_ECV.
    #[inline]
    pub(crate) fn mmfr0() -> u64 {
        let v: u64;
        // SAFETY: een ID-register lezen heeft geen bijwerkingen.
        unsafe {
            asm!("mrs {}, id_aa64mmfr0_el1", out(reg) v, options(nomem, nostack, preserves_flags))
        };
        v
    }

    /// Zet CNTKCTL_EL1 (de event-stream van de generic timer).
    #[inline]
    pub(crate) fn set_cntkctl(v: u64) {
        // SAFETY: CNTKCTL_EL1 regelt alleen de event-stream en de EL0-toegang
        // tot de teller; een app op EL1 mag hem zetten en er hangt geen
        // geheugen van af.
        unsafe {
            asm!("msr cntkctl_el1, {}", "isb", in(reg) v, options(nomem, nostack, preserves_flags))
        };
    }

    /// Eén WFE; geeft de tikken die hij werkelijk duurde.
    #[inline]
    pub(crate) fn wfe() -> u64 {
        let a = counter();
        // SAFETY: WFE wacht op een event (SEV, de event-stream, een
        // interrupt) en raakt geen geheugen of registers.
        unsafe { asm!("wfe", options(nomem, nostack, preserves_flags)) };
        counter().wrapping_sub(a)
    }

    /// De coöperatieve yield naar de EL2-switcher (HVC #1), met de wektijd
    /// in x1: vóór die tellerstand hoeft de rotatie ons niet te hervatten.
    /// Geeft de idle-wall-tijd in tikken (mede-bewoner plus slaap).
    ///
    /// De Go-versie bewaarde V8-V15 en FPCR zelf omdat de switcher op EL2
    /// met de MMU uit geen FP-store naar Device-geheugen kan doen. Dit image
    /// is softfloat: de compiler raakt die registers nooit, dus er valt niets
    /// te bewaren.
    #[inline]
    pub(crate) fn hvc_yield(deadline: u64) -> u64 {
        let a = counter();
        // SAFETY: de switcher bewaart en herstelt onze GP- en
        // systeemregisters en hervat ons na de HVC; `clobber_abi("C")` laat
        // de compiler alle caller-saved registers als verloren beschouwen,
        // dus ook als de switcher er een omgooit, is dat geen fout.
        unsafe { asm!("hvc #1", in("x1") deadline, clobber_abi("C"), options(nostack)) };
        counter().wrapping_sub(a)
    }

    /// Geeft de core aan de kern terug (HVC #0 naar de EL2-parkeerlus).
    /// PSCI CPU_OFF was op de Pi 5-stockfirmware een deur zonder terugweg;
    /// de kern bezit zijn cores en ze gaan nooit terug naar de firmware.
    pub(crate) fn park_exit() -> ! {
        loop {
            // SAFETY: HVC #0 trapt naar de EL2-vectoren van de kern, die de
            // core parkeert; de status staat al op de control-page. Keert
            // in de praktijk niet terug, en doet hij dat toch, dan opnieuw.
            unsafe { asm!("hvc #0", options(nomem, nostack)) };
        }
    }
}

#[cfg(not(all(target_os = "none", target_arch = "aarch64")))]
mod imp {
    //! Host-stub: dezelfde signaturen, geen ijzer. De teller is een
    //! getal dat de tests zetten; slapen en yielden duren niets.
    use core::sync::atomic::{AtomicU64, Ordering::Relaxed};

    /// De nep-teller van de host.
    pub(crate) static FAKE_COUNTER: AtomicU64 = AtomicU64::new(0);

    pub(crate) fn counter() -> u64 {
        FAKE_COUNTER.load(Relaxed)
    }

    pub(crate) fn counter_hz() -> u64 {
        1_000_000_000
    }

    pub(crate) fn mmfr0() -> u64 {
        0
    }

    pub(crate) fn set_cntkctl(_v: u64) {}

    pub(crate) fn wfe() -> u64 {
        0
    }

    pub(crate) fn hvc_yield(_deadline: u64) -> u64 {
        0
    }

    pub(crate) fn park_exit() -> ! {
        loop {
            core::hint::spin_loop();
        }
    }
}
