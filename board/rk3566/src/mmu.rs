//! De identity map van de RK3566, als data in het image.
//!
//! De boot-stub (`cpu::boot`) zet TTBR0_EL2 op `__boot_ttbr0` en doet de
//! MMU aan; welk bereik Device en welk Normal is, weet alleen het board.
//! De tabellen staan vast in `.rodata`.
//!
//! De keuze is die van de Go-kern op dit bord (tamago: Normal alleen binnen
//! de eigen RAM-declaratie, al het overige Device-nGnRnE), met de
//! DMA-regio's als Normal-NC zoals `memattr` ze in Go omzette. GEMETEN
//! 20-09: de NIC-DMA van Device naar Normal-NC bracht de inbound van 15,5
//! naar 56,6 MB/s (Device-geheugen is duur om te LEZEN: elke load een
//! aparte, strikt geordende transactie, en de ontvangstkant kopieert elk
//! frame eruit). Device heeft een tweede voordeel: de CPU speculeert er
//! nooit in, dus een gat in het DRAM (een bord van 1 GB onder een map van 4
//! GB) kan geen SError geven.
//!
//! - 0x0000_0000 tot 0x0020_0000: TF-A (bl31). Ongemapt: wij blijven eraf,
//!   en een niet-beveiligde toegang daar faultt.
//! - 0x0020_0000 tot 0x0220_0000: de firmware-keten (U-Boot). Device.
//! - 0x0220_0000 tot 0x0620_0000: de kern-RAM, Normal WB (64 MB, zoals Go).
//! - 0x0620_0000 tot 0x0640_0000: de structuren van de kern (control-pages,
//!   kooien, boot-scratch, levenstekens, vluchtrecorder): Device, coherent
//!   met een core die met de MMU uit binnenkomt.
//! - 0x0640_0000 tot 0x0780_0000: NIC-DMA, USB-DMA en de framebuffer:
//!   Normal-NC.
//! - 0x0780_0000 tot 0x0880_0000: het staging-venster van de kern-flip.
//!   Device.
//! - 0x0880_0000 tot 0x1_0000_0000: de pool, de DTB en de initrd van U-Boot
//!   en de MMIO van de SoC (vanaf 0xF000_0000): Device.

#[cfg(all(target_arch = "aarch64", target_os = "none"))]
core::arch::global_asm!(
    r#"
    .section .rodata.pagetables, "a"
    .balign 4096
    .global __boot_ttbr0
__boot_ttbr0:
    .quad __boot_l2_lo + 3
    .quad {dev1}
    .quad {dev2}
    .quad {dev3}
    .fill 508, 8, 0

    .balign 4096
__boot_l2_lo:
    .quad 0
    .set blk, 1
    .rept {fw_blocks}
    .quad {dev0} + (blk * 0x200000)
    .set blk, blk + 1
    .endr
    .rept {kern_blocks}
    .quad {ram0} + (blk * 0x200000)
    .set blk, blk + 1
    .endr
    .rept {struct_blocks}
    .quad {dev0} + (blk * 0x200000)
    .set blk, blk + 1
    .endr
    .rept {nc_blocks}
    .quad {nc0} + (blk * 0x200000)
    .set blk, blk + 1
    .endr
    .rept {pool_blocks}
    .quad {dev0} + (blk * 0x200000)
    .set blk, blk + 1
    .endr
"#,
    dev0 = const cpu::boot::block(0, cpu::boot::ATTR_DEVICE),
    ram0 = const cpu::boot::block(0, cpu::boot::ATTR_NORMAL),
    nc0 = const cpu::boot::block(0, cpu::boot::ATTR_NORMAL_NC),
    dev1 = const cpu::boot::block(0x4000_0000, cpu::boot::ATTR_DEVICE),
    dev2 = const cpu::boot::block(0x8000_0000, cpu::boot::ATTR_DEVICE),
    dev3 = const cpu::boot::block(0xC000_0000, cpu::boot::ATTR_DEVICE),
    fw_blocks = const FW_BLOCKS,
    kern_blocks = const KERN_BLOCKS,
    struct_blocks = const STRUCT_BLOCKS,
    nc_blocks = const NC_BLOCKS,
    pool_blocks = const POOL_BLOCKS,
);

/// Eén blok van niveau 2.
const MB2: u64 = 0x20_0000;
/// De firmware-blokken na blok 0 (TF-A, ongemapt).
const FW_BLOCKS: u64 = (crate::KERN_RAM.base.0 - crate::DRAM_BASE) / MB2;
/// De kern-RAM.
const KERN_BLOCKS: u64 = crate::KERN_RAM.size / MB2;
/// Het structurenvenster.
const STRUCT_BLOCKS: u64 = crate::STRUCT_WINDOW.size / MB2;
/// De ongecachete DMA-regio's.
const NC_BLOCKS: u64 = crate::DMA.size / MB2;
/// De rest van de eerste gigabyte.
const POOL_BLOCKS: u64 = 512 - 1 - FW_BLOCKS - KERN_BLOCKS - STRUCT_BLOCKS - NC_BLOCKS;

// De `.rept`-tellingen moeten op het plan passen: aaneengesloten, 2
// MB-gealigneerd, en samen precies de eerste gigabyte.
const _: () = {
    use crate::{DMA, DRAM_BASE, KERN_RAM, STAGE_WINDOW, STRUCT_WINDOW};
    assert!(DRAM_BASE == MB2);
    assert!(KERN_RAM.base.0.is_multiple_of(MB2) && KERN_RAM.size.is_multiple_of(MB2));
    assert!(KERN_RAM.base.0 + KERN_RAM.size == STRUCT_WINDOW.base.0);
    assert!(STRUCT_WINDOW.base.0 + STRUCT_WINDOW.size == DMA.base.0);
    assert!(DMA.base.0 + DMA.size == STAGE_WINDOW.base.0);
    assert!(1 + FW_BLOCKS + KERN_BLOCKS + STRUCT_BLOCKS + NC_BLOCKS + POOL_BLOCKS == 512);
    assert!(POOL_BLOCKS > 0);
};

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_map_matches_the_plan() {
        assert_eq!(FW_BLOCKS, 16);
        assert_eq!(KERN_BLOCKS, 32);
        assert_eq!(STRUCT_BLOCKS, 1);
        assert_eq!(NC_BLOCKS, 10);
        assert_eq!(POOL_BLOCKS, 452);
    }
}
