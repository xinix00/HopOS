//! De symbolen `memcpy`, `memcmp` en `bcmp` voor de kern op arm64; de
//! lussen staan in [`dev::mem`], met het waarom.
//!
//! Op het opslagpad was dit de rem (01-10, O6N): een lees van 1 MiB gaat in
//! de kern door vier kopieën die ongelijk uitgelijnd zijn (de datablok-kopie
//! naar `out`, `out` op stroompositie +12 de TxRing van leannet in, de
//! TxRing naar frame+54, en terug bij een schrijf), elk ~1 cyclus per byte
//! in de `memcpy` van `compiler_builtins`.
//!
//! Het snelle pad alleen waar de MMU van déze core aan staat en de
//! uitlijncontrole uit (SCTLR M = 1, A = 0, per aanroep gelezen): de stub
//! van de kern zet ze vóór `kmain` (cpu/src/boot.rs, board/uefi/src/el2.rs),
//! maar de EL2-switcher op een app-core draait met de MMU uit, en een kern
//! die op EL1 binnenkwam ook. De kern kopieert met `memcpy` alleen RAM
//! (Normal write-back) en het glas (Normal-NC): registers en vensters van
//! devices gaan via de vluchtige toegang van `dev`. Op Apple en de Radxa
//! mapt de kern de pool Device, behalve de staart van elk slot
//! (`glue::tail_rings`): een ringrecord kopieert hij met `memcpy` alleen
//! als die remap lukte; wat hij verder uit de pool kopieert, is niet
//! nagelopen.
//!
//! `#[inline(never)]`: anders plakt LTO de lus in elke aanroeper, en het
//! kernimage moet in zijn flipvenster passen.

use dev::mem::{cmp_fast, cmp_slow, copy_fast, copy_slow};

/// Staan de MMU van deze core aan en de uitlijncontrole uit?
#[inline(always)]
fn normal() -> bool {
    let sctlr: u64;
    // SAFETY: twee leesacties van systeemregisters zonder bijwerkingen; de
    // tweede op het EL waar we draaien (een SCTLR_EL2-lees op EL1 is
    // ongedefinieerd).
    unsafe {
        core::arch::asm!(
            "mrs {el}, CurrentEL",
            "ubfx {el}, {el}, #2, #2",
            "cmp {el}, #2",
            "b.ne 1f",
            "mrs {s}, sctlr_el2",
            "b 2f",
            "1: mrs {s}, sctlr_el1",
            "2:",
            el = out(reg) _,
            s = out(reg) sctlr,
            options(nomem, nostack),
        );
    }
    sctlr & 0b11 == 0b01
}

/// `memcpy(3)`.
///
/// # Safety
///
/// Het contract van `memcpy`: twee geldige, niet-overlappende bereiken.
#[unsafe(no_mangle)]
#[inline(never)]
pub(crate) unsafe extern "C" fn memcpy(dst: *mut u8, src: *const u8, n: usize) -> *mut u8 {
    // SAFETY: het contract van `memcpy`; het snelle pad alleen op Normal
    // geheugen met de uitlijncontrole uit, en vanaf 16 bytes.
    unsafe {
        if n >= 16 && normal() {
            copy_fast(dst, src, n);
        } else {
            copy_slow(dst, src, n);
        }
    }
    dst
}

/// `memcmp(3)`.
///
/// # Safety
///
/// Het contract van `memcmp`: twee geldige bereiken van `n` bytes.
#[unsafe(no_mangle)]
#[inline(never)]
pub(crate) unsafe extern "C" fn memcmp(a: *const u8, b: *const u8, n: usize) -> i32 {
    // SAFETY: het contract van `memcmp`; het snelle pad als bij `memcpy`.
    unsafe {
        if n >= 8 && normal() {
            cmp_fast(a, b, n)
        } else {
            cmp_slow(a, b, n)
        }
    }
}

/// `bcmp(3)`: LLVM maakt er een van een `memcmp` die alleen op nul toetst.
///
/// # Safety
///
/// Als [`memcmp`].
#[unsafe(no_mangle)]
pub(crate) unsafe extern "C" fn bcmp(a: *const u8, b: *const u8, n: usize) -> i32 {
    // SAFETY: als `memcmp`.
    unsafe { memcmp(a, b, n) }
}
