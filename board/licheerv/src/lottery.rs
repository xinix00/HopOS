//! De boot-hart-loterij (Go, board/licheerv/lottery.go en
//! hop/cpuinit_riscv64.s): de FSBL start het image op de C906B (1 GHz), maar
//! de kern hoort op de C906L (700 MHz), zodat de apps de grote core krijgen.
//! Het allereerste dat draait, vóór de boot-stub (`cpu::riscv::boot`, feature
//! `lottery`), op elk hart dat `_start` binnenkomt:
//!
//! - ben ik de C906L, door de loterij gestart? Dat zegt de vector-override:
//!   aan mét `_start` erin zet alleen de loterij, de FSBL boot de C906B
//!   zonder. Dan het levensteken, en door als de kern;
//! - anders ben ik de C906B: de C906L hard in reset (de FSBL laat hem met
//!   vendor-code draaiend achter, gemeten 17-08), de vector op `_start`,
//!   los, en wachten op het levensteken. Komt het, dan parkeer ik als
//!   app-hart 0 in het postvak van de boot-stub. Blijft het tien seconden
//!   uit, dan de C906L terug in reset en zelf de kern (voortgang 2): een
//!   mislukte wissel is een console-regel, geen baksteen.
//!
//! Discriminator: GEEN `mhartid` (beide cores lezen 0, gemeten 01-08) en
//! GEEN reset-bit. `_start` ligt op elk image van dit board op
//! 0x8400_0000 (link-riscv.ld, image/licheerv-agent.sh toetst het), dus de
//! toets overleeft een kern-flip: de nieuwe kern komt op de C906L `_start`
//! in en herkent zichzelf; de C906B komt na een flip nooit op `_start`
//! (hij wacht in de uit-stub van de switcher op een parkeer-ingang).
//!
//! Alles vóór de boot-stub met de D-cache uit, zodat elk woord DRAM-echt
//! is (de cores zijn niet coherent; Go's boot 4 zag markers in de cache
//! blijven hangen). Het loterij-blok staat op de boot-scratch in de regel
//! 0x40..0x7F (de woorden van de stubs, `abi::layout::HANDOFF_PTR_OFF`):
//! +64 de voortgang, +88 het levensteken, de offsets van Go.
//!
//! UIT sinds 03-10 (de feature `lottery` van dit board, niet in de build):
//! de PLIC van de C906L heeft geen ethernetbron ([`super::GMAC_IRQ`]), dus
//! de kern hoort op de C906B voor de dwmac-interrupt 31 in plaats van de
//! pomp van 300 µs (46 tot 48 procent van de kern in rust). Zonder de
//! feature is [`state`] altijd [`State::None`], en herkent [`on_little`] een
//! kern die toch op de C906L wakker wordt (een koude flip vanaf een
//! loterij-kern) zodat `discover` het bord reset.

use core::sync::atomic::{AtomicU64, Ordering::Relaxed};
use dev::Pa;

/// De voortgang: 1 = de C906B parkeert als app-hart, 2 = zelfredding.
const PROGRESS_OFF: u64 = 64;
/// Het levensteken van de C906L.
const ALIVE_OFF: u64 = 88;
/// De zelfredding: 10 s op de timebase van 25 MHz.
const RESCUE_TICKS: u64 = 10 * super::TIMEBASE_HZ;

// De regel 0x40..0x7F is van de stubs; het handoff-paar ligt erna. En de
// zelfredding is tien seconden op 25 MHz, zoals in Go (RESCUETK).
const _: () = {
    assert!(PROGRESS_OFF >= 0x40 && ALIVE_OFF + 8 <= abi::layout::HANDOFF_PTR_OFF);
    assert!(RESCUE_TICKS == 250_000_000);
};

/// Hoe de loterij afliep.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
pub enum State {
    /// De kern draait op de C906L, de C906B is het app-hart.
    Swapped,
    /// De C906L gaf geen levensteken: de kern bleef op de C906B, de C906L
    /// staat in reset en is het app-hart (de oude rolverdeling).
    Rescued,
    /// Geen loterij-blok (een image zonder de loterij, of DRAM-vuil).
    None,
}

/// De uitkomst, één keer gelezen (de boot-scratch is gecachet DRAM;
/// daarna een woord in het image).
static STATE: AtomicU64 = AtomicU64::new(0);

/// De uitkomst van de loterij op dit hart; zonder de feature altijd
/// [`State::None`] (het blok op de boot-scratch kan van een vorige boot
/// zijn: DRAM overleeft een reset en een koude flip).
#[must_use]
pub fn state() -> State {
    if !cfg!(feature = "lottery") {
        return State::None;
    }
    let decode = |v: u64| match v {
        1 => State::Swapped,
        2 => State::Rescued,
        _ => State::None,
    };
    match STATE.load(Relaxed) {
        0 => {
            let pa = Pa(super::slots::BOOT_SCRATCH_PA + PROGRESS_OFF);
            dev::pull(pa, 8);
            let v = dev::read64(pa);
            STATE.store(if matches!(v, 1 | 2) { v } else { 3 }, Relaxed);
            decode(v)
        }
        v => decode(v),
    }
}

/// Het hart van de kern: 1 (de C906L) na een geslaagde wissel, anders 0.
#[must_use]
pub fn os_hart() -> usize {
    usize::from(state() == State::Swapped)
}

/// Ben ik de C906L, op `_start` gezet door een loterij? De discriminator
/// van de asm hieronder: de vector-override aan mét `_start` erin. Een kern
/// zonder loterij zet hem zelf nooit zo (`start_little` zet de
/// reset-ingang, niet `_start`).
#[must_use]
pub fn on_little() -> bool {
    let vec = u64::from(dev::read32(super::SEC_SYS_VEC_LO))
        | u64::from(dev::read32(super::SEC_SYS_VEC_HI)) << 32;
    dev::read32(super::SEC_SYS_CTRL) & OVERRIDE != 0 && vec == start_pc()
}

/// De vector-override in SEC_SYS_CTRL.
const OVERRIDE: u32 = 1 << 13;

#[cfg(all(target_arch = "riscv64", target_os = "none"))]
fn start_pc() -> u64 {
    unsafe extern "C" {
        /// De eerste instructie van het image (`cpu::riscv::boot`).
        static _start: u8;
    }
    (&raw const _start) as u64
}

/// Host: geen image, geen `_start`.
#[cfg(not(all(target_arch = "riscv64", target_os = "none")))]
fn start_pc() -> u64 {
    u64::MAX
}

#[cfg(all(feature = "lottery", target_arch = "riscv64", target_os = "none"))]
core::arch::global_asm!(
    r#"
    .section .text.hopos_lottery, "ax"
    .global __hopos_lottery
__hopos_lottery:
    // 0. Het T-Head-regime aan (zonder THEADISAEE is th.dcache.* een
    //    illegal instruction), terugschrijven, en de D-cache uit: alles
    //    hierna is DRAM-echt.
    li t0, 0xc0638000
    csrw 0x7c0, t0
    .4byte 0x0030000b
    .4byte 0x01b0000b
    li t0, 2
    csrc 0x7c1, t0
    // 1. De override aan mét _start erin: dan ben ik de gestarte C906L.
    li t0, {ctrl}
    lw t1, 0(t0)
    li t2, {ovr}
    and t1, t1, t2
    beqz t1, 2f
    la t2, _start
    li t0, {veclo}
    lwu t1, 0(t0)
    slli t3, t2, 32
    srli t3, t3, 32
    bne t1, t3, 2f
    li t0, {vechi}
    lwu t1, 0(t0)
    srli t3, t2, 32
    bne t1, t3, 2f
    li t0, {scratch}
    li t1, 1
    sd t1, {alive}(t0)
    fence
    li s3, -1
    la t0, __hopos_stubenter
    jr t0
    // 2. De C906B: de C906L hard in reset, de vector op _start, los.
2:  li t0, {rstn}
    lw t1, 0(t0)
    li t2, {rbit}
    not t2, t2
    and t1, t1, t2
    sw t1, 0(t0)
    fence
    la t1, _start
    li t0, {veclo}
    sw t1, 0(t0)
    srli t1, t1, 32
    li t0, {vechi}
    sw t1, 0(t0)
    li t0, {ctrl}
    lw t1, 0(t0)
    li t2, {ovr}
    or t1, t1, t2
    sw t1, 0(t0)
    fence
    li t0, {scratch}
    sd zero, {alive}(t0)
    li t1, 1
    sd t1, {progress}(t0)
    fence
    li t0, {rstn}
    lw t1, 0(t0)
    li t2, {rbit}
    or t1, t1, t2
    sw t1, 0(t0)
    fence
    // 3. Wachten op het levensteken, hooguit RESCUE tikken; tussen twee
    //    blikken 10 000 tikken (400 us) pauze: spinnen op DRAM naast de
    //    net-DMA van de kern is anders 100 procent duty voor niets.
    li t0, {scratch}
    rdtime t3
    li t4, {rescue}
    add t3, t3, t4
3:  ld t1, {alive}(t0)
    bnez t1, 5f
    rdtime t4
    bgeu t4, t3, 4f
    li t5, 10000
    add t4, t4, t5
31: rdtime t5
    bltu t5, t4, 31b
    j 3b
    // 4. Zelfredding: de C906L terug in reset, voortgang 2, zelf de kern.
4:  li t0, {rstn}
    lw t1, 0(t0)
    li t2, {rbit}
    not t2, t2
    and t1, t1, t2
    sw t1, 0(t0)
    fence
    li t0, {scratch}
    li t1, 2
    sd t1, {progress}(t0)
    fence
    li s3, -1
    la t0, __hopos_stubenter
    jr t0
    // 5. De kern leeft op de C906L: parkeren als app-hart 0.
5:  li s3, 1
    la t0, __hopos_stubenter
    jr t0
"#,
    ctrl = const super::SEC_SYS_CTRL.0,
    veclo = const super::SEC_SYS_VEC_LO.0,
    vechi = const super::SEC_SYS_VEC_HI.0,
    ovr = const OVERRIDE,
    rstn = const super::C906L_RESET.0,
    rbit = const super::RESET_BIT,
    scratch = const super::slots::BOOT_SCRATCH_PA,
    progress = const PROGRESS_OFF,
    alive = const ALIVE_OFF,
    rescue = const RESCUE_TICKS,
);
