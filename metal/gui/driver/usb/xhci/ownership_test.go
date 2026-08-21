//go:build gui

package xhci

import (
	"errors"
	"strings"
	"testing"
)

func ownershipHC(n int) *HC {
	res := make([]*slotRes, n+1)
	for i := 1; i <= n; i++ {
		res[i] = &slotRes{}
	}
	return &HC{
		Name:   "test",
		nSlots: n,
		res:    res,
	}
}

func TestReleaseSlotClearsOwnershipOnlyAfterConfirmedDisable(t *testing.T) {
	h := ownershipHC(2)
	h.res[1].inUse = true
	table := []uint64{0, 0xfeed000, 0}

	if err := h.releaseSlotWith(1, func(slot int) error {
		if slot != 1 {
			t.Fatalf("Disable Slot kreeg %d, wil 1", slot)
		}
		return nil
	}, func(slot int) {
		table[slot] = 0
	}); err != nil {
		t.Fatal(err)
	}
	if h.res[1].inUse || h.res[1].quarantined {
		t.Fatalf("slotstate na bevestigde disable: inUse=%v quarantined=%v",
			h.res[1].inUse, h.res[1].quarantined)
	}
	if table[1] != 0 {
		t.Fatalf("DCBAA[1] = %#x, wil 0", table[1])
	}
	if h.poisoned != nil {
		t.Fatalf("controller ten onrechte poisoned: %v", h.poisoned)
	}
}

func TestReleaseSlotFailureQuarantinesWithoutClearingState(t *testing.T) {
	h := ownershipHC(2)
	h.res[1].inUse = true
	table := []uint64{0, 0xfeed000, 0}
	want := errors.New("completion timeout")

	err := h.releaseSlotWith(1, func(int) error { return want }, func(slot int) {
		table[slot] = 0
	})
	if err == nil || !strings.Contains(err.Error(), "controllerreset vereist") {
		t.Fatalf("release-fout = %v, wil expliciete reset-eis", err)
	}
	if !h.res[1].inUse || !h.res[1].quarantined {
		t.Fatalf("onbevestigd slot werd vergeten: inUse=%v quarantined=%v",
			h.res[1].inUse, h.res[1].quarantined)
	}
	if table[1] != 0xfeed000 {
		t.Fatalf("DCBAA[1] gewist zonder disable-bevestiging: %#x", table[1])
	}
	if h.poisoned == nil {
		t.Fatal("controller niet poisoned na onbevestigde disable")
	}
}

func TestOutOfRangeEnabledSlotIsDisabledOrControllerPoisoned(t *testing.T) {
	t.Run("confirmed cleanup", func(t *testing.T) {
		h := ownershipHC(2)
		disabled := 0
		err := h.claimEnabledSlot(3, func(slot int) error {
			disabled = slot
			return nil
		})
		if err == nil {
			t.Fatal("out-of-range Enable Slot werd geaccepteerd")
		}
		if disabled != 3 {
			t.Fatalf("cleanup disablede slot %d, wil 3", disabled)
		}
		if h.poisoned != nil {
			t.Fatalf("bevestigd opgeruimd afwijkend slot poisonde controller: %v", h.poisoned)
		}
	})

	t.Run("cleanup failure", func(t *testing.T) {
		h := ownershipHC(2)
		err := h.claimEnabledSlot(3, func(int) error { return errors.New("timeout") })
		if err == nil || h.poisoned == nil {
			t.Fatalf("cleanup-fout niet gequarantained: err=%v poisoned=%v", err, h.poisoned)
		}
	})

	t.Run("slot zero cannot be disabled", func(t *testing.T) {
		h := ownershipHC(2)
		called := false
		err := h.claimEnabledSlot(0, func(int) error {
			called = true
			return nil
		})
		if err == nil || h.poisoned == nil {
			t.Fatalf("slot 0 niet gequarantained: err=%v poisoned=%v", err, h.poisoned)
		}
		if called {
			t.Fatal("Disable Slot werd ten onrechte met slot 0 verstuurd")
		}
	})
}

func TestPoisonedControllerRefusesNewAttachBeforeMMIO(t *testing.T) {
	h := ownershipHC(2)
	h.running = true
	h.poisoned = errors.New("slot 1 disable unknown")
	if _, err := h.Attach(1); err == nil || !strings.Contains(err.Error(), "vóór controllerreset") {
		t.Fatalf("Attach op poisoned controller = %v", err)
	}
}
