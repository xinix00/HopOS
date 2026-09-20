//go:build o6n

// board_o6n.go — de Radxa Orion O6N-kant van de agent-main: het UEFI-pad
// (board/uefi, PE-stub, ACPI) met het O6N-board (board/o6n/hop) eroverheen.
// Zelfde RAM-declaratie als board_uefi.go: het venster kiest de stub, RamStart
// patcht mkkernel -pe per variant.
package main

import (
	"fmt"
	"github.com/xinix00/HopOS/metal/v2/cmd/hopos/cfgblob"
	"time"
	_ "unsafe" // go:linkname (RAM-declaratie)

	"github.com/xinix00/HopOS/metal/v2/board"
	o6n "github.com/xinix00/HopOS/metal/v2/board/o6n/hop" // registreert het board (na board/uefi/hop)
	"github.com/xinix00/HopOS/metal/v2/board/uefi"
	"github.com/xinix00/HopOS/metal/v2/kern/kernflip"
)

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = uefi.GoRAMSize

func init() {
	// De SBSA-watchdog uit de GTDT, als de firmware er een beschrijft; anders
	// meldt Arm dat en draait de node zonder vangnet (dan is de Cix-eigen
	// watchdog het volgende meetpunt).
	nodeWDTOff = func() { uefi.WatchdogOff() }
	nodeWDT = &wdHardware{
		Arm:      func() (string, bool) { return uefi.WatchdogArm(12 * time.Second) },
		Pet:      uefi.WatchdogPet,
		PetEvery: 4 * time.Second,
	}
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

	// Board-nawerk: de klasse-indeling melden, de klok op vol (de firmware
	// laat de cores op de boot-OPP staan) en de eerste temperatuur — de
	// thermometer zelf loopt via board.Thermometer op de heartbeat.
	boardExtra = func() {
		fmt.Println(o6n.Describe())
		o6n.StartClock(bootParam)
		if mC := board.TempMilliC(); mC != 0 {
			fmt.Printf("hwmon: SoC %d.%dC (SCMI) - on every heartbeat\n", mC/1000, mC%1000/100)
		}
	}
}
