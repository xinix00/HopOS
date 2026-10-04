//! De symbolen `memcpy`, `memcmp` en `bcmp` voor de kern op arm64; de
//! lussen en de symbolen staan in [`dev::mem`] ([`dev::mem_symbols`]), met
//! het waarom.
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
//! (`kooi::tail_rings`): een ringrecord kopieert hij met `memcpy` alleen
//! als die remap lukte; wat hij verder uit de pool kopieert, is niet
//! nagelopen.

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

dev::mem_symbols!(normal);
