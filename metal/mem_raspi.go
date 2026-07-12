//go:build rpi4 || rpi5

package main

import (
	_ "unsafe"

	"hop-os/metal/board/raspi"
)

// Geheugendeclaratie van de HOP-kern op de Raspberry Pi 4 én 5: 128MB vanaf
// het laadadres 0x80000 (Pi-default; de Pi 5-EEPROM negeert kernel_address en
// laadt daar sowieso, de Pi 4 volgt met kernel_address=0x80000 — link -T
// 0x90000). Beide boards zijn hierin identiek. Alles daarbuiten — de
// plan-regio's (0x10000000+), de pool (0x20000000+) en de DTB (0xF000000) —
// is voor HOP device-gemapt: ongecached, dus coherent met wat app-cores en de
// EL2-trampolines er lezen (plus dev.CleanInv waar een app het cacheable
// raakt).

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = raspi.HopKernelStart

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = raspi.HopKernelSize // 128MB
