package raspi

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// Hardware-watchdog van het BCM-PM-blok — dezelfde registerfamilie op de
// BCM2711 (Pi 4) en BCM2712 (Pi 5, bcm2712.dtsi watchdog@7d200000; Linux
// bcm2835-pm-wdt): een hardwareteller die het hele SoC reset als hij niet op
// tijd geaaid wordt. Dít is het vangnet voor een totale node-freeze, wat de
// oorzaak ook is (HOP-leven = node-leven): bevroren software kan niets meer,
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

// wdTicks onthoudt de gewapende timeout zodat WatchdogPet de teller op
// dezelfde waarde herlaadt.
var wdTicks uint32

// WatchdogArm wapent de hardware-watchdog met de gegeven timeout (max ~15s).
// Alleen de hardware — het beleid (wanneer aaien, wanneer niet) woont in
// cmd/hopos/watchdog.go, één keer voor alle boards. ok=false als het board
// geen WatchdogBase zette (QEMU).
func WatchdogArm(timeout time.Duration) (desc string, ok bool) {
	base := WatchdogBase
	if base == 0 {
		return "no PM watchdog base on this board", false
	}
	if timeout > 15*time.Second {
		timeout = 15 * time.Second
	}
	wdTicks = uint32(timeout.Seconds()*pmTicksPerSecond) & pmWDOGTicksMask
	dev.Write32(base+pmWDOG, pmPassword|wdTicks)
	dev.Write32(base+pmRSTC, pmPassword|dev.Read32(base+pmRSTC)&^uint32(pmRSTCWrCfgMask)|pmRSTCFullReset)
	return fmt.Sprintf("BCM PM block, %.0fs", timeout.Seconds()), true
}

// WatchdogPet laadt de teller terug op vol.
func WatchdogPet() {
	dev.Write32(WatchdogBase+pmWDOG, pmPassword|wdTicks)
}
