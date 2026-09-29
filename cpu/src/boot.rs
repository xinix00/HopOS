//! De boot-stub: van de eerste instructie tot `kmain`.
//!
//! `_start` doet precies wat Rust nog niet kan: alleen core 0 gaat door,
//! de stack komt op zijn plek, BSS wordt gewist, en op EL2 gaan de vectoren
//! en de MMU aan. Daarna `kmain(dtb, el)`, dat de binary levert.
//!
//! HopOS eist EL2 (de stage-2-kooi is een invariant, geen optie). Wie ons
//! op een ander niveau aflevert, krijgt toch een stack en BSS maar géén
//! MMU (dat zijn andere registers), en `kmain` meldt de reden op de UART en
//! parkeert: stil parkeren vóór de console is een fout die niemand vindt.
//! Zo deed de Go-kern het ook (`board.RequireEL2` in de main).
//!
//! Het linkcontract (de binary levert het, `hopos/link.ld`):
//!
//! - `__stack_top`: de top van de boot-stack, 16-gealigneerd;
//! - `__bss_start`, `__bss_end`: 8-gealigneerd;
//! - `__boot_ttbr0`: de niveau-1-tabel van de identity map (4 KB-korrel,
//!   39-bit VA). Het board levert hem (`global_asm!`), want welk bereik
//!   Device en welk Normal is, is board-kennis;
//! - `kmain`: `extern "C" fn(dtb: u64, el: u64) -> !`.
//!
//! De MMU-attributen staan hier vast, zodat elk board dezelfde indexen
//! gebruikt: [`ATTR_DEVICE`], [`ATTR_NORMAL`], [`ATTR_NORMAL_NC`].

/// MAIR-index 0: Device-nGnRnE (MMIO).
pub const ATTR_DEVICE: u64 = 0;
/// MAIR-index 1: Normal, write-back, read/write-allocate (de kern-RAM).
pub const ATTR_NORMAL: u64 = 1;
/// MAIR-index 2: Normal, non-cacheable (DMA-regio's zonder cache-onderhoud).
pub const ATTR_NORMAL_NC: u64 = 2;

/// MAIR_EL2: index 0 = 0x00 (Device-nGnRnE), 1 = 0xff (Normal WB RWA),
/// 2 = 0x44 (Normal NC).
pub const MAIR: u64 =
    (0x00 << (8 * ATTR_DEVICE)) | (0xff << (8 * ATTR_NORMAL)) | (0x44 << (8 * ATTR_NORMAL_NC));

/// Een blokbeschrijving (niveau 1: 1 GB, niveau 2: 2 MB) voor `pa` met
/// MAIR-index `attr`: AF gezet, inner shareable voor Normal, en XN voor
/// alles wat geen gecachte kern-RAM is (code draait alleen uit de kern).
#[must_use]
pub const fn block(pa: u64, attr: u64) -> u64 {
    const VALID_BLOCK: u64 = 0b01;
    const AF: u64 = 1 << 10;
    const SH_INNER: u64 = 0b11 << 8;
    const XN: u64 = 1 << 54;
    let mut d = pa | VALID_BLOCK | (attr << 2) | AF;
    if attr != ATTR_DEVICE {
        d |= SH_INNER;
    }
    if attr != ATTR_NORMAL {
        d |= XN;
    }
    d
}

/// Het merkteken van een kern-flip in x3 bij de ingang ("HOPFLIPE"
/// little-endian): de trampoline van `cpu::el2::chain` zet het, geen
/// firmware doet dat (het Linux-bootprotocol eist x1 tot en met x3 nul, en
/// de Pi-firmware, U-Boot en QEMU houden zich daaraan).
///
/// Waarom: `_start` laat bij een koude boot alleen de core met affiniteit 0
/// door; een firmware die alle cores op de ingang loslaat, krijgt zo één
/// kern en een parkeerlus. Een geflipte kern springt vanaf de OS-core, en
/// die is niet per se core 0 (`hopos.oscore`, PORT.md beslissing 2): zonder
/// dit merkteken parkeerde de nieuwe kern zich zonder één regel (29-09,
/// gezien in de ingang, niet op ijzer). Met het merkteken mag elke core
/// door; er komt er ook maar één, want de app-cores draaien in de
/// switch-code in de plan-regio en raken het beeld nooit.
pub const FLIP_ENTRY: u64 = 0x4550_494C_4650_4F48;

/// De poort van `_start`, als Rust: gaat een core met MPIDR-affiniteit
/// `aff` en `x3` bij de ingang door naar `kmain`? De assembly hieronder is
/// dezelfde beslissing in vier instructies; deze vorm is er voor de
/// host-tests.
#[must_use]
pub const fn admits(aff: u64, x3: u64) -> bool {
    aff & 0xff_ffff == 0 || x3 == FLIP_ENTRY
}

/// Parkeert deze core voor altijd: een WFE-lus, zodat hij niets verbruikt
/// en niets meer aanraakt.
pub fn park() -> ! {
    loop {
        arch::wfe();
    }
}

#[cfg(all(target_arch = "aarch64", target_os = "none"))]
mod arch {
    /// Eén WFE.
    #[inline]
    pub(super) fn wfe() {
        // SAFETY: WFE wacht op een event; geen geheugeneffect.
        unsafe { core::arch::asm!("wfe", options(nomem, nostack, preserves_flags)) }
    }
}

#[cfg(not(all(target_arch = "aarch64", target_os = "none")))]
mod arch {
    /// Host-stub.
    #[inline]
    pub(super) fn wfe() {
        core::hint::spin_loop();
    }
}

// De stub. Registers: x19 = MPIDR-affiniteit, x20 = DTB (x0 van de
// firmware), x21 = het EL. x3 = [`FLIP_ENTRY`] laat elke core door
// ([`admits`]): een geflipte kern komt op de OS-core binnen.
//
// TCR_EL2 (niet-VHE): T0SZ = 25 (39-bit VA, start op niveau 1), IRGN0 en
// ORGN0 = WB-WA, SH0 = inner, TG0 = 4 KB, PS uit ID_AA64MMFR0_EL1.PARange,
// en de RES1-bits 23 en 31.
//
// SCTLR_EL2 (niet-VHE): de RES1-set 0x30c50830, plus M, C, I en SA; A, WXN
// en EE uit (geen uitlijncontrole, little-endian).
//
// HCR_EL2: RW (een lagere EL is AArch64), en IMO, FMO en AMO: fysieke
// IRQ, FIQ en SError komen naar EL2. Zonder IMO is een IRQ op EL2
// "gericht op EL1" en dus nooit genomen.
#[cfg(all(target_arch = "aarch64", target_os = "none"))]
core::arch::global_asm!(
    r#"
    .section .text.boot, "ax"
    .global _start
_start:
    mrs x19, mpidr_el1
    and x19, x19, #0xffffff
    ldr x9, ={flip}
    cmp x3, x9
    b.eq 8f
    cbnz x19, 9f
8:
    mov x20, x0
    mrs x21, CurrentEL
    lsr x21, x21, #2
    and x21, x21, #3

    adrp x1, __stack_top
    add x1, x1, :lo12:__stack_top
    mov sp, x1

    adrp x1, __bss_start
    add x1, x1, :lo12:__bss_start
    adrp x2, __bss_end
    add x2, x2, :lo12:__bss_end
1:  cmp x1, x2
    b.hs 2f
    str xzr, [x1], #8
    b 1b
2:
    cmp x21, #2
    b.ne 3f

    adrp x1, __hopos_vectors
    add x1, x1, :lo12:__hopos_vectors
    msr vbar_el2, x1

    mov x1, #(1 << 31)
    orr x1, x1, #(7 << 3)
    msr hcr_el2, x1

    ldr x1, ={mair}
    msr mair_el2, x1
    mrs x2, id_aa64mmfr0_el1
    and x2, x2, #7
    cmp x2, #5
    mov x3, #5
    csel x2, x2, x3, ls
    ldr x1, =((1 << 31) | (1 << 23) | (3 << 12) | (1 << 10) | (1 << 8) | 25)
    orr x1, x1, x2, lsl #16
    msr tcr_el2, x1
    adrp x1, __boot_ttbr0
    add x1, x1, :lo12:__boot_ttbr0
    msr ttbr0_el2, x1
    dsb ish
    isb
    tlbi alle2
    dsb ish
    isb
    ldr x1, =(0x30c50830 | (1 << 0) | (1 << 2) | (1 << 3) | (1 << 12))
    msr sctlr_el2, x1
    isb

3:
    mov x0, x20
    mov x1, x21
    bl kmain
9:
    wfe
    b 9b
    .ltorg
"#,
    mair = const MAIR,
    flip = const FLIP_ENTRY,
);

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn block_descriptors_carry_the_right_bits() {
        // Device: geldig blok, AF, XN, geen shareability.
        assert_eq!(block(0, ATTR_DEVICE), 0x0040_0000_0000_0401);
        // Kern-RAM: index 1, inner shareable, uitvoerbaar.
        assert_eq!(block(0x4000_0000, ATTR_NORMAL), 0x4000_0705);
        // DMA: index 2, inner shareable, XN.
        assert_eq!(block(0x4f00_0000, ATTR_NORMAL_NC), 0x0040_0000_4f00_0709);
    }

    #[test]
    fn only_core_zero_boots_cold_and_any_core_lands_a_flip() {
        // Koud: de core met affiniteit 0, en alleen die.
        assert!(admits(0, 0));
        assert!(!admits(1, 0), "a second core must park on a cold boot");
        assert!(!admits(0x100, 0), "Pi 5: core 1 is aff1 = 1");
        // Een flip vanaf de OS-core: core 2 op virt, core 1 op de Pi 5.
        assert!(admits(2, FLIP_ENTRY));
        assert!(admits(0x100, FLIP_ENTRY));
        // Alleen het volle merkteken; rommel in x3 is geen flip.
        assert!(!admits(3, FLIP_ENTRY ^ 1));
        assert!(!admits(3, u64::from(FLIP_ENTRY as u32)));
        // De bovenste bits van MPIDR (U, MT, RES1) tellen niet mee.
        assert!(admits(0x8000_0000, 0));
        assert_eq!(&FLIP_ENTRY.to_le_bytes(), b"HOPFLIPE");
    }

    #[test]
    fn mair_indexes_match_the_constants() {
        assert_eq!((MAIR >> (8 * ATTR_DEVICE)) & 0xff, 0x00);
        assert_eq!((MAIR >> (8 * ATTR_NORMAL)) & 0xff, 0xff);
        assert_eq!((MAIR >> (8 * ATTR_NORMAL_NC)) & 0xff, 0x44);
    }
}
