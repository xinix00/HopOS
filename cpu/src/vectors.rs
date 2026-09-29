//! De arm64-vectortabel van HopOS (EL2).
//!
//! De IRQ-deur is het contract uit de Go-kern (`cpu/idle/irqdoor_arm64.go`):
//! een interrupt is een wek-signaal, geen plek om code te draaien. De
//! vector zet [`IRQ_FLAG`], wekt de dispatcher ([`crate::irq::on_irq`]: een
//! `Signal::set`, dus atomics en een SEV) en keert terug met I gemaskeerd;
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
//! gaan naar de console en de core parkeert. Een synchrone exception van
//! een lagere EL (een HVC uit een slot) gaat naar de haak die het
//! kooi-spoor zet ([`set_hvc_handler`]); zonder haak is ook dat fataal.

use core::sync::atomic::{
    AtomicPtr, AtomicU32, AtomicU64,
    Ordering::{AcqRel, Acquire, Relaxed, Release},
};

/// De IRQ-vlag: gezet door de vector. Wie de verloren-wek-toets vóór een
/// WFI doet, kijkt ernaar ([`irq_pending`]); wie hem als deur gebruikt,
/// wist hem ([`take_irq`]).
pub static IRQ_FLAG: AtomicU32 = AtomicU32::new(0);

/// Meetlat: hoe vaak de deur een gezette vlag vond.
pub static IRQ_TAKEN: AtomicU64 = AtomicU64::new(0);

/// Meetlat: hoe vaak de IRQ-vector liep.
pub static IRQ_ENTRIES: AtomicU64 = AtomicU64::new(0);

/// De IRQ-ingang: vlag zetten en de dispatcher wekken. Meer niet: geen
/// claim, geen allocatie, geen `Local`.
#[cfg_attr(
    not(all(target_arch = "aarch64", target_os = "none")),
    allow(dead_code) // alleen de vector roept dit aan
)]
extern "C" fn irq_entry() {
    IRQ_FLAG.store(1, Release);
    IRQ_ENTRIES.fetch_add(1, Relaxed);
    crate::irq::on_irq();
}

/// Staat de IRQ-vlag? Kijken, niet wissen: voor de laatste toets vóór WFI.
#[must_use]
pub fn irq_pending() -> bool {
    IRQ_FLAG.load(Acquire) != 0
}

/// Wist de IRQ-vlag en zegt of hij stond: de IRQ-deur.
pub fn take_irq() -> bool {
    let was = IRQ_FLAG.swap(0, AcqRel) != 0;
    if was {
        IRQ_TAKEN.fetch_add(1, Relaxed);
    }
    was
}

/// De registers van een lagere EL op het moment van de trap, zoals de
/// vector ze op de stack legt. De HVC-haak mag `x` aanpassen (de
/// retourwaarden) en `elr` verzetten; bij terugkeer gaat alles terug.
#[repr(C)]
#[derive(Debug)]
pub struct TrapFrame {
    /// x0 tot en met x30.
    pub x: [u64; 31],
    /// ESR_EL2: waarom de trap.
    pub esr: u64,
    /// ELR_EL2: waar de lagere EL verdergaat.
    pub elr: u64,
    /// SPSR_EL2: de PSTATE van de lagere EL.
    pub spsr: u64,
}

const _: () = assert!(core::mem::size_of::<TrapFrame>() == 34 * 8);

/// ESR.EC van een HVC uit AArch64.
pub const EC_HVC64: u64 = 0x16;

/// De HVC-haak als rauwe pointer; null = geen haak.
static HVC_HANDLER: AtomicPtr<()> = AtomicPtr::new(core::ptr::null_mut());

/// Zet de haak voor synchrone traps van een lagere EL (HVC, en wat het
/// kooi-spoor verder vangt). Draait in exception-context: geen allocatie,
/// geen `Local`, geen slot.
pub fn set_hvc_handler(f: fn(&mut TrapFrame)) {
    HVC_HANDLER.store(f as *mut (), Release);
}

fn hvc_handler() -> Option<fn(&mut TrapFrame)> {
    let p = HVC_HANDLER.load(Acquire);
    if p.is_null() {
        return None;
    }
    // SAFETY: alleen `set_hvc_handler` schrijft dit woord, met een geldige
    // `fn(&mut TrapFrame)`; null is hierboven uitgesloten.
    Some(unsafe { core::mem::transmute::<*mut (), fn(&mut TrapFrame)>(p) })
}

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

/// Een synchrone trap van een lagere EL: naar de haak, of fataal.
#[cfg_attr(
    not(all(target_arch = "aarch64", target_os = "none")),
    allow(dead_code) // alleen de vector roept dit aan
)]
extern "C" fn lower_sync(frame: &mut TrapFrame) {
    match hvc_handler() {
        Some(h) => h(frame),
        None => fault(8, frame.esr, frame.elr, 0),
    }
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
    mov x0, #7
    b 90f

    // Lagere EL, AArch64: de slots.
    .balign 0x80
    b 70f
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

    // Fataal: soort in x0, de drie registers erbij, naar Rust.
90:
    mrs x1, esr_el2
    mrs x2, elr_el2
    mrs x3, far_el2
    b {fault}

    // Synchroon van een lagere EL: het volle frame op de stack, de haak
    // aanroepen, en alles terug (de haak mag x0-x30 en ELR aanpassen).
70:
    sub sp, sp, #(34 * 8)
    stp x0, x1, [sp, #(0 * 8)]
    stp x2, x3, [sp, #(2 * 8)]
    stp x4, x5, [sp, #(4 * 8)]
    stp x6, x7, [sp, #(6 * 8)]
    stp x8, x9, [sp, #(8 * 8)]
    stp x10, x11, [sp, #(10 * 8)]
    stp x12, x13, [sp, #(12 * 8)]
    stp x14, x15, [sp, #(14 * 8)]
    stp x16, x17, [sp, #(16 * 8)]
    stp x18, x19, [sp, #(18 * 8)]
    stp x20, x21, [sp, #(20 * 8)]
    stp x22, x23, [sp, #(22 * 8)]
    stp x24, x25, [sp, #(24 * 8)]
    stp x26, x27, [sp, #(26 * 8)]
    stp x28, x29, [sp, #(28 * 8)]
    mrs x0, esr_el2
    stp x30, x0, [sp, #(30 * 8)]
    mrs x0, elr_el2
    mrs x1, spsr_el2
    stp x0, x1, [sp, #(32 * 8)]
    mov x0, sp
    bl {lower}
    ldp x0, x1, [sp, #(32 * 8)]
    msr elr_el2, x0
    msr spsr_el2, x1
    ldr x30, [sp, #(30 * 8)]
    ldp x28, x29, [sp, #(28 * 8)]
    ldp x26, x27, [sp, #(26 * 8)]
    ldp x24, x25, [sp, #(24 * 8)]
    ldp x22, x23, [sp, #(22 * 8)]
    ldp x20, x21, [sp, #(20 * 8)]
    ldp x18, x19, [sp, #(18 * 8)]
    ldp x16, x17, [sp, #(16 * 8)]
    ldp x14, x15, [sp, #(14 * 8)]
    ldp x12, x13, [sp, #(12 * 8)]
    ldp x10, x11, [sp, #(10 * 8)]
    ldp x8, x9, [sp, #(8 * 8)]
    ldp x6, x7, [sp, #(6 * 8)]
    ldp x4, x5, [sp, #(4 * 8)]
    ldp x2, x3, [sp, #(2 * 8)]
    ldp x0, x1, [sp, #(0 * 8)]
    add sp, sp, #(34 * 8)
    eret
"#,
    entry = sym irq_entry,
    fault = sym fault,
    lower = sym lower_sync,
);

/// Zet VBAR_EL2 op de tabel. De boot-stub doet dit vóór `kmain`; een
/// tweede core (het kooi-spoor) roept dit zelf aan.
#[cfg(all(target_arch = "aarch64", target_os = "none"))]
pub fn install() {
    // SAFETY: `__hopos_vectors` is de 2 KB-gealigneerde tabel hierboven;
    // VBAR_EL2 zetten raakt geen geheugen, en de ISB maakt hem geldig vóór
    // de volgende exception.
    unsafe {
        core::arch::asm!(
            "adrp {t}, __hopos_vectors",
            "add {t}, {t}, :lo12:__hopos_vectors",
            "msr vbar_el2, {t}",
            "isb",
            t = out(reg) _,
            options(nostack, preserves_flags),
        );
    }
}

/// Host-stub: er is geen vectortabel.
#[cfg(not(all(target_arch = "aarch64", target_os = "none")))]
pub fn install() {}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_door_takes_the_flag_once() {
        IRQ_FLAG.store(1, Relaxed);
        assert!(irq_pending());
        assert!(take_irq());
        assert!(!irq_pending());
        assert!(!take_irq());
    }

    #[test]
    fn frame_layout_matches_the_assembly() {
        assert_eq!(core::mem::offset_of!(TrapFrame, esr), 31 * 8);
        assert_eq!(core::mem::offset_of!(TrapFrame, elr), 32 * 8);
        assert_eq!(core::mem::offset_of!(TrapFrame, spsr), 33 * 8);
    }
}
