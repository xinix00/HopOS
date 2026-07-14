//go:build uefi

// board_uefi.go — de UEFI/ACPI-kant van de agent-main (Ampere Altra en de
// QEMU-proeftuin): dezelfde HOP-agent-bytes, met het uefi-board voor
// discovery (MADT/MCFG/GOP/PSCI) en de igb-NIC. De RAM-declaratie is hier
// eigendom van board/uefi: het venster wordt door de PE-stub gekozen en
// RamStart per variant door mkkernel -pe gepatcht.
package main

import (
	"time"
	_ "unsafe" // go:linkname (RAM-declaratie)

	"hop-os/metal/board/uefi"
	_ "hop-os/metal/board/uefi/hop" // registreert het board (init); de basis levert de tamago-hooks
)

// RAM-declaratie: RamStart wordt door mkkernel -pe per venster-variant
// gepatcht; de stub claimt GoRAMSize plus de plan-carve (board/uefi).
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = uefi.GoRAMSize

func init() {
	// Hardware-watchdog per default (dezelfde filosofie als de Pi's): de
	// SBSA-watchdog uit de ACPI GTDT — een hang cyclet zichzelf naar een
	// verse boot. QEMU virt heeft er geen; dan meldt Start dat en draait
	// de node zonder vangnet (de Altra heeft hem wél).
	uefi.WatchdogStart(12 * time.Second)

	// Node-identiteit: het NIC-MAC volgt zodra hopnet up is; tot die tijd
	// volstaat de main-default. (SMBIOS-serial: latere verfijning.)
}
