/*
 * Het image van HopOS op riscv64, in machine mode.
 *
 * QEMU virt met `-bios none` laadt een ELF op zijn fysieke adressen en laat
 * ELK hart op 0x8000_0000 beginnen, met a0 = hart-id en a1 = de DTB (die
 * legt hij bovenin het RAM, dus niet onder het image zoals op arm64). Op de
 * LicheeRV laadt de vendor-FSBL het image als MONITOR-slot van de FIP op
 * 0x8400_0000 (build.rs zet KERN_BASE en KERN_SIZE per board): daaronder
 * draait de FSBL zelf nog terwijl hij U-Boot laadt (gemeten 19-08).
 *
 * Het contract met cpu::riscv::boot: __stack_top, __bss_start, __bss_end (8-
 * gealigneerd), __hopos_trap; met het board: __heap_start, __heap_end (het
 * begin van de DMA-regio).
 *
 * RISC-V kent de kleine secties (.sdata, .sbss, .srodata) naast de gewone;
 * ze horen in dezelfde vakken, anders wist de stub een deel van de BSS
 * niet. Geen __global_pointer$: zonder dat symbool relaxeert de linker niets
 * naar gp, en gp hoeft dan nergens gezet.
 */

ENTRY(_start)

KERN_BASE = 0x80000000;
KERN_SIZE = 0x0f000000;
KERN_END = KERN_BASE + KERN_SIZE;
STACK_SIZE = 0x40000;

SECTIONS
{
  . = KERN_BASE;

  .text : ALIGN(4096) {
    KEEP(*(.text.boot))
    KEEP(*(.text.trap))
    *(.text .text.*)
  }

  .rodata : ALIGN(4096) {
    *(.rodata .rodata.*)
    *(.srodata .srodata.*)
  }

  .data : ALIGN(4096) {
    *(.data .data.*)
    *(.sdata .sdata.*)
  }

  .bss (NOLOAD) : ALIGN(4096) {
    __bss_start = .;
    *(.sbss .sbss.*)
    *(.bss .bss.*)
    *(COMMON)
    . = ALIGN(8);
    __bss_end = .;
  }

  .stack (NOLOAD) : ALIGN(4096) {
    . += STACK_SIZE;
    __stack_top = .;
  }

  . = ALIGN(4096);
  __heap_start = .;
  __heap_end = KERN_END;

  /DISCARD/ : {
    *(.comment)
    *(.note .note.*)
    *(.eh_frame .eh_frame_hdr)
    *(.riscv.attributes)
  }
}

ASSERT(__heap_start < __heap_end, "het image loopt de DMA-regio in")
