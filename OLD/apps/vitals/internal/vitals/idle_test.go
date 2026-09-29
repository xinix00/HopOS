package vitals

import (
	"testing"
	"time"
)

func idleTestSample(seconds int, idle, wakes, cores uint64) idleSample {
	return idleSample{t: time.Unix(int64(seconds), 0), idle: idle, wakes: wakes, cores: cores}
}

func idleTestWindow(samples ...idleSample) idleWindow {
	i := idleSampler{cfg: Config{CounterHz: func() uint64 { return 100 }}, ring: samples}
	return i.window(time.Minute)
}

func TestIdleSingleCore(t *testing.T) {
	for _, cores := range []uint64{0, 1} {
		w := idleTestWindow(
			idleTestSample(0, 0, 0, cores),
			idleTestSample(2, 50, 2000, cores),
			idleTestSample(4, 100, 4000, cores),
		)
		if !w.OK || w.IdlePct != 25 || w.WakesPerS != 1000 || w.WakeCostUS != 750 || w.IdleNote != "" {
			t.Fatalf("cores=%d: %+v", cores, w)
		}
	}
}

func TestIdleSMPDoesNotClampSummedSleepTo100(t *testing.T) {
	w := idleTestWindow(
		idleTestSample(0, 0, 0, 2),
		idleTestSample(2, 300, 3000, 2),
		idleTestSample(4, 600, 6000, 2),
	)
	// Six core-seconds over four wall-seconds formerly became 100% idle.
	// Even dividing by two omits uncounted waits, so retain only wake rate.
	if !w.OK || w.IdlePct != -1 || w.WakesPerS != 1500 || w.WakeCostUS != 0 || w.IdleNote == "" {
		t.Fatalf("unsupported SMP idle percentage: %+v", w)
	}
}

func TestIdleWindowStartsAfterCounterReset(t *testing.T) {
	for _, reset := range []string{"idle", "wakes"} {
		t.Run(reset, func(t *testing.T) {
			first := idleTestSample(0, 0, 0, 1)
			before := idleTestSample(2, 0, 0, 1)
			if reset == "idle" {
				before.idle = 80
			} else {
				before.wakes = 3000
			}
			w := idleTestWindow(first, before,
				idleTestSample(4, 0, 0, 1),
				idleTestSample(6, 50, 2000, 1),
				idleTestSample(8, 100, 4000, 1),
			)
			// The final counter exceeds the original value: checking only the
			// endpoints would miss the reset inside the observation window.
			if !w.OK || w.SpanS != 4 || w.IdlePct != 25 || w.WakesPerS != 1000 {
				t.Fatalf("reset included in measurement: %+v", w)
			}
		})
	}
}

func TestIdleRecentResetWaitsForFreshWindow(t *testing.T) {
	w := idleTestWindow(
		idleTestSample(0, 100, 100, 1),
		idleTestSample(2, 200, 200, 1),
		idleTestSample(4, 0, 0, 1),
		idleTestSample(6, 50, 2000, 1),
	)
	if w.OK {
		t.Fatalf("used only two seconds after reset: %+v", w)
	}
}

func TestIdleCoreChangeStartsNewWindow(t *testing.T) {
	w := idleTestWindow(
		idleTestSample(0, 0, 0, 2),
		idleTestSample(2, 300, 3000, 2),
		idleTestSample(4, 500, 5000, 1),
		idleTestSample(6, 550, 7000, 1),
		idleTestSample(8, 600, 9000, 1),
	)
	if !w.OK || w.SpanS != 4 || w.IdlePct != 25 || w.WakesPerS != 1000 {
		t.Fatalf("mixed core counts in measurement: %+v", w)
	}
}
