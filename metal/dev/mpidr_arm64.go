//go:build tamago

package dev

// MPIDR geeft het rauwe MPIDR_EL1 van de huidige core (dev_arm64.s). Geen
// slotnummer en de nummering is per board anders (aff0 vs aff1/aff2) — het
// enige board-onafhankelijke aan de waarde is dat hij per fysieke core
// VERSCHILT. De boards interpreteren hem zelf (CoreID per board).
func MPIDR() uint64
