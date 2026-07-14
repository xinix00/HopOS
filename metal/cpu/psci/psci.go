// Package psci bevat de gedeelde PSCI/SMCCC-primitieven (ARM DEN 0022): de
// functie-ID's, de return-codes en de conduit-stub (SMC #0). Eén bron van
// waarheid voor die waarden — een verkeerde functie-ID of return-code hoort
// maar op één plek te leven, niet per board hergedefinieerd.
//
// De conduit is altijd SMC: HopOS eist een EL2-boot (stage-2-isolatie is een
// invariant, geen optie), dus de PSCI-provider zit per definitie ónder ons
// (TF-A op Pi/O6N, QEMU met virtualization=on). Wat NIET hier woont: de
// core-index→MPIDR-vertaling is boardspecifiek (de nummering verschilt per
// cluster) — die blijft in het board-pakket.
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

// SMC doet een SMC #0 met vier argumenten (SMCCC: args in R0-R3, resultaat
// in R0). Zie psci.s.
func SMC(fn, a1, a2, a3 uint64) uint64

// SMC4 doet een SMC #0 en geeft R0-R3 terug. Nodig voor SMCCC-functies die
// hun resultaat niet in R0 alleen leveren maar over R1-R3 verspreiden — met
// name TRNG_RND (Arm DEN 0098): R0 = statuscode, R1:R2:R3 = de entropie. Zie
// psci.s.
func SMC4(fn, a1, a2, a3 uint64) (r0, r1, r2, r3 uint64)
