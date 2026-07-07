// Package qemuvirt biedt HopOS-support voor de QEMU `-M virt` arm64-machine:
// PL011-console, generic timers, en PSCI (HVC-conduit, door QEMU geleverd).
//
// Dit is ons multikernel-referentiedoel in fase 1: in tegenstelling tot QEMU's
// imx8mp-evk (directe -kernel boot, geen TF-A) levert virt gegarandeerd PSCI,
// tot -smp 12, GICv3 — dezelfde bouwstenen als de Orion O6N (daar via TF-A/SMC).
//
// Alleen voor GOOS=tamago GOARCH=arm64. Het pakket levert de verplichte
// runtime/goos-hooks (zie tamago's goos-pakket): RamStart, RamStackOffset,
// Hwinit1, Printk, Nanotime, InitRNG, GetRandomData. RamSize komt uit de app.
package qemuvirt

import (
	_ "unsafe"

	"github.com/usbarmory/tamago/arm64"

	"hop-os/metal/idle"
)

// QEMU virt memory map (hw/arm/virt.c, stabiel gedocumenteerd).
const (
	UART0Base = 0x09000000 // PL011
	uartDR    = UART0Base + 0x00
	uartFR    = UART0Base + 0x18
	frTXFF    = 1 << 5

	GICDBase = 0x08000000 // GICv3 distributor (nog ongebruikt)
	GICRBase = 0x080a0000 // GICv3 redistributors
)

// ARM64 core-instantie (zelfde constructie als tamago's imx8mp).
var ARM64 = &arm64.CPU{
	TimerOffset: 1,
}

// RamStart en RamSize worden door elke image zelf gedefinieerd (HOP-kern en
// app-images hebben elk een eigen partitie — zie metal/layout); alleen de
// stack-offset is voor iedereen gelijk.
//
//go:linkname ramStackOffset runtime/goos.RamStackOffset
var ramStackOffset uint = 0x100

// hwinit1: post-World lagere-laag-init. QEMU zet CNTFRQ zelf, dus
// InitGenericTimers(0, 0) berekent alleen de TimerMultiplier.
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

// CoreID geeft de eigen core-index (MPIDR aff0; op virt 0..11). Voor een
// app-core is dit tevens het slotnummer — de enige identiteitsbron die
// onafhankelijk is van het linkadres (images zijn canoniek gelinkt en
// draaien via de stage-2-vertaling op elk slot).
func CoreID() int { return int(mpidr() & 0xFF) }
