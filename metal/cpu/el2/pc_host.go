//go:build !(tamago && arm64)

package el2

// Host-stubs: kern/stage2 (host-getest) importeert dit pakket voor de
// PC-accessors. Op de ontwikkelmachine bestaat de asm niet; de waarden zijn
// daar betekenisloos — de host-tests toetsen het protocol, niet de adressen.
// ImageBlobs geeft nullen: de plan-regio-install (stage2) slaat zichzelf dan
// over, precies zoals de rest van de dev-laag op de host een no-op is.
func S2TrampPC() uint64    { return 0 }
func S2SMPTrampPC() uint64 { return 0 }
func SMPStubPC() uint64    { return 0 }
func EntryPC() uint64      { return 0 }

func SetRelocated(entry, tramp, smp uint64) {}

func ImageBlobs() (entry, entryEnd, tramp, trampEnd, smp, smpEnd uint64) {
	return 0, 0, 0, 0, 0, 0
}
