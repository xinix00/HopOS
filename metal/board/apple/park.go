//go:build tamago && arm64

package apple

import "github.com/xinix00/HopOS/metal/dev"

// ParkEntryPC is het fysieke startadres van de parkeerlus (park_arm64.s) dat
// als entry naar Release gaat — zelfde vorm als rk3566.ParkEntryPC.
func ParkEntryPC() uint64

// Levenstekenwoorden: per core één, op WakeBase — buiten de RAM-declaratie en
// dus device-gemapt (zie de toelichting bij WakeBase). De HOP-core nult het
// woord vóór Release en pollt daarna; de gewekte core schrijft er zijn MPIDR
// in. Eén woord dat twee vragen beantwoordt: kwam hij op, en klopt m1n1's
// MPIDR-tabel met wat de core zelf zegt.
func WakeSlot(core int) uintptr { return uintptr(WakeBase) + uintptr(core)*8 }

// ClearWake nult het levenstekenwoord van een core.
func ClearWake(core int) { dev.Write64(WakeSlot(core), 0) }

// Wake leest het levenstekenwoord (0 = de core is nooit aangekomen).
func Wake(core int) uint64 { return dev.Read64(WakeSlot(core)) }

// In regs_arm64.s: systeemregisters die de probe rapporteert.
func ReadTCR() uint64
func ReadSCTLR() uint64
func ReadMMFR0() uint64
func ReadMMFR1() uint64
func ReadMMFR4() uint64
func CNTFRQ() uint64
func ReadCNTKCTL() uint64
func ReadSPRRConfig() uint64

// Idle-mechanica (meting, zie regs_arm64.s).
func wfeBurst(n uint64) uint64
func wfiTimer(ticks uint64) uint64

// WFEBurst/WFITimer zijn de geëxporteerde meetfuncties voor de probe.
func WFEBurst(n uint64) uint64 { return wfeBurst(n) }
func WFITimer(t uint64) uint64 { return wfiTimer(t) }
