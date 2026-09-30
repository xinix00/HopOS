/*
 * Het image van HopOS voor UEFI (board-uefi): een PIE op basis 0, dat
 * `rust-objcopy -O binary` tot een PE32+ maakt (image/uefi-run.sh). De
 * PE-header staat in de eerste pagina (board/uefi/src/entry.rs, sectie
 * .text.efihdr); RVA == bestandsoffset == linkadres, want de basis is 0.
 *
 * Twee PE-secties met echte rechten (EDK2's image-protection mapt code
 * read-only; Go-les 13-07):
 *
 *   .text  RX  de code en de rodata, vanaf 0x1000;
 *   .data  RW  data, GOT, de dynamische tabellen en de relocaties, en via
 *              VirtualSize de BSS en de boot-stack (de loader vult nullen).
 *
 * De relocaties (alleen R_AARCH64_RELATIVE, want alles is gebonden) past
 * _start_efi zelf toe: basis + addend. Ze mogen alleen in .data vallen;
 * image/uefi-run.sh weigert een image met een relocatie in .text.
 *
 * Het contract met board-uefi: __efi_head (de basis), __rela_start/end,
 * __bss_start/end, __stack_top, en de absolute maten voor de header. Met
 * cpu::boot: __stack_top, __stack_guard, __bss_start/end, en __boot_ttbr0 (alleen voor de
 * virt-ingang _start, die hier niet gebruikt wordt: een lege tabel).
 */

ENTRY(_start_efi)

STACK_SIZE = 0x40000;

SECTIONS
{
  . = 0;
  .efihdr : {
    KEEP(*(.text.efihdr))
  }

  . = ALIGN(4096);
  __text_start = .;
  .text : {
    KEEP(*(.text.efientry))
    KEEP(*(.text.boot))
    KEEP(*(.text.vectors))
    *(.text .text.*)
  }
  .rodata : ALIGN(16) {
    *(.rodata .rodata.*)
  }
  . = ALIGN(4096);
  __text_end = .;

  __data_start = .;
  .data.rel.ro : { *(.data.rel.ro .data.rel.ro.*) }
  .got : { *(.got .got.*) }
  .dynamic : { *(.dynamic) }
  .dynsym : { *(.dynsym) }
  .dynstr : { *(.dynstr) }
  .hash : { *(.hash) }
  .gnu.hash : { *(.gnu.hash) }
  .data : ALIGN(16) {
    *(.data .data.*)
  }
  .rela.dyn : ALIGN(8) {
    __rela_start = .;
    *(.rela .rela.*)
    __rela_end = .;
  }
  . = ALIGN(4096);
  __data_file_end = .;

  .bss (NOLOAD) : ALIGN(4096) {
    __bss_start = .;
    *(.bss .bss.*)
    *(COMMON)
    . = ALIGN(4096);
    __boot_ttbr0 = .;
    . += 4096;
    . = ALIGN(8);
    __bss_end = .;
  }

  /* De wachtpagina onder de stack (30-09): het board maakt hem ongeldig in
   * de map, zodat een overloop een luide fault is en niet stil het einde
   * van .bss overschrijft. Hij komt bovenop STACK_SIZE. */
  .stack (NOLOAD) : ALIGN(4096) {
    __stack_guard = .;
    . += 4096;
    . += STACK_SIZE;
    __stack_top = .;
  }
  . = ALIGN(4096);
  __image_end = .;

  /DISCARD/ : {
    *(.comment)
    *(.note .note.*)
    *(.eh_frame .eh_frame_hdr)
    *(.interp)
  }
}

/* De maten voor de PE-header, absoluut: de header leest ze met ABS32. */
__efi_text_rva = ABSOLUTE(__text_start);
__efi_text_size = ABSOLUTE(__text_end - __text_start);
__efi_data_rva = ABSOLUTE(__data_start);
__efi_data_raw = ABSOLUTE(__data_file_end - __data_start);
__efi_data_vsize = ABSOLUTE(__image_end - __data_start);
__efi_bss_size = ABSOLUTE(__image_end - __data_file_end);
__efi_image_size = ABSOLUTE(__image_end);
__efi_entry_rva = ABSOLUTE(_start_efi);

ASSERT(__text_start == 0x1000, "de header moet precies de eerste pagina zijn")
ASSERT(__rela_end >= __rela_start, "geen relocatietabel")
