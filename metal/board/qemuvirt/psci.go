package qemuvirt

// PSCI (Power State Coordination Interface, ARM DEN 0022). De conduit hangt
// af van waar de firmware ons afleverde — cpuinit.s schrijft het boot-EL op
// de scratch-pagina vóór de drop naar EL1:
//
//   - EL1-boot (QEMU virt zonder virtualization=on): QEMU is de "hypervisor"
//     boven ons → HVC #0.
//   - EL2-boot (QEMU virtualization=on, TF-A op Pi 5/O6N): de provider zit
//     onder ons → SMC #0.

import (
	"unsafe"

	"hop-os/metal/layout"
)

// SMCCC/PSCI function IDs (64-bit calling convention).
const (
	PSCI_VERSION       uint64 = 0x84000000
	PSCI_CPU_OFF       uint64 = 0x84000002
	PSCI_CPU_ON        uint64 = 0xC4000003
	PSCI_AFFINITY_INFO uint64 = 0xC4000004
)

// AFFINITY_INFO-resultaten.
const (
	AffinityOn        = 0
	AffinityOff       = 1
	AffinityOnPending = 2
)

// PSCI return codes.
const (
	PSCI_SUCCESS        = 0
	PSCI_NOT_SUPPORTED  = -1
	PSCI_INVALID_PARAMS = -2
	PSCI_DENIED         = -3
	PSCI_ALREADY_ON     = -4
)

// hvc4/smc4 doen een HVC/SMC #0 met vier argumenten (SMCCC). Zie psci.s.
func hvc4(fn, a1, a2, a3 uint64) uint64
func smc4(fn, a1, a2, a3 uint64) uint64

// S2TrampPC geeft het fysieke adres van de EL2-trampoline (el2.s): het
// CPU_ON-entrypoint voor app-cores onder stage-2-isolatie.
func S2TrampPC() uint64

// BootEL geeft het exception level waarop de firmware ons afleverde. Alleen
// het EL2/EL3-pad van cpuinit.s schrijft de scratch (het EL1-pad mag dat
// niet: app-cores hebben 'm read-only onder stage-2) — 0 betekent dus EL1.
func BootEL() uint64 {
	if el := *(*uint64)(unsafe.Pointer(uintptr(layout.BootScratch))); el >= 2 {
		return el
	}
	return 1
}

// call kiest de PSCI-conduit op basis van het boot-EL.
func call(fn, a1, a2, a3 uint64) uint64 {
	if BootEL() >= 2 {
		return smc4(fn, a1, a2, a3)
	}
	return hvc4(fn, a1, a2, a3)
}

// PSCIVersion geeft (major, minor) van de PSCI-provider.
func PSCIVersion() (major, minor uint16) {
	v := call(PSCI_VERSION, 0, 0, 0)
	return uint16(v >> 16), uint16(v)
}

// CPUOn start een secundaire core (target = MPIDR-affiniteit; op virt is
// core N gewoon N). De core begint op entry (fysiek adres, MMU uit) met
// ctx in x0.
func CPUOn(target, entry, ctx uint64) int64 {
	return int64(call(PSCI_CPU_ON, target, entry, ctx))
}

// CPUOff zet de AANROEPENDE core uit (PSCI kent geen remote CPU_OFF).
// Keert bij succes nooit terug.
func CPUOff() int64 {
	return int64(call(PSCI_CPU_OFF, 0, 0, 0))
}

// AffinityInfo geeft de powertoestand van een core (AffinityOn/Off/OnPending).
func AffinityInfo(target uint64) int64 {
	return int64(call(PSCI_AFFINITY_INFO, target, 0, 0))
}
