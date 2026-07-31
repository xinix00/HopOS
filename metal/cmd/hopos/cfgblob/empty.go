//go:build !embedcfg

package cfgblob

// Geen ingebakken config: het board leest zijn platform-config van het
// bootmedium (of heeft er geen).
const text = ""
