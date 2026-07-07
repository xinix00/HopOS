package rpi4

// PSCI loopt via de gedeelde raspi-laag (TF-A/armstub op EL3, conduit SMC —
// zie board/raspi/psci.go); dit bestand vertaalt alleen core-indexen naar
// A72-MPIDR-targets (aff0).
//
// LET OP: anders dan op de Pi 5 is TF-A hier geen "mogelijk nodig" maar een
// harde eis — de stock armstub8 heeft helemaal geen PSCI (spin-table) en
// een SMC hangt dan. Zie docs/rpi4.md en sd-rpi4/LEESMIJ.txt.

import "hop-os/metal/board/raspi"

// BootEL geeft het exception level waarop de firmware ons afleverde.
func BootEL() uint64 { return raspi.BootEL() }

// PSCIVersion geeft (major, minor) van de PSCI-provider.
func PSCIVersion() (major, minor uint16) { return raspi.PSCIVersion() }

// CPUOn start een secundaire core: core is de index (0..3). De core begint
// op entry (fysiek adres, MMU uit) met ctx in x0.
func CPUOn(core, entry, ctx uint64) int64 {
	return raspi.CPUOn(target(core), entry, ctx)
}

// CPUOff zet de AANROEPENDE core uit. Keert bij succes nooit terug.
func CPUOff() int64 { return raspi.CPUOff() }

// AffinityInfo geeft de powertoestand van een core (index, niet MPIDR).
func AffinityInfo(core uint64) int64 { return raspi.AffinityInfo(target(core)) }
