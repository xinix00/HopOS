package qemuvirt

// PSCI (Power State Coordination Interface, ARM DEN 0022), conduit altijd
// SMC #0: HopOS eist een EL2-boot (QEMU met virtualization=on), dus de
// provider zit per definitie onder ons. cpuinit.s schrijft het boot-EL op de
// scratch-pagina — de mains weigeren te draaien als daar geen ≥2 staat, dus
// een onverwachte EL1-aflevering faalt zichtbaar vóór de eerste PSCI-call.
// De functie-ID's, return-codes en de SMC-stub wonen in metal/psci (gedeeld
// met board/raspi).

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

// PSCIVersion geeft (major, minor) van de PSCI-provider.
func PSCIVersion() (major, minor uint16) {
	v := psci.SMC(psci.VERSION, 0, 0, 0)
	return uint16(v >> 16), uint16(v)
}

// CPUOn start een secundaire core (target = MPIDR-affiniteit; op virt is
// core N gewoon N). De core begint op entry (fysiek adres, MMU uit) met
// ctx in x0.
func CPUOn(target, entry, ctx uint64) int64 {
	return int64(psci.SMC(psci.CPU_ON, target, entry, ctx))
}

// CPUOff zet de AANROEPENDE core uit (PSCI kent geen remote CPU_OFF).
// Keert bij succes nooit terug.
func CPUOff() int64 {
	return int64(psci.SMC(psci.CPU_OFF, 0, 0, 0))
}

// AffinityInfo geeft de powertoestand van een core (AffinityOn/Off/OnPending).
func AffinityInfo(target uint64) int64 {
	return int64(psci.SMC(psci.AFFINITY_INFO, target, 0, 0))
}
