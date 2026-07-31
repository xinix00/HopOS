package licheerv

import (
	"fmt"
	"sync"
	_ "unsafe"
)

// PLACEHOLDER — xorshift64* geseed met mtime, NIET cryptografisch veilig. De
// CV181x heeft een hardware-TRNG in zijn security subsystem; die hoort hieraan
// gehangen te worden.
//
// Dit weegt zwaarder dan het klonk toen het hier kwam te staan. Toen deed dit
// board niets met crypto; nu haalt de agent zijn artifacts over TLS van GitHub
// en draait er een Cloudflare-tunnel in een slot. Elke sessiesleutel, nonce en
// ephemeral key komt dus uit een generator waarvan de hele staat volgt uit één
// mtime-lezing bij boot — voorspelbaar voor wie weet wanneer de node opkwam.
//
// Waarom hier geen TRNG staat en geen gegokt MMIO-adres: de registerkaart van dat
// subsysteem zit niet in de vendor-tree die we hebben (geen hwrng-driver, geen
// DTS-knoop), en adressen gokken op ijzer is precies hoe je een board stilzet.
// Referentie eerst. Tot die tijd is de eerlijke maatregel dat niemand hier per
// ongeluk op vertrouwt: Warn() zegt het bij élke boot, en luid.

var (
	rngMux   sync.Mutex
	rngState uint64
)

//go:linkname initRNG runtime/goos.InitRNG
func initRNG() {
	rngState = Mtime() | 1
}

// Warn meldt dat de entropiebron een placeholder is. De board-main roept dit bij
// het opkomen aan, náást de andere onveilig-waarschuwingen — één regel die een
// operator niet kan missen en die in de console-historie blijft staan.
func Warn() {
	fmt.Println("board: WARNING — no hardware TRNG on this board: Go crypto (TLS keys, nonces) runs on a boot-time-seeded PRNG and is PREDICTABLE. Do not put secrets or public services on this node. HOPOS_RNG_INSECURE")
}

//go:linkname getRandomData runtime/goos.GetRandomData
func getRandomData(b []byte) {
	rngMux.Lock()
	defer rngMux.Unlock()

	for i := range b {
		rngState ^= rngState << 13
		rngState ^= rngState >> 7
		rngState ^= rngState << 17
		b[i] = byte((rngState * 0x2545f4914f6cdd1d) >> 56)
	}
}
