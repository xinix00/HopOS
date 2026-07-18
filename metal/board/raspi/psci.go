package raspi

// PSCI (ARM DEN 0022), conduit altijd SMC: op beide Pi's draait TF-A (de
// armstub, BL31) op EL3 en levert ons op EL2 af. De functie-ID's, return-codes,
// SMC-stub én de call-wrappers (psci.Version/On/Off/AffinityInfo) wonen in
// metal/cpu/psci; de vertaling core-index → MPIDR-target is boardspecifiek
// (A72: aff0, A76: aff1) en zit in rpi4/rpi5.Target.

import "unsafe"

// BootEL geeft het exception level waarop de firmware ons afleverde (door
// cpuinit.s op de scratch geschreven; 0 ⇒ EL1-pad, dat niet schrijft). De
// mains weigeren te draaien als hier geen ≥2 staat, dus een onverwachte
// EL1-aflevering faalt zichtbaar vóór de eerste SMC.
func BootEL() uint64 {
	if el := *(*uint64)(unsafe.Pointer(uintptr(BootScratch))); el >= 2 {
		return el
	}
	return 1
}
