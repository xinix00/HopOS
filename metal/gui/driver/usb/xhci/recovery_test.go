//go:build gui

package xhci

import (
	"errors"
	"strings"
	"testing"
)

func TestRecoverRebuildsRetainedDMAWindow(t *testing.T) {
	h := &HC{
		Name:     "test",
		dmaBase:  0x120000,
		dmaSize:  0x200000,
		poisoned: errors.New("disable slot completion unknown"),
	}

	var steps []string
	err := h.recoverWith(
		func() error {
			steps = append(steps, "reset")
			return nil
		},
		func(base, size uintptr) error {
			steps = append(steps, "start")
			if base != h.dmaBase || size != h.dmaSize {
				t.Fatalf("Start kreeg DMA [%#x,%#x), wil [%#x,%#x)", base, base+size, h.dmaBase, h.dmaBase+h.dmaSize)
			}
			return nil
		},
		func() { steps = append(steps, "power") },
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(steps, ","), "reset,start,power"; got != want {
		t.Fatalf("herstelvolgorde = %q, wil %q", got, want)
	}
	if err := h.RecoveryNeeded(); err != nil {
		t.Fatalf("controller bleef poisoned na volledig herstel: %v", err)
	}
}

func TestRecoverStartFailureStaysPoisonedAndCanRetry(t *testing.T) {
	h := &HC{
		Name:     "test",
		dmaBase:  0x120000,
		dmaSize:  0x200000,
		poisoned: errors.New("disable slot completion unknown"),
		running:  true,
	}

	resets, starts, powers := 0, 0, 0
	start := func(base, size uintptr) error {
		starts++
		if starts == 1 {
			return errors.New("event ring setup failed")
		}
		return nil
	}
	reset := func() error {
		resets++
		return nil
	}
	power := func() { powers++ }

	if err := h.recoverWith(reset, start, power); err == nil {
		t.Fatal("eerste Start-fout werd geaccepteerd")
	}
	if h.RecoveryNeeded() == nil {
		t.Fatal("Start-fout maakte controller ten onrechte gezond")
	}
	if h.running {
		t.Fatal("Start-fout liet running staan")
	}
	if powers != 0 {
		t.Fatalf("PowerOn aangeroepen na mislukte Start: %d", powers)
	}

	if err := h.recoverWith(reset, start, power); err != nil {
		t.Fatalf("tweede herstelpoging: %v", err)
	}
	if resets != 2 || starts != 2 || powers != 1 {
		t.Fatalf("herstelcalls reset/start/power = %d/%d/%d, wil 2/2/1", resets, starts, powers)
	}
	if h.RecoveryNeeded() != nil {
		t.Fatalf("controller bleef poisoned na retry: %v", h.RecoveryNeeded())
	}
}

func TestRecoverResetFailureDoesNotStart(t *testing.T) {
	want := errors.New("HCRST timeout")
	h := &HC{
		Name:     "test",
		dmaBase:  0x120000,
		dmaSize:  0x200000,
		poisoned: errors.New("disable slot completion unknown"),
	}
	started, powered := false, false
	err := h.recoverWith(
		func() error { return want },
		func(uintptr, uintptr) error {
			started = true
			return nil
		},
		func() { powered = true },
	)
	if err == nil || !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("Reset-fout = %v, wil %v", err, want)
	}
	if started || powered {
		t.Fatalf("na Reset-fout: started=%v powered=%v", started, powered)
	}
	if h.RecoveryNeeded() == nil {
		t.Fatal("Reset-fout maakte controller ten onrechte gezond")
	}
}

func TestRecoverHealthyControllerDoesNothing(t *testing.T) {
	h := &HC{dmaBase: 0x120000, dmaSize: 0x200000}
	called := false
	if err := h.recoverWith(
		func() error { called = true; return nil },
		func(uintptr, uintptr) error { called = true; return nil },
		func() { called = true },
	); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("gezonde controller werd gereset")
	}
}
