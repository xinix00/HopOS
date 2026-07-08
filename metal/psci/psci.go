// Package psci bevat de gedeelde PSCI/SMCCC-primitieven (ARM DEN 0022): de
// functie-ID's, de return-codes en de twee conduit-stubs (HVC/SMC #0). Eén
// bron van waarheid voor die waarden — een verkeerde functie-ID of return-code
// hoort maar op één plek te leven, niet per board hergedefinieerd.
//
// Wat NIET hier woont: de conduitkeuze (HVC vs SMC) en de core-index→MPIDR-
// vertaling zijn boardspecifiek (het boot-EL staat op een board-eigen scratch,
// de MPIDR-nummering verschilt per cluster) — die blijven in het board-pakket,
// dat HVC/SMC hieruit aanroept.
//
// Alleen voor GOOS=tamago GOARCH=arm64.
package psci

// SMCCC/PSCI function IDs (64-bit calling convention).
const (
	CPU_OFF       uint64 = 0x84000002
	CPU_ON        uint64 = 0xC4000003
	AFFINITY_INFO uint64 = 0xC4000004
	VERSION       uint64 = 0x84000000
)

// AFFINITY_INFO-resultaten.
const (
	AffinityOn        = 0
	AffinityOff       = 1
	AffinityOnPending = 2
)

// PSCI return codes.
const (
	SUCCESS        = 0
	NOT_SUPPORTED  = -1
	INVALID_PARAMS = -2
	DENIED         = -3
	ALREADY_ON     = -4
)

// HVC/SMC doen een HVC/SMC #0 met vier argumenten (SMCCC: args in R0-R3,
// resultaat in R0). Zie psci.s. Het board kiest welke conduit past bij waar de
// firmware ons afleverde (HVC = hypervisor boven ons, SMC = firmware onder ons).
func HVC(fn, a1, a2, a3 uint64) uint64
func SMC(fn, a1, a2, a3 uint64) uint64
