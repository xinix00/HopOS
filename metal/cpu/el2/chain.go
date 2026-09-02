//go:build tamago && arm64

package el2

import (
	"runtime"

	"github.com/xinix00/HopOS/metal/abi/layout"
)

// chainload is de sprong zelf (chain_arm64.s): DAIF dicht, MMU/caches uit
// (m1n1's mmu_disable, lezen-en-maskeren), datacache by-VA geveegd over
// [ramStart,ramEnd) — het enige cacheable venster van deze kern — en de
// I-hygiëne van élke spring-ingang (hygiene.h). Keert nooit terug.
func chainload(entry, x0arg, ramStart, ramEnd, vbar uint64)

// Chainload springt op EL2 in een nieuwe, net geplaatste kern (de kern-flip,
// docs/kern-flip.md): entry = fysiek entrypoint, x0arg = de firmware-x0 voor
// de nieuwe kern. Dit woont hier en niet in kern/stage2 omdat het
// EL2-machinerie is, broertje van s2tramp en smpEL2Tramp — drie manieren om
// een core in verse code te laten springen, één pakket, één hygiëne-blok.
func Chainload(entry, x0arg uint64) {
	s, e := runtime.MemRegion()
	// De vectoren alvast op de kale el2fault-dumper (de tabel op RevokeVecPA):
	// een vroege fault in de LANDING vectort anders door de nog levende
	// tamago-vectoren van DEZE kern — recht het lijk in, dat de echte
	// ESR/ELR/FAR opeet en een onzin-trace print (GEMETEN 01-09, zevende
	// ijzer-flip: "EL1 exception" met oude g-nummers en oude adressen).
	chainload(entry, x0arg, uint64(s), uint64(e), uint64(layout.RevokeVecPA()))
}
