package uefi

// ReadESR/ReadFAR: ESR_EL1/FAR_EL1 van deze core — voor de laatste woorden
// bij een exception op HOP's core (cmd/hopos/exception_uefi.go).
func ReadESR() uint64
func ReadFAR() uint64
