package rpi5

// PSCI (ARM DEN 0022) op de Pi 5: de armstub (TF-A BL31) draait op EL3 en
// levert ons op EL2 af → conduit SMC. De conduitkeuze loopt toch via het
// boot-EL op de scratch (cpuinit.s), zodat een onverwachte EL1-aflevering
// zichtbaar wordt in plaats van stil te falen.
//
// LET OP (meetpunt probe): de standaard Pi-armstub zet secundaire cores
// mogelijk al "aan" (CPU_ON → ALREADY_ON). Dan vervangen we hem met een
// zelfgebouwde upstream-TF-A bl31.bin (armstub=bl31.bin in config.txt), die
// cores netjes geparkeerd houdt tot CPU_ON. Zie docs/rpi5.md.

import "unsafe"

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

// BootEL geeft het exception level waarop de firmware ons afleverde (door
// cpuinit.s op de scratch geschreven; 0 ⇒ EL1-pad, dat niet schrijft).
func BootEL() uint64 {
	if el := *(*uint64)(unsafe.Pointer(uintptr(bootScratch))); el >= 2 {
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

// CPUOn start een secundaire core: core is de index (0..3); het
// MPIDR-target is aff1-genummerd (zie target). De core begint op entry
// (fysiek adres, MMU uit) met ctx in x0.
func CPUOn(core, entry, ctx uint64) int64 {
	return int64(call(PSCI_CPU_ON, target(core), entry, ctx))
}

// CPUOff zet de AANROEPENDE core uit. Keert bij succes nooit terug.
func CPUOff() int64 {
	return int64(call(PSCI_CPU_OFF, 0, 0, 0))
}

// AffinityInfo geeft de powertoestand van een core (index, niet MPIDR).
func AffinityInfo(core uint64) int64 {
	return int64(call(PSCI_AFFINITY_INFO, target(core), 0, 0))
}
