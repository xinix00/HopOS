//go:build rk3566

package main

import (
	_ "unsafe"

	"github.com/xinix00/HopOS/metal/board/rk3566"
)

// Geheugendeclaratie van de HOP-kern op de Radxa Zero 3E: hetzelfde venster als
// de agent (board/rk3566/plan.go) — 64MB vanaf 0x02200000. Alles daarbuiten (de
// plan-regio's op 0x06200000+, de pool op 0x07000000+ en de DTB die U-Boot
// bovenin legde) is voor HOP device-gemapt: ongecached, dus coherent met wat
// app-cores en de EL2-trampolines er lezen.

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = rk3566.RamBase

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = 0x04000000 // 64MB
