/*
 * Het image van HopOS op QEMU virt (en elk board met RAM op 0x4000_0000).
 *
 * QEMU laadt een ELF op zijn fysieke adressen en springt naar de entry.
 * De DTB legt hij op de RAM-basis (0x4000_0000) zolang het image daar niet
 * begint (hw/arm/boot.c), met het begin van het image als grens: daarom
 * begint de tekst op 0x4020_0000 en heeft de DTB 2 MB. De Go-kern linkte
 * op 0x4001_0000 (-T) en liet de DTB zo 64 KB.
 *
 * De kern-RAM loopt tot 0x4f00_0000 (layout.HopRAMStart + HopRAMSize);
 * daarboven begint de DMA-regio. De heap is alles tussen het einde van de
 * stack en die grens.
 *
 * Het contract met cpu::boot: __stack_top, __bss_start, __bss_end (8-
 * gealigneerd); met board-qemuvirt: __heap_start, __heap_end.
 */

ENTRY(_start)

/*
 * KERN_END staat als afstand tot KERN_BASE, niet als eigen adres: de
 * flip-bundel (image/flip-bundle.sh) linkt de kern een tweede keer op een
 * schaduwadres (HOPOS_LINK_BASE, zie build.rs) en leest de relocatietabel
 * uit het verschil. Een vast __heap_end zou dan in de ADRP van de code een
 * verschil geven dat geen 8-byte-woord is, en de diff terecht laten falen.
 */
KERN_BASE = 0x40200000;
KERN_END = KERN_BASE + 0x0ee00000;
STACK_SIZE = 0x40000;

SECTIONS
{
  . = KERN_BASE;

  .text : ALIGN(4096) {
    KEEP(*(.text.boot))
    KEEP(*(.text.vectors))
    *(.text .text.*)
  }

  .rodata : ALIGN(4096) {
    KEEP(*(.rodata.pagetables))
    *(.rodata .rodata.*)
  }

  .data : ALIGN(4096) {
    *(.data .data.*)
  }

  .bss (NOLOAD) : ALIGN(4096) {
    __bss_start = .;
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
  }
}

ASSERT(__heap_start < __heap_end, "het image loopt de DMA-regio in")
