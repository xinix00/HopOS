//go:build !(tamago && arm64)

package el2

// Host-stubs: kern/stage2 (host-getest) importeert dit pakket voor de
// PC-accessors. Op de ontwikkelmachine bestaat de asm niet; de waarden zijn
// daar betekenisloos — de host-tests toetsen het protocol, niet de adressen.
func S2TrampPC() uint64    { return 0 }
func S2SMPTrampPC() uint64 { return 0 }
func SMPStubPC() uint64    { return 0 }
func EntryPC() uint64      { return 0 }
