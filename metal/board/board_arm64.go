//go:build arm64

package board

// PSCISuccess is de PSCI-return-code voor succes (SMCCC).
const PSCISuccess int64 = 0

// Board is één concreet ARM64-board — het volledige HOP-contract, geïmplementeerd
// door de hop-helft van elk board-pakket (board/<x>/hop). Bovenop Common staat
// hier wat ARM-eigen is: het exceptielevel-model (de kooi is stage-2 onder EL2),
// PSCI-power-control en de EL2-trampolines.
type Board interface {
	Common

	// Boot: ≥2 vereist (stage-2-kooi); 1 = EL1: de mains weigeren te starten.
	BootEL() int

	// PSCI power-control (return: PSCISuccess of een foutcode).
	CPUOn(core, entry, ctx uint64) int64
	CPUOff() int64
	AffinityInfo(core uint64) PowerState
	PSCIVersion() (major, minor uint16)

	// S2TrampPC is het fysieke entrypoint van de EL2-trampoline voor app-cores
	// onder stage-2-isolatie. De hard-kill vereist géén board-methode meer: die
	// loopt board-neutraal via stage2.Revoke (stage-2-intrekking + HVC/TLBI),
	// niet via de interrupt-controller.
	S2TrampPC() uint64

	// S2SMPTrampPC (fase 5): één app over meerdere cores met een gedeelde heap.
	// Een secundaire core komt op via CPU_ON naar dit fysieke adres (in de
	// HOP-image) en ERET't naar de EL1-stub in het app-image. HOP publiceert het
	// op de control-page; de app-OS-laag (metal/cpu/smp) haalt zijn eigen
	// stub-adres bij el2.SMPStubPC — hetzelfde symbool op elk board, dus geen
	// board-methode. De app blijft oblivious.
	S2SMPTrampPC() uint64
}
