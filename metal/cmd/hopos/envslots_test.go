package main

import (
	"github.com/xinix00/hop/pkg/hopos"
	"testing"
)

type placementHintSlots struct {
	hopos.SlotManager
	primary, width int
}

func (s *placementHintSlots) CanPlaceDedicated(slot, cores int) bool {
	s.primary, s.width = slot, cores
	return slot == 8 && cores == 2 // pool owns core 7, SMP can use 8..9
}
func TestEnvSlotsPreservesPhysicalPlacementHint(t *testing.T) {
	inner := &placementHintSlots{}
	var wrapped hopos.SlotManager = envSlots{SlotManager: inner}
	hint, ok := wrapped.(hopos.DedicatedPlacement)
	if !ok {
		t.Fatal("environment adapter hid physical placement query")
	}
	if hint.CanPlaceDedicated(7, 2) {
		t.Fatal("adapter accepted pool-owned core 7")
	}
	if inner.primary != 7 || inner.width != 2 {
		t.Fatal("adapter changed placement arguments")
	}
	if !hint.CanPlaceDedicated(8, 2) {
		t.Fatal("adapter hid free SMP run 8..9")
	}
	if inner.primary != 8 || inner.width != 2 {
		t.Fatal("adapter changed placement arguments")
	}
}
func TestEnvSlotsWithoutPlacementHintDefersToStartStream(t *testing.T) {
	wrapped := envSlots{SlotManager: struct{ hopos.SlotManager }{}}
	if !wrapped.CanPlaceDedicated(1, 2) {
		t.Fatal("missing optional hint must leave final decision to StartStream")
	}
}
