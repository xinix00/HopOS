//go:build riscv64

package hopslot

import (
	_ "unsafe" // voor go:linkname

	"github.com/xinix00/HopOS/metal/cpu/drbg"
)

// Dezelfde SHA-256-DRBG als op ARM (metal/cpu/drbg), maar zonder
// hardware-seed: RISC-V heeft geen equivalent van FEAT_RNG/RNDR dat we hier
// mogen aannemen (Zkr is optioneel en de C906 heeft het niet), en de TRNG van
// de CV181x zit in het security-subsysteem — MMIO, dus buiten de kooi. De DRBG
// valt daarom terug op timing-jitter uit de TIME CSR, precies zoals hij dat op
// ARM-silicium zonder FEAT_RNG doet.
//
// Dat is geen crypto-grade entropie. Zodra HOP een TRNG-venster kan granten
// (of het silicium Zkr heeft) hoort hier een echte bron.

//go:linkname initRNG runtime/goos.InitRNG
func initRNG() { drbg.Init(noHardwareRNG, RV64.Counter) }

//go:linkname getRandomData runtime/goos.GetRandomData
func getRandomData(b []byte) { drbg.Read(b) }

// noHardwareRNG meldt "geen bron" zodat drbg naar jitter-seeding gaat.
func noHardwareRNG([]byte) (string, bool) { return "", false }
