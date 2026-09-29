// Package checksum levert de content-som die HOP én de apps over hetzelfde
// bestand rekenen. Eén bron van waarheid voor de algoritmekeuze (FNV-1a via
// hash/fnv): zo kunnen app-kant en HOP-kant nooit uit elkaar lopen op een
// hand-getypte variant.
package checksum

import "hash/fnv"

// FNV64 is de FNV-1a-64-som van b.
func FNV64(b []byte) uint64 {
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}
