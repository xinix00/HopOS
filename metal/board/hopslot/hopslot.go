// Package hopslot is het generieke app-board: de board-basis voor images die
// in een HOP-slot draaien — en dat is voor een app het hele universum. Onder
// stage-2 raakt een app geen MMIO, geen firmware-tabel en geen UART; alles
// wat hij ziet is board-onafhankelijk: de ARM arch-timer (CNTFRQ zette de
// firmware), zijn eigen RAM-declaratie (door HOP gepatcht), de control-page
// en de ringen op hun IPA's. De kooi ís het board — dus één app-binary draait
// ongewijzigd op QEMU, de Pi's en de Altra.
//
// Dit pakket levert exact de tamago-goos-hooks die een gekooide image nodig
// heeft (allemaal MMIO-vrij) plus de appboard-registratie:
//
//   - Hwinit1: alleen ARM64-init + arch-timers + idle — geen DTB/ACPI-parse;
//   - Printk: stil (runtime-output valt; app-logs lopen via de hop-ABI-ring);
//   - RNG: RNDR (FEAT_RNG) waar het silicium dat heeft, anders een
//     jitter-geseede SHA-256-DRBG — het uefi-recept, zie rng.go;
//   - CoreID: de slotHint die HOP bij Start in de image patcht (het
//     laad-contract van slots.Start; MPIDR-aff0 als terugval voor images
//     die buiten slots om draaien, zoals QEMU-dev-boots);
//   - cpuinit (linkcpuinit): de kale EL1-tak — SCTLR schoon, stack uit de
//     RAM-declaratie, door naar de runtime (cpuinit.s).
//
// Per-board app-varianten (app4/app5/app-uefi) bestaan hiermee niet meer;
// de board-tags blijven alleen voor HOP-binaries (die linken drivers).
package hopslot

import (
	_ "unsafe" // voor go:linkname

	"github.com/usbarmory/tamago/arm64"

	"hop-os/metal/board/appboard"
	"hop-os/metal/cpu/idle"
)

// ARM64 is tamago's generieke ARM64-driver — timers, cache en Now lopen
// hierdoorheen; er zit geen board-kennis in.
var ARM64 = &arm64.CPU{
	TimerOffset: 1,
}

//go:linkname ramStackOffset runtime/goos.RamStackOffset
var ramStackOffset uint = 0x100

// hwinit1: post-World lagere-laag-init. Bewust het minimum: de firmware (of
// HOP's EL2-trampoline) heeft CNTFRQ en de timer-toegang al geregeld, dus
// InitGenericTimers(0, 0) berekent alleen de TimerMultiplier. Geen DTB- of
// ACPI-parse: een gekooide core mag die adressen niet eens aanraken.
//
//go:linkname hwinit1 runtime/goos.Hwinit1
func hwinit1() {
	ARM64.Init()
	ARM64.EnableCache()
	ARM64.InitGenericTimers(0, 0)
	idle.Enable() // ná Init (die zet de default governor)
}

//go:linkname nanotime runtime/goos.Nanotime
func nanotime() int64 {
	return ARM64.GetTime()
}

// mpidr leest MPIDR_EL1 (cpu_arm64.s).
func mpidr() uint64

// MPIDR geeft het rauwe MPIDR_EL1 van de huidige core. Voor apps is dit géén
// slotnummer (dat is CoreID via de slotHint) en de nummering is per board
// anders (aff0 vs aff1) — het enige board-onafhankelijke aan de waarde is dat
// hij per fysieke core VERSCHILT. Precies dat maakt hem bruikbaar als
// core-onderscheider in SMP-diagnostiek (appspike smpBench).
func MPIDR() uint64 { return mpidr() }

// slotHint wordt door slots.Start in een app-image gepatcht (symbool
// "hop-os/metal/board/hopslot.slotHint"): het slotnummer van deze start.
// 0 = niet gepatcht (een image buiten slots om). Moet in dít pakket blijven
// wonen — de symboolnaam is deel van het laad-contract.
var slotHint uint64

// CoreID geeft het eigen slotnummer. Primair uit de slotHint (board-neutraal:
// op servers is MPIDR géén slotnummer — Altra: aff0 altijd 0, en de Pi 5
// nummert in aff1). De aff0-terugval blijft voor images die buiten
// slots.Start om draaien (QEMU-dev-boots, waar aff0 wél de core-index is).
func CoreID() int {
	if slotHint != 0 {
		return int(slotHint)
	}
	return int(mpidr() & 0xFF)
}

// appBoard is het app-zichtbare board (appboard.Board): core-identiteit en
// klok-offset — meer heeft een app niet, en meer is er in een kooi ook niet.
type appBoard struct{}

func (appBoard) CoreID() int            { return CoreID() }
func (appBoard) SetTimerOffset(o int64) { ARM64.TimerOffset = o }

func init() { appboard.Use(appBoard{}) }
