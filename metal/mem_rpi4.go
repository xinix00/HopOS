//go:build rpi4

package main

import _ "unsafe"

// Geheugendeclaratie van de HOP-kern op de Pi 4: 128MB vanaf het laadadres
// 0x80000 (Pi-default; link -T 0x90000 — géén kernel_address, gelijk aan de
// Pi 5). Alles daarbuiten — de plan-regio's (0x10000000+), de pool
// (0x20000000+) en de DTB (0xF000000) — is voor HOP device-gemapt: ongecached,
// dus coherent met wat app-cores en de EL2-trampolines er lezen (plus
// dev.CleanInv waar een app het cacheable raakt).

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = 0x80000

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = 0x8000000 // 128MB
