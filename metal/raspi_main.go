//go:build rpi4 || rpi5

package main

// Gedeelde acceptatie-helpers voor de Pi 4- en Pi 5-main (pi4_main.go /
// pi5_main.go): byte-identiek tussen beide boards, hier één keer. De main()
// zelf blijft per board (verschillende markerstrings HOPOS_PI4_*/HOPOS_PI5_*,
// en de Pi 5 draait extra P2/P2b-secties — net/dvfs — die de Pi 4 nog niet
// heeft).

import (
	"fmt"
	"time"

	"hop-os/metal/layout"
	"hop-os/metal/slots"
)

// drainLogs pompt de ring-logs van een slot naar de console; count (optioneel)
// telt de regels voor de acceptatie-asserts.
func drainLogs(slot int, count *int) {
	for line := range slots.Logs(slot) {
		fmt.Printf("[slot%d] %s\n", slot, line)
		if count != nil {
			*count++
		}
	}
}

// waitExit wacht (begrensd) tot een slot exit meldt en geeft de exitcode.
func waitExit(slot int, timeout time.Duration) (uint64, error) {
	deadline := time.Now().Add(timeout)
	for slots.Get(slot).App != layout.StatusExited {
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("slot %d meldt geen exit", slot)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return slots.Get(slot).ExitCode, nil
}
