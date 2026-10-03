//! De boot-stub van een RISC-V-hart in machine mode: van de eerste
//! instructie tot `kmain`, en de parkeerlus van de andere harts.
//!
//! HopOS draait op RISC-V in MACHINE MODE, zoals de Go-generatie: de kooi is
//! een PMP-whitelist plus een Sv39-tabel (`kern/cage` in Go, [`super::pmp`]
//! en [`super::sv39`] hier), en PMP programmeren kan alleen machine mode. Op
//! de LicheeRV neemt het image daarom de plek van OpenSBI in (het
//! MONITOR-slot van de FIP, image/licheerv-agent.sh); op QEMU is dat
//! `-bios none`, waarop QEMU elk hart in M-mode op 0x8000_0000 laat
//! beginnen. Onder OpenSBI (`-bios default`) zou de kern in S-mode landen en
//! is de PMP van de firmware: dan is er geen kooi, en dat is dezelfde
//! weigering als "HopOS eist EL2" op ARM (`kmain` meldt het niveau).
//!
//! Hart 0 gaat door: stack, BSS, `mtvec`, de FPU aan, en `kmain(dtb, 3)`
//! (3 = machine mode, in de rol van het EL op ARM). Elk ander hart parkeert
//! in een `wfi`-lus met alleen MSIE aan: [`start_hart`] zet een ingang in
//! zijn postvak en belt hem met zijn `msip`. Dat is het PSCI-equivalent van
//! deze generatie: onder OpenSBI zou het SBI HSM `hart_start` zijn, maar
//! machine mode heeft niemand onder zich om het aan te vragen.
//!
//! Het linkcontract (`hopos/link-riscv.ld`): `__stack_top`, `__bss_start`,
//! `__bss_end` (8-gealigneerd), `__hopos_trap` ([`super::trap`]) en
//! `kmain`: `extern "C" fn(dtb: u64, mode: u64) -> !`.

use core::sync::atomic::{AtomicU64, Ordering::Release};
use dev::Pa;

/// Het grootste hart-id dat de parkeerlus kent. De SG2002 heeft er twee,
/// QEMU virt tot acht per socket.
pub const MAX_HARTS: usize = 8;

/// Het privilege-niveau dat de stub aan `kmain` meldt: machine mode.
pub const MODE_MACHINE: u64 = 3;

/// Het postvak van de geparkeerde harts: de ingang (0 = blijf parkeren).
///
/// In `.data` en niet in `.bss`: hart 0 wist de BSS terwijl de andere harts
/// al kunnen kijken. Op QEMU begint RAM leeg, maar op ijzer is BSS vóór het
/// wissen DRAM-vuil, en een hart dat vuil als ingang leest springt het niets
/// in. `.data` staat in het image en is dus nul vóór het eerste hart loopt.
#[cfg_attr(target_os = "none", unsafe(link_section = ".data.hartpark"))]
static HART_ENTRY: [AtomicU64; MAX_HARTS] = [const { AtomicU64::new(0) }; MAX_HARTS];
/// Het argument dat een gestart hart in a1 meekrijgt.
#[cfg_attr(target_os = "none", unsafe(link_section = ".data.hartpark"))]
static HART_ARG: [AtomicU64; MAX_HARTS] = [const { AtomicU64::new(0) }; MAX_HARTS];
/// De `msip`-PA van elk hart, zodat het zijn eigen bel kan wissen vóór de
/// sprong (de CLINT is board-kennis; de starter geeft hem mee).
#[cfg_attr(target_os = "none", unsafe(link_section = ".data.hartpark"))]
static HART_MSIP: [AtomicU64; MAX_HARTS] = [const { AtomicU64::new(0) }; MAX_HARTS];

/// Het hart-id dat de reset-ingang ([`reset_pc`]) aan zijn hart geeft. Op
/// de SG2002 noemen BEIDE cores zichzelf `mhartid` 0 (gemeten 01-08, boot
/// 8): een C906L die uit reset `_start` inliep, nam het pad van het
/// boot-hart en draaide `kmain` een tweede keer. Wie een hart uit reset
/// haalt, zegt daarom hier wie het is ([`set_reset_hart`]), vóór de reset
/// losgaat. In `.data`, om dezelfde reden als het postvak.
#[cfg_attr(target_os = "none", unsafe(link_section = ".data.hartpark"))]
static RESET_HART: AtomicU64 = AtomicU64::new(u64::MAX);

/// Welke harts [`start_hart`] al een ingang gaf (bit per hart): de kooi
/// start een app-hart één keer; daarna leeft het in de switcher en wekt de
/// kern het met zijn `msip`.
static STARTED: AtomicU64 = AtomicU64::new(0);

/// Waarom een hart niet gestart kon worden.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum StartError {
    /// Het hart-id valt buiten [`MAX_HARTS`].
    NoSuchHart {
        /// Het hart.
        hart: usize,
    },
    /// De ingang is nul: dat leest de parkeerlus als "blijf staan".
    NoEntry,
}

impl core::fmt::Display for StartError {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        match *self {
            Self::NoSuchHart { hart } => {
                write!(f, "hart {hart} outside the park (max {MAX_HARTS})")
            }
            Self::NoEntry => f.write_str("hart start with entry 0"),
        }
    }
}

/// Start een geparkeerd hart op `entry` met `a0 = hart-id` en `a1 = arg`:
/// eerst het argument en de bel, dan de ingang (Release), dan de `msip`.
/// Het hart wist zijn eigen `msip` en zijn postvak vóór hij springt, dus
/// een tweede start van hetzelfde hart is een nieuw startschot.
///
/// De ingang draait zonder stack en met `mtvec` op de trap van de kern; wie
/// Rust wil draaien, zet eerst zijn eigen `sp` (de switcher doet dat met
/// `mscratch`).
///
/// `msip` = `Pa(0)`: dit hart heeft geen bel die de kern kan luiden (de
/// SG2002: de CLINT is per core, er is geen IPI tussen de harts, Go 01-08).
/// Dan staat het postvak klaar en haalt het hart het op zodra het de
/// parkeerlus bereikt, uit reset via [`reset_pc`].
pub fn start_hart(hart: usize, entry: u64, arg: u64, msip: Pa) -> Result<(), StartError> {
    if entry == 0 {
        return Err(StartError::NoEntry);
    }
    let (Some(e), Some(a), Some(m)) = (
        HART_ENTRY.get(hart),
        HART_ARG.get(hart),
        HART_MSIP.get(hart),
    ) else {
        return Err(StartError::NoSuchHart { hart });
    };
    a.store(arg, Release);
    m.store(msip.0, Release);
    e.store(entry, Release);
    // Het postvak staat in gecachet RAM; op QEMU is dat coherent, op de
    // C906 niet: publiceer de drie woorden vóór de bel.
    dev::push(
        Pa(HART_ENTRY.as_ptr() as u64),
        core::mem::size_of_val(&HART_ENTRY),
    );
    dev::push(
        Pa(HART_ARG.as_ptr() as u64),
        core::mem::size_of_val(&HART_ARG),
    );
    dev::push(
        Pa(HART_MSIP.as_ptr() as u64),
        core::mem::size_of_val(&HART_MSIP),
    );
    if msip.0 != 0 {
        dev::write32(msip, 1);
    }
    if let Some(bit) = 1u64.checked_shl(hart as u32) {
        STARTED.fetch_or(bit, Release);
    }
    Ok(())
}

/// Gaf [`start_hart`] dit hart al een ingang (sinds de boot)?
#[must_use]
pub fn is_started(hart: usize) -> bool {
    1u64.checked_shl(hart as u32)
        .is_some_and(|bit| STARTED.load(core::sync::atomic::Ordering::Acquire) & bit != 0)
}

/// Zet het hart-id dat [`reset_pc`] meegeeft aan het volgende hart dat uit
/// reset komt, en publiceert het (de C906L leest het met zijn cache nog
/// uit).
pub fn set_reset_hart(hart: usize) {
    RESET_HART.store(hart as u64, Release);
    dev::push(
        Pa(core::ptr::from_ref(&RESET_HART) as u64),
        core::mem::size_of_val(&RESET_HART),
    );
}

/// Het fysieke adres van de reset-ingang: dezelfde stub als `_start`, maar
/// het hart-id komt uit [`set_reset_hart`] en niet uit `mhartid`, en het
/// hart gaat altijd de parkeerlus in. Voor een hart dat de kern zelf uit
/// reset haalt (de C906L van de LicheeRV, `start_little`).
#[must_use]
pub fn reset_pc() -> u64 {
    imp::reset_pc()
}

#[cfg(all(target_arch = "riscv64", target_os = "none"))]
mod imp {
    pub(super) fn reset_pc() -> u64 {
        unsafe extern "C" {
            /// De reset-ingang (hieronder).
            static __hopos_resetenter: u8;
        }
        (&raw const __hopos_resetenter) as u64
    }
}

#[cfg(not(all(target_arch = "riscv64", target_os = "none")))]
mod imp {
    //! Host-stub: er is geen stub.
    pub(super) fn reset_pc() -> u64 {
        0
    }
}

/// Parkeert dit hart voor altijd: een `wfi`-lus met de interrupts dicht.
pub fn park() -> ! {
    super::csr::mask();
    loop {
        super::csr::wfi();
    }
}

// Het T-Head-regime van de C906, voor ELK hart en als eerste: mxstatus met
// MAEE en THEADISAEE (de gemeten FSBL-waarde 0xc0638000; zonder
// THEADISAEE is elke `th.dcache.*` een illegal instruction), dan de hele
// D-cache terug naar DRAM (`th.dcache.ciall`, zodat het invalideren niets
// verliest van wat de FSBL nog vuil liet), dan I+D+branch invalideren
// (`mcor`), en de vendor-regimes voor perf en caches (`mhint` 0x16e30c,
// `mhcr` 0x11ff: I$ en D$ aan).
//
// Waarom op elk hart: de C906L komt kaal uit reset en de FSBL heeft er
// nooit gedraaid. Elke niet-gezette bit blijft 0, en dat was I-CACHE UIT:
// HopOS draaide in Go wekenlang met instructie-fetches uit DRAM (gemeten
// 18-08: 788 µs per frame op de C906L tegen 9,3 op de C906B, ~77×). Op de
// C906B zijn de waarden al gezet en is het schrijven idempotent.
#[cfg(all(feature = "thead", target_arch = "riscv64", target_os = "none"))]
macro_rules! thead_regime {
    () => {
        r#"
    li t0, 0xc0638000
    csrw 0x7c0, t0
    .4byte 0x0030000b
    .4byte 0x01b0000b
    li t0, 0x70003
    csrw 0x7c2, t0
    li t0, 0x16e30c
    csrw 0x7c5, t0
    li t0, 0x11ff
    csrw 0x7c1, t0
    fence.i
"#
    };
}

// Zonder `thead`: niets (QEMU virt kent de T-Head-CSRs niet).
#[cfg(all(not(feature = "thead"), target_arch = "riscv64", target_os = "none"))]
macro_rules! thead_regime {
    () => {
        ""
    };
}

// De stub. s0 = hart-id, s1 = DTB (a1 van QEMU of de FSBL).
//
// Secundaire harts: MSIE aan en MIE uit, dus een `msip` wekt de `wfi`
// zonder trap (privileged spec 3.3.3); daarna het postvak lezen. Een valse
// wek (die mag) leest een nul en slaapt verder.
#[cfg(all(target_arch = "riscv64", target_os = "none"))]
core::arch::global_asm!(
    r#"
    .section .text.boot, "ax"
    .global _start
_start:
    li s3, -1
    j 10f
    // De reset-ingang: hetzelfde regime, maar het hart-id uit RESET_HART
    // en altijd de parkeerlus in (zie `reset_pc`).
    .global __hopos_resetenter
__hopos_resetenter:
    li s3, 0
10:
    csrw mie, zero
    la t0, __hopos_trap
    csrw mtvec, t0
"#,
    thead_regime!(),
    r#"
    li t0, {fs}
    csrs mstatus, t0
    csrr s0, mhartid
    mv s1, a1
    bltz s3, 11f
    la t0, {reset}
    ld s0, 0(t0)
    j 20f
11:
    bnez s0, 20f

    la sp, __stack_top
    la t1, __bss_start
    la t2, __bss_end
1:  bgeu t1, t2, 2f
    sd zero, 0(t1)
    addi t1, t1, 8
    j 1b
2:
    mv a0, s1
    li a1, {mode}
    call kmain
9:  wfi
    j 9b

20:
    li t0, 8
    csrs mie, t0
    li t0, {max}
    bgeu s0, t0, 9b
    slli s2, s0, 3
    // Eerst kijken, dan slapen: een hart uit reset (zonder bel) vindt zijn
    // ingang al in het postvak. Een bel ná de toets staat pending en laat
    // de `wfi` meteen terugkeren.
    j 23f
21: wfi
23: la t1, {entry}
    add t1, t1, s2
    ld t3, 0(t1)
    beqz t3, 21b
    sd zero, 0(t1)
    la t1, {msip}
    add t1, t1, s2
    ld t2, 0(t1)
    beqz t2, 22f
    sw zero, 0(t2)
22: la t1, {arg}
    add t1, t1, s2
    ld a1, 0(t1)
    mv a0, s0
    csrw mie, zero
    fence
    fence.i
    jr t3
"#,
    fs = const super::csr::MSTATUS_FS_INITIAL,
    mode = const MODE_MACHINE,
    max = const MAX_HARTS,
    entry = sym HART_ENTRY,
    arg = sym HART_ARG,
    msip = sym HART_MSIP,
    reset = sym RESET_HART,
);

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn start_refuses_nonsense() {
        let mut bell = [0u32; 1];
        let pa = Pa(bell.as_mut_ptr() as u64);
        assert_eq!(
            start_hart(MAX_HARTS, 0x1000, 0, pa),
            Err(StartError::NoSuchHart { hart: MAX_HARTS })
        );
        assert_eq!(start_hart(1, 0, 0, pa), Err(StartError::NoEntry));
        start_hart(1, 0x8000_1000, 7, pa).unwrap();
        assert_eq!(bell[0], 1);
        assert_eq!(
            HART_ENTRY[1].load(core::sync::atomic::Ordering::Relaxed),
            0x8000_1000
        );
        assert_eq!(HART_ARG[1].load(core::sync::atomic::Ordering::Relaxed), 7);
    }
}
