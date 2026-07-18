//go:build qemuvirt || rpi4 || rpi5

package main

// Gedeelde slot-helpers van álle embed-mains (virt/pi4/pi5) — stonden
// byte-identiek in virt_main.go én raspi_main.go, hier één keer.

import (
	"fmt"
	"time"

	"hop-os/metal/abi/layout"
	"hop-os/metal/kern/slots"
)

// drainLogs abonneert op het logkanaal van de actieve servicer van een slot
// en multiplext de regels geprefixt naar de console — wat HOP's
// LogBroadcaster (GetStdout) doet. Per Start opnieuw aanroepen: elke start
// krijgt een verse servicer (en dus een vers kanaal). count (optioneel) telt
// de regels voor de acceptatie-asserts.
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
