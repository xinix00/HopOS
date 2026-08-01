package licheerv

import (
	"fmt"
	"sync"
	"time"
	_ "unsafe"

	"hop-os/metal/cpu/idle"
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

	// Meteen ook de CLINT-uitslag: of dit hart een wekker heeft bepaalt of een
	// hart mag slapen of moet blijven spinnen (clint.go). Hier omdat dit de
	// plek is waar dit board over zichzelf vertelt wat de code niet kan weten.
	fmt.Println("board: " + ProbeCLINT())

	// Slaagt de probe, dan gaat HOP's EIGEN hart óók aan de governor — geen
	// aparte slaap-implementatie, dezelfde cpu/idle-governor als elke app, met
	// als enige verschil de laatste stap: een app bereikt de M-mode-slaapcode
	// via zijn ecall-yield, HOP staat al in M-mode en roept hem direct (MSleep,
	// door de probe zelf bewezen). HOP is gewoon de eerste bewoner van zijn
	// hart. Zónder wekker gebeurt hier niets en spint de Go-scheduler zoals hij
	// altijd deed — falen gaat de goede kant op.
	//
	// De plek (na de probe, vóór agentboot.Run) en niet board-Init zoals op de
	// ARM-boards: dáár is WFE onvoorwaardelijk veilig, hier is de wekker een
	// probe-uitslag.
	if CLINTUsable() {
		idle.UseMSleep(MSleep)
		idle.Enable()
	}

	// En de thermometer: dit board is fanless en de slaap-stand is een
	// warmte-claim — die toets je met de on-die sensor (temp.go), niet met een
	// vinger. Eén regel per minuut; zo draagt élke duurtest zijn eigen
	// temperatuurverloop in de console.
	TempInit()
	go func() {
		for {
			t := TempMilliC()
			fmt.Printf("board: die temp %d.%dC\n", t/1000, t%1000/100)
			time.Sleep(time.Minute)
		}
	}()
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
