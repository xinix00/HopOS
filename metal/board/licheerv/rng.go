package licheerv

import (
	"fmt"
	"time"
	_ "unsafe"

	"hop-os/metal/cpu/drbg"
	"hop-os/metal/cpu/idle"
)

// Dezelfde SHA-256-DRBG als op de andere boards (metal/cpu/drbg), zonder
// hardware-seed: de TRNG van de CV181x zit in het security-subsysteem en de
// registerkaart daarvan ontbreekt in de vendor-tree (geen hwrng-driver, geen
// DTS-knoop) — adressen gokken op ijzer is precies hoe je een board stilzet;
// referentie eerst. De DRBG seedt daarom op timing-jitter uit de teller (zoals
// hopslot's riscv64-helft en ARM-silicium zonder FEAT_RNG). Dat is véél beter
// dan de mtime-geseedde xorshift die hier stond (die was integraal
// voorspelbaar uit de boottijd), maar nog geen hardware-entropie — Warn()
// blijft het bij elke boot luid zeggen, zodat niemand er per ongeluk
// geheimen op bouwt.

//go:linkname initRNG runtime/goos.InitRNG
func initRNG() { drbg.Init(noHardwareRNG, RV64.Counter) }

// noHardwareRNG meldt "geen bron" zodat drbg naar jitter-seeding gaat.
func noHardwareRNG([]byte) (string, bool) { return "", false }

// Warn meldt dat de entropiebron een placeholder is. De board-main roept dit bij
// het opkomen aan, náást de andere onveilig-waarschuwingen — één regel die een
// operator niet kan missen en die in de console-historie blijft staan.
func Warn() {
	fmt.Println("board: WARNING — no hardware TRNG on this board: Go crypto (TLS keys, nonces) runs on a jitter-seeded DRBG, not hardware entropy. Avoid high-value secrets on this node. HOPOS_RNG_INSECURE")

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
func getRandomData(b []byte) { drbg.Read(b) }
