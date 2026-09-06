package main

import (
	"github.com/xinix00/HopOS/metal/v2/board"
	"testing"
	"time"
)

// Named-file host test: watchdog.go + watchdog_policy_test.go. The actual
// main.go boot/config machinery is deliberately outside this unit.
func bootParam(string) string { return "" }

func watchdogFixture(t *testing.T) (*int, *uint64) {
	t.Helper()
	oldHW, oldDeadline, oldEarly, oldCounter := nodeWDT, flipBootDeadline, earlyPet, bootGuardCounter
	calls := new(int)
	ticks := new(uint64)
	nodeWDT = &wdHardware{Pet: func() { *calls++ }, PetEvery: time.Millisecond, Arm: func() (string, bool) { return "test", true }}
	flipBootDeadline = 0
	earlyPet = nil
	bootGuardCounter = func() uint64 { return *ticks }
	t.Cleanup(func() {
		nodeWDT, flipBootDeadline, earlyPet, bootGuardCounter = oldHW, oldDeadline, oldEarly, oldCounter
	})
	return calls, ticks
}
func TestBlindPetsShareOneFlipDeadline(t *testing.T) {
	calls, ticks := watchdogFixture(t)
	flipBootDeadline = 120000
	*ticks = 60000
	if !petBootGuard() {
		t.Fatal("early pet refused")
	}
	earlyPet = make(chan struct{})
	stopped := earlyPet
	stopBootGuard()
	select {
	case <-stopped:
	default:
		t.Fatal("early worker not stopped")
	}
	if flipBootDeadline != 120000 {
		t.Fatal("handover renewed grace")
	}
	*ticks = 119999
	if !petBootGuard() {
		t.Fatal("phase1 pet refused")
	}
	*ticks = 120000
	if petBootGuard() {
		t.Fatal("pet at deadline")
	}
	*ticks = 999999
	if petBootGuard() || *calls != 2 {
		t.Fatal("pet after deadline")
	}
}
func TestBootGuardUsesRawCounterDespiteClockEpochChange(t *testing.T) {
	calls, ticks := watchdogFixture(t)
	*ticks = 24000000
	nodeWDT.PetEvery = time.Hour // no worker tick while this deterministic test runs
	armBootGuard(24000000)
	stopBootGuard()
	want := uint64(24000000 + 120*24000000)
	if flipBootDeadline != want {
		t.Fatalf("deadline=%d want raw ticks %d", flipBootDeadline, want)
	}
	// A date-sized clock offset would instantly expire the former time.Now
	// deadline. It must never be incorporated into the raw counter budget.
	epochOffset := uint64(time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC).UnixNano())
	if epochOffset <= want {
		t.Fatal("invalid clock-shift fixture")
	}
	if !petBootGuard() || flipBootDeadline != want {
		t.Fatal("unchanged raw counter lost its grace")
	}
	*ticks = want - 1
	if !petBootGuard() {
		t.Fatal("raw grace shortened")
	}
	*ticks = want
	if petBootGuard() || *calls != 2 {
		t.Fatal("raw deadline not enforced")
	}
}
func TestColdBootBlindPetRemainsUnlimited(t *testing.T) {
	calls, ticks := watchdogFixture(t)
	*ticks = 1
	if !petBootGuard() {
		t.Fatal("cold pet refused")
	}
	*ticks = 1 << 63
	if !petBootGuard() || *calls != 2 {
		t.Fatal("cold bring-up lost unlimited grace")
	}
}
func TestExpiredFlipCanaryDoesNotRearm(t *testing.T) {
	calls, ticks := watchdogFixture(t)
	arms := 0
	nodeWDT.Arm = func() (string, bool) { arms++; return "test", true }
	flipBootDeadline = 100
	*ticks = 100
	nodeCanary()
	if arms != 0 || *calls != 0 {
		t.Fatalf("expired handover rearmed/petted: arm=%d pet=%d", arms, *calls)
	}
}

type watchdogNoIPBoard struct{ board.Board }

func (watchdogNoIPBoard) Net() board.NetConfig { return board.NetConfig{} }
func TestFlipCanaryWithoutAgentStopsBlindPets(t *testing.T) {
	calls, _ := watchdogFixture(t)
	old := board.Current()
	board.Use(watchdogNoIPBoard{})
	t.Cleanup(func() { board.Use(old) })
	raw := uint64(0)
	bootGuardCounter = func() uint64 { raw++; return raw }
	flipBootDeadline = 4
	nodeCanary()
	before := *calls
	if raw < 4 || petBootGuard() || *calls != before {
		t.Fatal("failed canary kept petting")
	}
}
func TestIntentionalRebootStopsEarlyPets(t *testing.T) {
	watchdogFixture(t)
	earlyPet = make(chan struct{})
	stopped := earlyPet
	marker := new(int)
	nodeWDT.Reboot = func() {
		select {
		case <-stopped:
		default:
			t.Fatal("reboot left early pet worker running")
		}
		if earlyPet != nil {
			t.Fatal("reboot retained early owner")
		}
		panic(marker) // emulate non-returning hardware reset, without hardware
	}
	defer func() {
		if got := recover(); got != marker {
			t.Fatalf("unexpected reset result: %v", got)
		}
	}()
	rebootNow()
}
