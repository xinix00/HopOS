//! De trap-ingang van de kern in machine mode (`mtvec` van het hart van de
//! kern): de riscv-tegenhanger van `cpu::vectors`.
//!
//! Het contract is dat van de Go-kern en van de arm64-vector: een interrupt
//! is een wek-signaal, geen plek om code te draaien. De ingang zet de BRON
//! uit in `mie` (MEIE voor de PLIC, MSIE voor de kick, MTIE voor een
//! verdwaalde timer) en wekt de dispatcher ([`crate::irq::on_irq`], dezelfde
//! vlag als op arm64). Op ARM keert de vector terug met I gemaskeerd;
//! hier kan dat niet per bron via `mstatus` (MPIE zet MIE bij `mret` terug),
//! dus is het de `mie`-bit van de bron die dicht blijft tot de dispatch-taak
//! geclaimd heeft en hem weer opent. Zonder dat is een level-lijn van de
//! PLIC een storm: hij staat nog tot iemand claimt.
//!
//! Een exception in machine mode is een bug in de kern: `mcause`, `mepc` en
//! `mtval` naar de console, en het hart parkeert.
//!
//! De ingang bewaart alleen de integer-registers die de C-ABI de aangeroepene
//! laat klobberen. De FP-registers niet: de Rust-kant hieronder raakt alleen
//! atomics en een waker aan, en rekent niet in floating point.

use core::sync::atomic::{AtomicU64, Ordering::Relaxed};

/// Meetlat: hoe vaak de ingang voor een interrupt liep.
pub static IRQ_ENTRIES: AtomicU64 = AtomicU64::new(0);
/// Meetlat: timer-traps. Hoort nul te zijn: de slaap armt de timer alleen
/// met MIE uit; een genomen timer-trap is een vergeten ontwapening.
pub static STRAY_TIMER: AtomicU64 = AtomicU64::new(0);

/// `mcause`-bit 63: een interrupt, geen exception.
pub const CAUSE_INTERRUPT: u64 = 1 << 63;
/// Machine software interrupt (de kick).
pub const IRQ_MSI: u64 = 3;
/// Machine timer interrupt.
pub const IRQ_MTI: u64 = 7;
/// Machine external interrupt (de PLIC).
pub const IRQ_MEI: u64 = 11;

/// Wat de ingang met een interrupt doet: welke `mie`-bit dicht moet en of
/// de dispatcher gewekt wordt. Puur, zodat de host-test het contract toetst.
#[must_use]
pub const fn interrupt_action(code: u64) -> (u64, bool) {
    match code {
        IRQ_MEI => (super::csr::MIP_MEIP, true),
        IRQ_MSI => (super::csr::MIP_MSIP, true),
        IRQ_MTI => (super::csr::MIP_MTIP, false),
        c if c < 64 => (1 << c, false),
        _ => (0, false),
    }
}

/// De Rust-kant van de ingang. Draait in trap-context: atomics en een
/// waker, niets anders.
#[cfg_attr(
    not(all(target_arch = "riscv64", target_os = "none")),
    allow(dead_code) // alleen de ingang roept dit aan
)]
extern "C" fn trap_entry(cause: u64, epc: u64, tval: u64) {
    if cause & CAUSE_INTERRUPT == 0 {
        fault(cause, epc, tval);
    }
    let code = cause & !CAUSE_INTERRUPT;
    let (bit, wake) = interrupt_action(code);
    super::csr::mie_clear(bit);
    if code == IRQ_MTI {
        STRAY_TIMER.fetch_add(1, Relaxed);
    }
    if wake {
        IRQ_ENTRIES.fetch_add(1, Relaxed);
        crate::irq::on_irq();
    }
}

/// De naam van een exception-oorzaak, voor de foutregel.
#[must_use]
pub const fn cause_name(cause: u64) -> &'static str {
    match cause {
        0 => "instruction address misaligned",
        1 => "instruction access fault",
        2 => "illegal instruction",
        3 => "breakpoint",
        4 => "load address misaligned",
        5 => "load access fault",
        6 => "store/AMO address misaligned",
        7 => "store/AMO access fault",
        8 => "ecall from U-mode",
        9 => "ecall from S-mode",
        11 => "ecall from M-mode",
        12 => "instruction page fault",
        13 => "load page fault",
        15 => "store/AMO page fault",
        _ => "?",
    }
}

/// Een fatale exception van de kern zelf: melden en parkeren.
fn fault(cause: u64, epc: u64, tval: u64) -> ! {
    crate::console::emergency(format_args!(
        "exception: {} mcause={cause:#x} mepc={epc:#x} mtval={tval:#x} HOPOS_EXCEPTION",
        cause_name(cause)
    ));
    super::boot::park()
}

#[cfg(all(target_arch = "riscv64", target_os = "none"))]
core::arch::global_asm!(
    r#"
    .section .text.trap, "ax"
    .balign 4
    .global __hopos_trap
__hopos_trap:
    addi sp, sp, -128
    sd ra, 0(sp)
    sd t0, 8(sp)
    sd t1, 16(sp)
    sd t2, 24(sp)
    sd t3, 32(sp)
    sd t4, 40(sp)
    sd t5, 48(sp)
    sd t6, 56(sp)
    sd a0, 64(sp)
    sd a1, 72(sp)
    sd a2, 80(sp)
    sd a3, 88(sp)
    sd a4, 96(sp)
    sd a5, 104(sp)
    sd a6, 112(sp)
    sd a7, 120(sp)
    csrr a0, mcause
    csrr a1, mepc
    csrr a2, mtval
    call {entry}
    ld ra, 0(sp)
    ld t0, 8(sp)
    ld t1, 16(sp)
    ld t2, 24(sp)
    ld t3, 32(sp)
    ld t4, 40(sp)
    ld t5, 48(sp)
    ld t6, 56(sp)
    ld a0, 64(sp)
    ld a1, 72(sp)
    ld a2, 80(sp)
    ld a3, 88(sp)
    ld a4, 96(sp)
    ld a5, 104(sp)
    ld a6, 112(sp)
    ld a7, 120(sp)
    addi sp, sp, 128
    mret
"#,
    entry = sym trap_entry,
);

#[cfg(test)]
mod tests {
    use super::*;
    use crate::riscv::csr::{MIP_MEIP, MIP_MSIP, MIP_MTIP};

    #[test]
    fn each_source_closes_its_own_enable() {
        assert_eq!(interrupt_action(IRQ_MEI), (MIP_MEIP, true));
        assert_eq!(interrupt_action(IRQ_MSI), (MIP_MSIP, true));
        assert_eq!(interrupt_action(IRQ_MTI), (MIP_MTIP, false));
        assert_eq!(interrupt_action(9), (1 << 9, false));
        assert_eq!(interrupt_action(99), (0, false));
    }

    #[test]
    fn the_cause_has_a_name() {
        assert_eq!(cause_name(7), "store/AMO access fault");
    }
}
