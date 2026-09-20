//go:build o6n

package uefi

// earlyUART op de Orion O6/O6N: UART2, de 3-pins debug-header (PL011, door
// de firmware al op 115200 gezet — haar eigen console). Link-time constante:
// de asm (init.s earlyMark) leest hem met MMU uit, vóór de runtime; hwinit1
// spiegelt printk ernaartoe (de SPCR wijst naar UART0/3, die de SCP dicht
// houdt).
var earlyUART uintptr = 0x040d0000

// EarlyUARTOEM: de vroege UART hoort bij déze firmware (XSDT-OEMID). Draait
// dezelfde o6n-smaak op een ander bord (de Ampere, 19-09), dan is 0x040d0000
// een adres van niets en moet hwinit1 gewoon de SPCR-console nemen — anders
// print HopOS in het luchtledige en zie je op de header-UART alleen de stub.
var EarlyUARTOEM = "CIXTEK"
