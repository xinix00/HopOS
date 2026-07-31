//go:build linkramsize

package licheerv

import (
	_ "unsafe"
)

// Het geheugenplan van een APP-SLOT op het app-hart (C906L). Bouwen met
// -tags linkramsize; mem.go (het HOP-plan) valt dan weg.
//
//	0x80000000  HOP: kern + heap, 128MB          (mem.go)
//	0x88000000  app-partitie, 64MB               ← dit bestand
//	0x8C000000  vrij
//	0x8FF10000  control page / mailbox (granted MMIO-achtig venster)
//
// De kooi (slot/stub-slot.S) geeft precies deze partitie + de control page
// + UART0 vrij en verzegelt de rest — inclusief HOP's eigen 128MB.
// SlotBase/SlotSize staan in c906l.go: HOP kent ze óók (hij plaatst het
// blob en geeft de partitie in de kooi vrij).
const SlotStack = 0x100

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint64 = SlotBase

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint64 = SlotSize

//go:linkname ramStackOffset runtime/goos.RamStackOffset
var ramStackOffset uint64 = SlotStack
