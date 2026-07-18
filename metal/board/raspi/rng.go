package raspi

import (
	_ "unsafe" // voor go:linkname

	"hop-os/metal/dev"
)

// Hardware-entropie voor de Pi 4/5: de Broadcom RNG200 (iproc-rng200), het
// on-chip TRNG-blok van de BCM2711/2712. Vervangt de placeholder-xorshift die
// hier vóór P2 stond — die was expliciet NIET cryptografisch en moest weg vóór
// er TLS/keys op de Pi draaien. RNG200 voedt via runtime/goos.GetRandomData de
// hele runtime z'n crypto/rand.
//
// Recept uit de Linux-referentie drivers/char/hw_random/iproc-rng200.c: schakel
// de RBG in (RBGEN in RNG_CTRL), doe de warm-up/restart-sequence (soft-reset van
// RBG en RNG + IRQ-status wissen), en lees dan per 32-bit-woord uit de FIFO
// zodra RNG_FIFO_COUNT[7:0] > 0. LET OP: dit is de iproc-rng200-variant (BCM2711/
// 2712, DT "brcm,bcm2711-rng200") — de teller staat in FIFO_COUNT (0x24, bits
// [7:0]) en de data in FIFO_DATA (0x20); dit is NIET het oudere bcm2835-rng-blok
// (RNG_STATUS-count op 0x4). Zie docs/archief/rpi5.md resp. docs/archief/rpi4.md.
//
// Het basisadres verschilt per board (Pi 4: 0xFE104000, Pi 5: 0x107d208000), dus
// rng.go is board-agnostisch: elk board zet RNG200Base in zijn init(). Zolang
// RNG200Base nog 0 is (board-init nog niet gedraaid, of een niet-Pi-board dat
// deze bestand niet compileert) valt getRandomData terug op de PRNG — die dekt
// alleen de vroege boot-trekkingen vóór de eerste crypto. board/qemuvirt houdt
// bewust zijn eigen kopie (andere ARM64-instantie, geen RNG200).
//
// Levert de hardware binnen een begrensde poll geen woord (FIFO leeg door een
// storing), dan valt díé trekking terug op de PRNG mét een eenmalige waarschuwing
// — nooit een oneindige lus, maar het default-pad blíjft de RNG200.

// RNG200Base is het board-specifieke MMIO-basisadres van het RNG200-blok, gezet
// door de board-init (rpi4/rpi5). 0 = onbekend → PRNG-terugval.
var RNG200Base uintptr

// RNG200-registeroffsets (iproc-rng200.c).
const (
	rngCtrl      = 0x00 // RBGEN in [12:0]
	rngSoftReset = 0x04 // bit0 = RNG soft-reset
	rbgSoftReset = 0x08 // bit0 = RBG soft-reset
	rngIntStatus = 0x18 // schrijf 0xFFFFFFFF = alle IRQ-status wissen
	rngFIFOData  = 0x20 // één 32-bit random-woord per read
	rngFIFOCount = 0x24 // aantal beschikbare woorden in [7:0]

	rngCtrlRBGENMask   = 0x00001FFF
	rngCtrlRBGENEnable = 0x00000001
	rngSoftResetBit    = 0x00000001
	rbgSoftResetBit    = 0x00000001
	rngFIFOCountMask   = 0x000000FF

	// rngPollLimit begrenst het wachten op één FIFO-woord (nooit eeuwig). Ruim:
	// de RBG levert na de warm-up continu, dus dit raakt alleen bij een storing.
	rngPollLimit = 200_000

	// rngWarmupLimit is de ruimere grens voor het ÁLLEREERSTE woord na de
	// enable: de ring-oscillatoren moeten entropie opbouwen en dat duurt op de
	// BCM2711 aantoonbaar langer dan op de BCM2712 (gemeten 2026-07-11: de Pi 4
	// viel bij de eerste boot-trekking terug, de Pi 5 niet).
	rngWarmupLimit = 5_000_000
)

var (
	rngState    uint64 // PRNG-fallback-state (xorshift64*)
	rng200Ready bool   // RNG200 ingeschakeld + warm-up gedaan
	rng200Warn  bool   // eenmalige fallback-waarschuwing gedaan
	rng200Said  bool   // eenmalige "online"-bevestiging gedaan (pre-soak-check #3)
)

//go:linkname initRNG runtime/goos.InitRNG
func initRNG() {
	// PRNG-fallback seeden uit de cycle-counter — voor de vroege trekkingen die
	// de runtime doet vóór board-init RNG200Base zet, en voor de begrensde-poll-
	// terugval. De RNG200 zelf wordt lazy in getRandomData ingeschakeld zodra
	// z'n basis bekend is (de init-volgorde runtime↔board-init ligt niet vast).
	rngState = ARM64.Counter() | 1
}

//go:linkname getRandomData runtime/goos.GetRandomData
func getRandomData(b []byte) {
	base := RNG200Base
	if base == 0 {
		prngFill(b) // geen hardware-basis (board-init nog niet gedraaid): PRNG
		return
	}
	limit := rngPollLimit
	if !rng200Ready {
		rng200Restart(base)
		rng200Ready = true
		limit = rngWarmupLimit // eerste woord na enable: warm-up-marge (BCM2711!)
	}
	for off := 0; off < len(b); {
		// Wacht (begrensd) op minstens één beschikbaar woord.
		avail := false
		for i := 0; i < limit; i++ {
			if dev.Read32(base+rngFIFOCount)&rngFIFOCountMask != 0 {
				avail = true
				break
			}
		}
		if !avail {
			// Hardware levert niet op tijd: val voor de rest van déze trekking
			// terug op de PRNG en waarschuw één keer. Default blíjft de RNG200.
			if !rng200Warn {
				rng200Warn = true
				print("HOPOS_RNG200_FALLBACK: no FIFO word within the poll limit — PRNG for this draw\n")
			}
			prngFill(b[off:])
			return
		}
		limit = rngPollLimit
		if !rng200Said {
			// Positief bewijs voor de soak-log (pre-soak-check #3), één keer:
			// de hardware-RNG levert écht.
			rng200Said = true
			print("rng: RNG200 online — hardware entropy available\n")
		}
		w := dev.Read32(base + rngFIFOData)
		for j := 0; j < 4 && off < len(b); j, off = j+1, off+1 {
			b[off] = byte(w)
			w >>= 8
		}
	}
}

// rng200Restart draait de iproc-rng200-warm-up: RBG uit, IRQ-status wissen,
// RBG en RNG soft-resetten, weer aan. Hierna vult de FIFO zich.
func rng200Restart(base uintptr) {
	rng200Enable(base, false)
	dev.Write32(base+rngIntStatus, 0xFFFFFFFF)

	dev.Write32(base+rbgSoftReset, dev.Read32(base+rbgSoftReset)|rbgSoftResetBit)
	dev.Write32(base+rngSoftReset, dev.Read32(base+rngSoftReset)|rngSoftResetBit)
	dev.Write32(base+rngSoftReset, dev.Read32(base+rngSoftReset)&^uint32(rngSoftResetBit))
	dev.Write32(base+rbgSoftReset, dev.Read32(base+rbgSoftReset)&^uint32(rbgSoftResetBit))

	rng200Enable(base, true)
}

// rng200Enable zet of wist RBGEN in RNG_CTRL (de andere RBGEN-bits op 0).
func rng200Enable(base uintptr, on bool) {
	v := dev.Read32(base+rngCtrl) &^ uint32(rngCtrlRBGENMask)
	if on {
		v |= rngCtrlRBGENEnable
	}
	dev.Write32(base+rngCtrl, v)
}

// prngFill vult b met de xorshift64*-PRNG — de niet-cryptografische fallback.
func prngFill(b []byte) {
	x := rngState
	for i := range b {
		x ^= x >> 12
		x ^= x << 25
		x ^= x >> 27
		b[i] = byte((x * 0x2545F4914F6CDD1D) >> 56)
	}
	rngState = x
}
