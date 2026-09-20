//go:build uefi || o6n

package main

import (
	"fmt"

	"github.com/usbarmory/tamago/arm64"

	"github.com/xinix00/HopOS/metal/v2/board/uefi"
)

// Een EL1-exception op HOP's core mét adres: ESR/FAR/PC vóór tamago's panic,
// zoals board_apple.go dat doet. Zonder dit is een data-abort in een driver
// "EL1 exception" en niets meer.
func init() {
	arm64.SystemExceptionHandler = func(pc uintptr) {
		fmt.Printf("!!EL1 ESR=%#016x FAR=%#016x PC=%#x\n", uefi.ReadESR(), uefi.ReadFAR(), pc)
		arm64.DefaultExceptionHandler(pc)
	}
}
