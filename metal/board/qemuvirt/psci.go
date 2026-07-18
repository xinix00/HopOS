package qemuvirt

// PSCI (ARM DEN 0022), conduit altijd SMC #0: HopOS eist een EL2-boot (QEMU
// met virtualization=on), dus de provider zit per definitie onder ons. De
// functie-ID's, return-codes, SMC-stub én de call-wrappers
// (psci.Version/On/Off/AffinityInfo) wonen in metal/cpu/psci.

import (
	"unsafe"

	"hop-os/metal/abi/layout"
)

// BootEL geeft het exception level waarop de firmware ons afleverde. Alleen
// het EL2/EL3-pad van cpuinit.s schrijft de scratch (het EL1-pad mag dat
// niet: app-cores hebben 'm read-only onder stage-2) — 0 betekent dus EL1.
// De mains weigeren te draaien als hier geen ≥2 staat, dus een onverwachte
// EL1-aflevering faalt zichtbaar vóór de eerste PSCI-call.
func BootEL() uint64 {
	if el := *(*uint64)(unsafe.Pointer(uintptr(layout.BootScratch))); el >= 2 {
		return el
	}
	return 1
}
