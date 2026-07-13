package raspi

import (
	"fmt"
	"time"

	"hop-os/metal/dev"
)

// Hardware-watchdog van het BCM-PM-blok — dezelfde registerfamilie op de
// BCM2711 (Pi 4) en BCM2712 (Pi 5, bcm2712.dtsi watchdog@7d200000; Linux
// bcm2835-pm-wdt): een hardwareteller die het hele SoC reset als hij niet op
// tijd geaaid wordt. Dít is het vangnet voor een totale fabric-freeze
// (freeze-jacht 2026-07-13, C1-erratum): bevroren software kan niets meer,
// maar de PM-teller tikt onafhankelijk door en trekt de node er zelf uit —
// geen stekker nodig. Board-agnostisch via WatchdogBase (RNG200Base-patroon):
// elk board zet zijn PM-basis in init(); 0 = geen watchdog (bv. QEMU).
//
// PM_WDOG [19:0] = timeout in ticks van 1/65536 s (max ~16 s); PM_RSTC krijgt
// wrconfig FULL_RESET. Elke write eist het password in de topbyte.

// WatchdogBase is het board-specifieke PM-blok-basisadres (Pi 4: 0xFE100000,
// Pi 5: 0x10_7d20_0000), gezet door de board-init. 0 = geen watchdog.
var WatchdogBase uintptr

const (
	pmRSTC     = 0x1c
	pmWDOG     = 0x24
	pmPassword = 0x5a000000

	pmRSTCWrCfgMask  = 0x30
	pmRSTCFullReset  = 0x20
	pmWDOGTicksMask  = 0x000fffff
	pmTicksPerSecond = 65536
)

// WatchdogStart wapent de hardware-watchdog met de gegeven timeout (max ~15s)
// en start een aai-goroutine op een derde van de timeout. Bevriest de node
// volledig — inclusief een hangende bóót, mits vroeg gewapend — dan blijft de
// aai uit en reset het PM-blok het SoC; de reboot is de melding. No-op als
// het board geen WatchdogBase zette.
func WatchdogStart(timeout time.Duration) {
	base := WatchdogBase
	if base == 0 {
		return
	}
	if timeout > 15*time.Second {
		timeout = 15 * time.Second
	}
	ticks := uint32(timeout.Seconds()*pmTicksPerSecond) & pmWDOGTicksMask
	dev.Write32(base+pmWDOG, pmPassword|ticks)
	dev.Write32(base+pmRSTC, pmPassword|dev.Read32(base+pmRSTC)&^uint32(pmRSTCWrCfgMask)|pmRSTCFullReset)
	fmt.Printf("watchdog: hardware reset armed (%.0fs) — a full freeze now self-reboots\n", timeout.Seconds())
	go func() {
		for {
			time.Sleep(timeout / 3)
			dev.Write32(base+pmWDOG, pmPassword|ticks) // aaien: teller terug op vol
		}
	}()
}
