package qemuvirt

// PSCI (Power State Coordination Interface, ARM DEN 0022). De conduit hangt
// af van waar de firmware ons afleverde — cpuinit.s schrijft het boot-EL op
// de scratch-pagina vóór de drop naar EL1:
//
//   - EL1-boot (QEMU virt zonder virtualization=on): QEMU is de "hypervisor"
//     boven ons → HVC #0.
//   - EL2-boot (QEMU virtualization=on, TF-A op Pi 5/O6N): de provider zit
//     onder ons → SMC #0.
//
// De functie-ID's, return-codes en de HVC/SMC-stubs wonen in metal/psci
// (gedeeld met board/raspi).

import (
	"unsafe"

	"hop-os/metal/layout"
	"hop-os/metal/psci"
)

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
		return psci.SMC(fn, a1, a2, a3)
	}
	return psci.HVC(fn, a1, a2, a3)
}

// PSCIVersion geeft (major, minor) van de PSCI-provider.
func PSCIVersion() (major, minor uint16) {
	v := call(psci.VERSION, 0, 0, 0)
	return uint16(v >> 16), uint16(v)
}

// CPUOn start een secundaire core (target = MPIDR-affiniteit; op virt is
// core N gewoon N). De core begint op entry (fysiek adres, MMU uit) met
// ctx in x0.
func CPUOn(target, entry, ctx uint64) int64 {
	return int64(call(psci.CPU_ON, target, entry, ctx))
}

// CPUOff zet de AANROEPENDE core uit (PSCI kent geen remote CPU_OFF).
// Keert bij succes nooit terug.
func CPUOff() int64 {
	return int64(call(psci.CPU_OFF, 0, 0, 0))
}

// AffinityInfo geeft de powertoestand van een core (AffinityOn/Off/OnPending).
func AffinityInfo(target uint64) int64 {
	return int64(call(psci.AFFINITY_INFO, target, 0, 0))
}
