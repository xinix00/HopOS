//go:build tamago && arm64

package el2

// De IMAGE-adressen van de drie EL2-blobs (HOP-image is identity-geladen,
// dus symbooladres = fysiek adres) plus hun eindmarkers. Dit zijn de
// kopieerbronnen voor de plan-regio-install (kern/stage2); de publieke
// accessors staan in pc.go en geven de plan-kopie zodra die er is.
func entryPC() uint64
func entryEndPC() uint64
func s2trampPC() uint64
func s2trampEndPC() uint64
func smpTrampPC() uint64
func smpTrampEndPC() uint64

// SMPStubPC geeft het adres van de EL1-stub (smp.s). In een app-image is dat
// de IPA waar goos.Task naar laat ERET'en zodra de gedeelde stage-2 actief is.
// Deze blijft een image-adres: hij draait uit het APP-image op EL1 en doet
// niet mee met de plan-regio-verhuizing.
func SMPStubPC() uint64
