//! De identity map van QEMU virt, als data in het image.
//!
//! De boot-stub (`cpu::boot`) zet TTBR0_EL2 op `__boot_ttbr0` en doet de
//! MMU aan; welk bereik Device en welk Normal is, weet alleen het board.
//! Geen code die tabellen bouwt: de tabellen staan vast in `.rodata`, dus
//! er is niets dat met de MMU uit fout kan gaan.
//!
//! - 0x0000_0000 tot 0x4000_0000: Device-nGnRnE (GIC, UART, virtio-mmio,
//!   PCIe-ECAM op 0x3f00_0000).
//! - 0x4000_0000 tot 0x4f00_0000: de kern-RAM, Normal WB, in blokken van
//!   2 MB.
//! - 0x4f00_0000 tot 0x5000_0000: de DMA-regio, Normal non-cacheable: een
//!   controller leest er zonder cache-onderhoud (de Go-kern: "buiten de
//!   RAM-declaratie, dus niet gecached").
//! - 0x5000_0000 tot 0x1_0000_0000: de rest van de RAM (`-m 3G`), Normal
//!   WB; de pool van de slots.

#[cfg(all(target_arch = "aarch64", target_os = "none"))]
core::arch::global_asm!(
    r#"
    .section .rodata.pagetables, "a"
    .balign 4096
    .global __boot_ttbr0
__boot_ttbr0:
    .quad {dev0}
    .quad __boot_l2_ram + 3
    .quad {ram2}
    .quad {ram3}
    .fill 508, 8, 0

    .balign 4096
__boot_l2_ram:
    .set blk, 0
    .rept 120
    .quad {ram1} + (blk * 0x200000)
    .set blk, blk + 1
    .endr
    .rept 8
    .quad {dma} + ((blk - 120) * 0x200000)
    .set blk, blk + 1
    .endr
    .rept 384
    .quad {ram1} + (blk * 0x200000)
    .set blk, blk + 1
    .endr
"#,
    dev0 = const cpu::boot::block(0, cpu::boot::ATTR_DEVICE),
    ram1 = const cpu::boot::block(0x4000_0000, cpu::boot::ATTR_NORMAL),
    ram2 = const cpu::boot::block(0x8000_0000, cpu::boot::ATTR_NORMAL),
    ram3 = const cpu::boot::block(0xc000_0000, cpu::boot::ATTR_NORMAL),
    dma = const cpu::boot::block(0x4f00_0000, cpu::boot::ATTR_NORMAL_NC),
);

#[cfg(test)]
mod tests {
    use crate::{DMA, KERN_RAM};

    /// De `.rept`-tellingen hierboven moeten bij het plan passen.
    #[test]
    fn the_map_matches_the_plan() {
        const MB2: u64 = 0x20_0000;
        assert_eq!(KERN_RAM.size / MB2, 120);
        assert_eq!(DMA.size / MB2, 8);
        assert_eq!(KERN_RAM.end(), DMA.base);
        assert_eq!(120 + 8 + 384, 512);
    }
}
