package vitals

// De idle-meting is passief en loopt altijd: idle-gedrag wil je juist zien
// als er verder niets gebeurt, dus een "test" die je start zou zijn eigen
// meting verpesten. De sampler leest elke 2s de twee tellerwoorden die applib
// op de control-page publiceert en die verder niemand leest (HOP leest
// CtrlWakes bewust niet — besluit 06-08, zie abi/layout):
//
//	CtrlIdle   geaccumuleerde idle-TIJD in counter-ticks
//	CtrlWakes  aantal idle-rondes van de scheduler
//
// Daaruit volgt over een venster: de idle-fractie (ΔIdle / (Δwand · Hz)), het
// wektempo (ΔWakes / Δwand) en — de rekensom uit layout.go — de kosten per
// wake: (1 − idle-fractie) / wekken-per-seconde. Let op: onder QEMU-TCG is
// WFE een no-op en zijn idle-cijfers betekenisloos; dit zijn ijzer-cijfers.

import (
	"sync"
	"time"
)

const (
	sampleEvery = 2 * time.Second
	ringSize    = 512 // ~17 min historie; genoeg voor elk venster op de pagina
)

type idleSample struct {
	t     time.Time
	idle  uint64 // CtrlIdle: idle-ticks
	wakes uint64 // CtrlWakes: idle-rondes
}

type idleSampler struct {
	mu   sync.Mutex
	ring []idleSample
	cfg  Config
}

// idleWindow is het JSON-vensterresultaat. IdlePct is -1 als de
// tellerfrequentie onbekend is (host-build, of board zonder Hz).
type idleWindow struct {
	OK         bool    `json:"ok"`
	SpanS      float64 `json:"span_s"`
	IdlePct    float64 `json:"idle_pct"`
	WakesPerS  float64 `json:"wakes_per_s"`
	WakeCostUS float64 `json:"wake_cost_us"`
}

func (i *idleSampler) start(cfg Config) {
	i.cfg = cfg
	if cfg.CtrlRead == nil {
		return // host-build: geen control-page, geen sampler
	}
	go func() {
		for {
			s := idleSample{
				t:     time.Now(),
				idle:  cfg.CtrlRead(cfg.Offsets.Idle),
				wakes: cfg.CtrlRead(cfg.Offsets.Wakes),
			}
			i.mu.Lock()
			i.ring = append(i.ring, s)
			if len(i.ring) > ringSize {
				i.ring = i.ring[len(i.ring)-ringSize:]
			}
			i.mu.Unlock()
			time.Sleep(sampleEvery)
		}
	}()
}

// window rekent de idle-cijfers uit over (ten hoogste) de laatste d.
func (i *idleSampler) window(d time.Duration) idleWindow {
	i.mu.Lock()
	defer i.mu.Unlock()
	if len(i.ring) < 3 {
		return idleWindow{}
	}
	last := i.ring[len(i.ring)-1]
	base := i.ring[0]
	cutoff := last.t.Add(-d)
	for k := len(i.ring) - 1; k >= 0; k-- {
		if i.ring[k].t.Before(cutoff) {
			break
		}
		base = i.ring[k]
	}
	span := last.t.Sub(base.t).Seconds()
	if span < 4 {
		return idleWindow{}
	}

	w := idleWindow{OK: true, SpanS: span, IdlePct: -1}
	dw := last.wakes - base.wakes
	w.WakesPerS = float64(dw) / span

	var hz uint64
	if i.cfg.CounterHz != nil {
		hz = i.cfg.CounterHz()
	}
	if hz == 0 {
		return w
	}
	frac := float64(last.idle-base.idle) / (span * float64(hz))
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	w.IdlePct = frac * 100
	if dw > 0 {
		// De rekensom uit abi/layout: tijd per wek = (1 − idle) / wekken·s⁻¹.
		w.WakeCostUS = (1 - frac) / w.WakesPerS * 1e6
	}
	return w
}
