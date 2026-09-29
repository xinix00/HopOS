/*
 * Het image van HopOS op de Raspberry Pi 4 en 5 (board/raspi).
 *
 * De firmware laadt kernel8.img (Pi 4) of hop-agent5.img (Pi 5) RAUW op
 * 0x80000 en springt naar de eerste byte: de Pi 5-EEPROM negeert
 * kernel_address (gemeten 09-07), de Pi 4 laadt daar per default. Geen
 * arm64-Image-header: met de ARM\x64-magic weigerde de Pi 5-firmware de
 * kern zonder levensteken (boot-meting 08-07), het raw-pad is bewezen.
 * Daarom staat de ingang van het board (.text.pi_head, board_raspi::
 * pi_entry!) vooraan: die doet de Pi-voorbereiding en springt naar _start.
 *
 * De kern-RAM loopt tot 0x0800_0000 (board_raspi::map::KERN_END); daarboven
 * het laadvenster met de DTB en de staging. De pagetabellen staan in .data
 * (.data.pagetables): discover zet er het DRAM uit de DTB bij.
 *
 * Het contract met cpu::boot: __stack_top, __bss_start, __bss_end (8-
 * gealigneerd); met board-raspi: __heap_start, __heap_end, __boot_ttbr0.
 */

ENTRY(_pi_start)

KERN_BASE = 0x80000;
KERN_END = KERN_BASE + 0x07f80000;
STACK_SIZE = 0x40000;

SECTIONS
{
  . = KERN_BASE;

  .text : {
    KEEP(*(.text.pi_head))
    KEEP(*(.text.boot))
    KEEP(*(.text.vectors))
    *(.text .text.*)
  }

  .rodata : ALIGN(4096) {
    *(.rodata .rodata.*)
  }

  .data : ALIGN(4096) {
    KEEP(*(.data.pagetables))
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

ASSERT(_pi_start == KERN_BASE, "de Pi-ingang staat niet vooraan in het image")
ASSERT(__heap_start < __heap_end, "het image loopt het laadvenster in")
