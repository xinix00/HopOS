package rpi5

import "unsafe"

// mmio32 lees/schrijf: zelfde patroon als board/qemuvirt (tamago's reg-pakket
// is internal en dus niet importeerbaar).

func mmioRead32(addr uintptr) uint32 {
	return *(*uint32)(unsafe.Pointer(addr))
}

func mmioWrite32(addr uintptr, val uint32) {
	*(*uint32)(unsafe.Pointer(addr)) = val
}

// printk stuurt één byte naar de debug-PL011 (poll op TX-FIFO-full). De
// firmware heeft de UART al geconfigureerd (uart_2ndstage=1); wij raken
// alleen DR en FR aan.
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	for mmioRead32(uartFR)&frTXFF != 0 {
	}
	mmioWrite32(uartDR, uint32(c))
}
