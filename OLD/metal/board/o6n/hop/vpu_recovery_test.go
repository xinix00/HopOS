package hop

import (
	"errors"
	"github.com/xinix00/HopOS/metal/v2/driver/scmi"
	"reflect"
	"testing"
)

func TestVPURecoveryConditions(t *testing.T) {
	// Register values captured on the O6N before and after recovery.
	if !vpuNeedsRecovery(0x07cef000, [4]uint32{}) {
		t.Fatal("cold boot with closed gates accepted")
	}
	if vpuNeedsRecovery(0x07cefffc, [4]uint32{}) {
		t.Fatal("healthy VPU would be reset")
	}
	for i := 0; i < 4; i++ {
		var term [4]uint32
		term[i] = 1
		if !vpuNeedsRecovery(0x07cefffc, term) {
			t.Fatalf("stuck slot %d missed", i)
		}
	}
}

func TestVPUPowerCycle(t *testing.T) {
	var calls [][2]uint32
	err := cycleVPUDomains(func(domain, state uint32) error { calls = append(calls, [2]uint32{domain, state}); return nil })
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]uint32{{15, scmi.PowerOff}, {14, scmi.PowerOff}, {13, scmi.PowerOff}, {12, scmi.PowerOff}, {11, scmi.PowerOff}, {11, scmi.PowerOn}, {12, scmi.PowerOn}, {13, scmi.PowerOn}, {14, scmi.PowerOn}, {15, scmi.PowerOn}}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("unsafe domain/order: %v", calls)
	}
}

func TestVPUPowerCycleStopsOnFailure(t *testing.T) {
	failure := errors.New("SCMI failed")
	for failAt := 0; failAt < 10; failAt++ {
		calls := 0
		err := cycleVPUDomains(func(domain, state uint32) error {
			calls++
			if calls == failAt+1 {
				return failure
			}
			return nil
		})
		if !errors.Is(err, failure) || calls != failAt+1 {
			t.Fatalf("failure %d: calls=%d err=%v", failAt, calls, err)
		}
	}
}
