//! De arm64-vectortabel van HopOS (EL2).
//!
//! De IRQ-deur is het contract uit de Go-kern (`cpu/idle/irqdoor_arm64.go`):
//! een interrupt is een wek-signaal, geen plek om code te draaien. De
//! vector wekt de dispatcher ([`crate::irq::on_irq`]: een `Signal::set`,
//! dus atomics en een SEV) en keert terug met I gemaskeerd;
//! de lijn blijft bij de GIC staan tot de dispatch-taak hem claimt, en die
//! opent I daarna weer. Waarom zo streng: in de
//! Go-kern draaide tamago runtime-code ín de exception-context, op de stack
//! van wat er onderbroken werd. Op de M4 gaf dat de findTimer-crash
//! (toolchain-patch 0003), op de Ampere een stille hang binnen seconden na
//! de eerste NIC-interrupts (19-09, L83: zes flips op rij, geen exception,
//! geen regel, gewoon weg), en op de M4 onder load een verloren heropening
//! van het I-masker waarna de pomp op zijn 10 ms-vangrail bleef hangen
//! (21-09, L83 p42). Een vector die één woord schrijft kan dat allemaal
//! niet.
//!
//! Een synchrone exception op EL2 is een bug in de kern: ESR, ELR en FAR
//! gaan naar de console en de core parkeert. Een beurt van een bewoner
//! draait op de vectoren van [`crate::el2`]; een lagere EL die hier landt,
//! is net zo fataal.

use core::sync::atomic::{
    AtomicU64,
    Ordering::{Acquire, Relaxed, Release},
};

/// De IRQ-ingang: de dispatcher wekken. Meer niet: geen claim, geen
/// allocatie, geen `Local`.
#[cfg_attr(
    not(all(target_arch = "aarch64", target_os = "none")),
    allow(dead_code) // alleen de vector roept dit aan
)]
extern "C" fn irq_entry() {
    crate::irq::on_irq();
}

/// ESR.EC van een HVC uit AArch64.
pub const EC_HVC64: u64 = 0x16;

/// De namen van de zestien ingangen, voor de foutregel.
const KINDS: [&str; 16] = [
    "sync/el2-sp0",
    "irq/el2-sp0",
    "fiq/el2-sp0",
    "serror/el2-sp0",
    "sync/el2",
    "irq/el2",
    "fiq/el2",
    "serror/el2",
    "sync/lower-a64",
    "irq/lower-a64",
    "fiq/lower-a64",
    "serror/lower-a64",
    "sync/lower-a32",
    "irq/lower-a32",
    "fiq/lower-a32",
    "serror/lower-a32",
];

/// Een fatale exception: melden en parkeren. Aangeroepen vanuit de vector.
#[cfg_attr(
    not(all(target_arch = "aarch64", target_os = "none")),
    allow(dead_code) // alleen de vector roept dit aan
)]
extern "C" fn fault(kind: u64, esr: u64, elr: u64, far: u64) -> ! {
    let name = usize::try_from(kind)
        .ok()
        .and_then(|k| KINDS.get(k))
        .copied()
        .unwrap_or("?");
    crate::console::emergency(format_args!(
        "exception: {name} ESR={esr:#x} (EC {:#x}) ELR={elr:#x} FAR={far:#x} HOPOS_EXCEPTION",
        esr >> 26
    ));
    crate::boot::park()
}

/// Staat de SError-drain gewapend? Dan neemt [`serror_el2`] een SError
/// van de kern zelf op in plaats van te parkeren.
static SERROR_DRAIN: core::sync::atomic::AtomicBool = core::sync::atomic::AtomicBool::new(false);
/// Wat de drain zag: het aantal, en ESR, ELR en FAR van de laatste.
static SERROR_SEEN: [AtomicU64; 4] = [
    AtomicU64::new(0),
    AtomicU64::new(0),
    AtomicU64::new(0),
    AtomicU64::new(0),
];

/// De SError van de kern zelf (ingang 7). Op Apple-silicium is een
/// verboden schrijf stil en landt de abort later als SError (Go, 29-08);
/// op EL2 blijft hij achter PSTATE.A staan tot de eerste beurt op EL1 hem
/// neemt (de M4, 30-09: de voorproef zag drie keer ESR 0xbe000000).
/// Gewapend door [`serror_drain`] wordt hij opgenomen en keert de kern
/// terug; anders is hij fataal, zoals altijd.
#[cfg_attr(
    not(all(target_arch = "aarch64", target_os = "none")),
    allow(dead_code) // alleen de vector roept dit aan
)]
extern "C" fn serror_el2(esr: u64, elr: u64, far: u64) {
    if !SERROR_DRAIN.load(Acquire) {
        fault(7, esr, elr, far);
    }
    SERROR_SEEN[1].store(esr, Relaxed);
    SERROR_SEEN[2].store(elr, Relaxed);
    SERROR_SEEN[3].store(far, Relaxed);
    SERROR_SEEN[0].fetch_add(1, Release);
}

/// Neemt een SError op die nu achter PSTATE.A pending staat: A even open,
/// een ISB, en weer dicht. Geeft `(esr, elr, far)` van wat er viel, of
/// `None`. Voor een board dat na elke bootstap wil weten of een schrijf
/// stil misging (board/apple), en vlak vóór de eerste beurt op EL1.
#[must_use]
pub fn serror_drain() -> Option<(u64, u64, u64)> {
    let before = SERROR_SEEN[0].load(Acquire);
    SERROR_DRAIN.store(true, Release);
    #[cfg(all(target_arch = "aarch64", target_os = "none"))]
    // SAFETY: A kort openen op EL2 raakt geen geheugen; een SError die valt
    // gaat door ingang 7 naar `serror_el2`, dat met de drain gewapend
    // terugkeert, en de tweede msr sluit A weer vóór de drain ontwapent.
    unsafe {
        core::arch::asm!(
            "msr daifclr, #4",
            "isb",
            "dsb sy",
            "isb",
            "msr daifset, #4",
            options(nomem, nostack)
        );
    }
    SERROR_DRAIN.store(false, Release);
    if SERROR_SEEN[0].load(Acquire) == before {
        return None;
    }
    Some((
        SERROR_SEEN[1].load(Relaxed),
        SERROR_SEEN[2].load(Relaxed),
        SERROR_SEEN[3].load(Relaxed),
    ))
}

#[cfg(all(target_arch = "aarch64", target_os = "none"))]
core::arch::global_asm!(
    r#"
    .section .text.vectors, "ax"
    .balign 2048
    .global __hopos_vectors
__hopos_vectors:
    // Zestien ingangen van 0x80 bytes (ARM DDI 0487, D1.10.2). Elke ingang
    // springt meteen door; de handlers staan erachter.
    .irp kind, 0, 1, 2, 3
    .balign 0x80
    mov x0, #\kind
    b 90f
    .endr

    // Huidige EL met SP_EL2: hier draait de kern.
    .balign 0x80
    mov x0, #4
    b 90f
    .balign 0x80
    b 80f
    .balign 0x80
    b 81f
    .balign 0x80
    b 84f

    // Lagere EL, AArch64: de slots.
    .balign 0x80
    mov x0, #8
    b 90f
    .balign 0x80
    b 80f
    .balign 0x80
    b 81f
    .balign 0x80
    mov x0, #11
    b 90f

    // Lagere EL, AArch32: bestaat bij ons niet.
    .irp kind, 12, 13, 14, 15
    .balign 0x80
    mov x0, #\kind
    b 90f
    .endr

    // De IRQ-deur: de caller-saved registers redden (x0-x18, x30), de
    // ingang roepen, en terug met I (IRQ) of F (FIQ) gemaskeerd in de
    // bewaarde PSTATE. Het doel is geen softfloat-code met SIMD, dus de
    // FP-registers blijven onaangeroerd.
80:
    sub sp, sp, #(20 * 8)
    stp x0, x1, [sp, #(0 * 8)]
    mov x1, #(1 << 7)
    b 82f
81:
    sub sp, sp, #(20 * 8)
    stp x0, x1, [sp, #(0 * 8)]
    mov x1, #(1 << 6)
82:
    stp x2, x3, [sp, #(2 * 8)]
    stp x4, x5, [sp, #(4 * 8)]
    stp x6, x7, [sp, #(6 * 8)]
    stp x8, x9, [sp, #(8 * 8)]
    stp x10, x11, [sp, #(10 * 8)]
    stp x12, x13, [sp, #(12 * 8)]
    stp x14, x15, [sp, #(14 * 8)]
    stp x16, x17, [sp, #(16 * 8)]
    stp x18, x30, [sp, #(18 * 8)]
    mrs x0, spsr_el2
    orr x0, x0, x1
    msr spsr_el2, x0
    bl {entry}
    ldp x18, x30, [sp, #(18 * 8)]
    ldp x16, x17, [sp, #(16 * 8)]
    ldp x14, x15, [sp, #(14 * 8)]
    ldp x12, x13, [sp, #(12 * 8)]
    ldp x10, x11, [sp, #(10 * 8)]
    ldp x8, x9, [sp, #(8 * 8)]
    ldp x6, x7, [sp, #(6 * 8)]
    ldp x4, x5, [sp, #(4 * 8)]
    ldp x2, x3, [sp, #(2 * 8)]
    ldp x0, x1, [sp, #(0 * 8)]
    add sp, sp, #(20 * 8)
    eret

    // De SError-deur van de kern zelf (ingang 7): de caller-saved registers
    // redden, naar Rust, en terug met A gemaskeerd in de bewaarde PSTATE.
    // Rust beslist: een gewapende drain neemt hem op en keert terug, anders
    // is hij fataal zoals de andere ingangen.
84:
    sub sp, sp, #(20 * 8)
    stp x0, x1, [sp, #(0 * 8)]
    stp x2, x3, [sp, #(2 * 8)]
    stp x4, x5, [sp, #(4 * 8)]
    stp x6, x7, [sp, #(6 * 8)]
    stp x8, x9, [sp, #(8 * 8)]
    stp x10, x11, [sp, #(10 * 8)]
    stp x12, x13, [sp, #(12 * 8)]
    stp x14, x15, [sp, #(14 * 8)]
    stp x16, x17, [sp, #(16 * 8)]
    stp x18, x30, [sp, #(18 * 8)]
    mrs x0, spsr_el2
    orr x0, x0, #(1 << 8)
    msr spsr_el2, x0
    mrs x0, esr_el2
    mrs x1, elr_el2
    mrs x2, far_el2
    bl {serror}
    ldp x18, x30, [sp, #(18 * 8)]
    ldp x16, x17, [sp, #(16 * 8)]
    ldp x14, x15, [sp, #(14 * 8)]
    ldp x12, x13, [sp, #(12 * 8)]
    ldp x10, x11, [sp, #(10 * 8)]
    ldp x8, x9, [sp, #(8 * 8)]
    ldp x6, x7, [sp, #(6 * 8)]
    ldp x4, x5, [sp, #(4 * 8)]
    ldp x2, x3, [sp, #(2 * 8)]
    ldp x0, x1, [sp, #(0 * 8)]
    add sp, sp, #(20 * 8)
    eret

    // Fataal: soort in x0, de drie registers erbij, naar Rust.
90:
    mrs x1, esr_el2
    mrs x2, elr_el2
    mrs x3, far_el2
    b {fault}
"#,
    entry = sym irq_entry,
    fault = sym fault,
    serror = sym serror_el2,
);
