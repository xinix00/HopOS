//go:build tamago

package slots

import "runtime"

// ownRegion is de RAM-declaratie van déze kern: het venster dat poolInit uit
// de pool knipt (kern-flip, docs/kern-flip.md). Na een flip is dat het
// geleende venster; op een gewone boot het board-venster (dan is de knip een
// no-op — het is daar al een plan-hole).
func ownRegion() (start, end uint64) {
	s, e := runtime.MemRegion()
	return uint64(s), uint64(e)
}
