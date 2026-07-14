// watchdog.go — de SBSA Generic Watchdog: het zelfherstel-vangnet op
// UEFI/ACPI-servers, tegenhanger van de BCM-PM-watchdog op de Pi's
// (board/raspi/watchdog.go, zelfde filosofie: per default aan, een hang
// cyclet zichzelf naar een verse boot). De frames komen uit de ACPI GTDT;
// QEMU virt heeft er geen — dan is Start een gemelde no-op en dekt QEMU
// dit pad dus niet: eerste echte proef is de Altra zelf.
//
// Registerlayout (SBSA/BSA): control-frame WCS +0x000 (bit 0 = enable),
// WOR +0x008 (timeout in system-counter-ticks, 32 bits); refresh-frame
// WRR +0x000 (elke write herstart de teller). De teller loopt op CNTFRQ
// (Altra: 25MHz → 32-bit WOR haalt ~171s, ruim genoeg voor 12s).
package uefi

import (
	"fmt"
	"time"

	"hop-os/metal/dev"
)

// WatchdogStart wapent de SBSA-watchdog met de gegeven timeout en start de
// aai-goroutine (elke timeout/3). Geen GTDT-watchdog → melding + no-op:
// de aanroeper (board_uefi.go) hoeft niet per platform te kiezen.
func WatchdogStart(timeout time.Duration) {
	t := Tables()
	if t == nil {
		fmt.Printf("watchdog: no ACPI tables — disabled\n")
		return
	}
	refresh, control, found := t.Watchdog()
	if !found {
		fmt.Printf("watchdog: no SBSA watchdog in GTDT — disabled (QEMU?)\n")
		return
	}
	if !MapHigh(refresh, 0x1000) || !MapHigh(control, 0x1000) {
		fmt.Printf("watchdog: frames unreachable (refresh %#x, control %#x) — disabled\n", refresh, control)
		return
	}

	// SBSA is tweetraps: na WOR ticks komt de WS0-interrupt (die niemand
	// afhandelt — we pollen), pas na nóg eens WOR de WS1-reset. WOR krijgt
	// dus de HALVE timeout zodat de reset op de gevraagde tijd valt
	// (review #10; Linux sbsa_gwdt halveert om dezelfde reden). Via
	// milliseconden gerekend en met ondergrens 1: een sub-seconde-timeout
	// mag nooit WOR=0 (= onmiddellijke reset-lus) opleveren.
	ticks := uint64(timeout/time.Millisecond) * uint64(cntfrq()) / 2000
	if ticks == 0 {
		ticks = 1
	}
	if ticks > 0xFFFFFFFF {
		ticks = 0xFFFFFFFF // WOR is 32 bits; beter een langere timeout dan geen
	}
	dev.Write32(uintptr(control)+0x8, uint32(ticks)) // WOR
	dev.Write32(uintptr(refresh), 1)                 // WRR: teller vers
	dev.Write32(uintptr(control), 1)                 // WCS: enable
	dev.MB()
	fmt.Printf("watchdog: SBSA armed, %v (refresh %#x, control %#x)\n", timeout, refresh, control)

	go func() {
		for {
			time.Sleep(timeout / 3)
			dev.Write32(uintptr(refresh), 1)
		}
	}()
}

// cntfrq leest CNTFRQ_EL0 (cpu_arm64.s) — de tikfrequentie van de
// system counter, door de firmware gezet.
func cntfrq() uint32
