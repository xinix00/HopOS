//go:build tamago && arm64

package rk3566

import "github.com/xinix00/HopOS/metal/dev"

// ParkEntryPC is het fysieke startadres van de parkeerlus (park_arm64.s) dat
// als `entry` naar PSCI CPU_ON gaat.
func ParkEntryPC() uint64

// Levenstekenwoorden: per core één, op WakeBase — BUITEN de RAM-declaratie en
// dus device-gemapt. Dat is een GEMETEN eis en geen smaak: in de eerste
// iteratie lagen ze binnen het venster, en toen meldde PSCI "accepted" terwijl
// de core stil bleef — hij schreef zijn woord in een regio die wij gecachet
// lezen, en de core komt met MMU uit (zie de toelichting bij WakeBase in
// rk3566.go). De HOP-core nult het woord vóór CPU_ON en pollt daarna; de gewekte
// core schrijft er zijn MPIDR in. Eén woord dat twee vragen beantwoordt: kwam
// hij op, en welke affiniteitsnummering heeft dit silicium.
func WakeSlot(core int) uintptr { return uintptr(WakeBase) + uintptr(core)*8 }

// ClearWake nult het levenstekenwoord van een core.
func ClearWake(core int) { dev.Write64(WakeSlot(core), 0) }

// Wake leest het levenstekenwoord (0 = de core is nooit aangekomen).
func Wake(core int) uint64 { return dev.Read64(WakeSlot(core)) }
