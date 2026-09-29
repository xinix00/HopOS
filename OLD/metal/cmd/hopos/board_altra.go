//go:build altra

// board_altra.go — de Ampere Altra-kant van de agent-main: dezelfde
// HOP-agent-bytes, met de generieke UEFI-laag voor discovery
// (MADT/MCFG/GOP/PSCI) en het Altra-board eroverheen (igb + SMpro). De RAM-declaratie is hier
// eigendom van board/uefi: het venster wordt door de PE-stub gekozen en
// RamStart per variant door mkkernel -pe gepatcht.
package main

import (
	"github.com/xinix00/HopOS/metal/v2/cmd/hopos/cfgblob"
	"time"
	_ "unsafe" // go:linkname (RAM-declaratie)

	altrahop "github.com/xinix00/HopOS/metal/v2/board/altra/hop" // registreert het board (init); de basis levert de tamago-hooks
	"github.com/xinix00/HopOS/metal/v2/board/uefi"
	"github.com/xinix00/HopOS/metal/v2/kern/kernflip"
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
	// De hardware-helft van de node-watchdog; beleid in watchdog.go.
	// PetEvery 4s bij de 12s-timeout van de SBSA-watchdog.
	nodeWDTOff = func() { uefi.WatchdogOff() }
	nodeWDT = &wdHardware{
		Arm:      func() (string, bool) { return uefi.WatchdogArm(12 * time.Second) },
		Pet:      uefi.WatchdogPet,
		PetEvery: 4 * time.Second,
	}

	// Platform-config uit hopos.cfg op de stick (door de stub vóór
	// ExitBootServices via de firmware-FAT gelezen — HopOS leest de config,
	// HOP-userspace kan er niet bij). Zelfde sleutels als de Pi-cmdline; de
	// main parseert ze. Beheer = het tekstbestandje bewerken, geen rebuild.
	// (Node-identiteit zonder hopos.node=: de main-default; een SMBIOS-
	// serial-terugval kan later via nodeSerial.)
	// De stick-config (uefi.BootConfigAll, via de firmware-feiten ook ná een
	// flip) wint; wat een flip-bundel ingebakken meebrengt (cmd/hopos/cfgblob,
	// -tags embedcfg) vult alleen sleutels aan die de stick niet zet — zoals
	// op de M4 (loader-blok eerst, dan cfgblob). Zo krijgt een meetbundel
	// zijn hopos.idlestat zonder de stick te herschrijven (20-09).
	bootParamAll = func(key string) []string {
		if v := uefi.BootConfigAll(key); len(v) > 0 {
			return v
		}
		return cfgblob.All(key)
	}
	kernflip.BoardHandoff = uefi.FwFactsCopy // firmware-feiten mee naar de geflipte kern
	kernflip.BoardOldCarve = uefi.OldCarve   // de carve van de vorige kern blijft buiten de pool
	kernflip.BoardPersistentCages = true
	kernflip.BoardScratchInWindow = true     // scratch = b+scratchOff: het paar verhuist mee naar het geleende venster
	kernflip.BoardFootprint = uefi.Footprint // lenen en vegen: RAM + carve, niet alleen RAM

	// Board-nawerk (het Pi-equivalent is StartDVFS): de temperatuur-
	// telemetrie uit de SMpro — de klok zelf is op servers firmware-domein.
	boardExtra = altrahop.StartTelemetry
}
