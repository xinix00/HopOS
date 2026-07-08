package raspi

// PSCI (ARM DEN 0022): op beide Pi's draait TF-A (de armstub, BL31) op EL3 en
// levert ons op EL2 af → conduit SMC. De conduitkeuze loopt via het boot-EL op
// de scratch (cpuinit.s van het board), zodat een onverwachte EL1-aflevering
// zichtbaar wordt in plaats van stil te falen. De functie-ID's, return-codes en
// de HVC/SMC-stubs wonen in metal/psci (gedeeld met qemuvirt).
//
// De vertaling core-index → MPIDR-target is boardspecifiek (A72: aff0,
// A76: aff1); alle calls hier nemen het al vertaalde target.

import (
	"unsafe"

	"hop-os/metal/psci"
)

// BootEL geeft het exception level waarop de firmware ons afleverde (door
// cpuinit.s op de scratch geschreven; 0 ⇒ EL1-pad, dat niet schrijft).
func BootEL() uint64 {
	if el := *(*uint64)(unsafe.Pointer(uintptr(BootScratch))); el >= 2 {
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

// CPUOn start een secundaire core: target is het MPIDR-target (al vertaald uit
// de core-index door het board). De core begint op entry (fysiek adres, MMU
// uit) met ctx in x0.
func CPUOn(target, entry, ctx uint64) int64 {
	return int64(call(psci.CPU_ON, target, entry, ctx))
}

// CPUOff zet de AANROEPENDE core uit. Keert bij succes nooit terug.
func CPUOff() int64 {
	return int64(call(psci.CPU_OFF, 0, 0, 0))
}

// AffinityInfo geeft de powertoestand van een core (MPIDR-target).
func AffinityInfo(target uint64) int64 {
	return int64(call(psci.AFFINITY_INFO, target, 0, 0))
}
