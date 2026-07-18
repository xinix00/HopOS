// usage.go: per-slot CPU-benutting uit de idle-teller — Dereks vraag
// (18-07): "kunnen we CPU per app achterhalen door te zien hoe idle hij
// runt?" Ja, en de meting bestond al: elke app publiceert zijn idle-TIJD
// (generic-timer-ticks, metal/cpu/idle) op de control-page (CtrlIdle) —
// een idle core accumuleert ~idle.CounterHz per seconde, een rekenende
// core staat stil. Het dvfs-klokbeleid op de Pi leest ditzelfde signaal op
// 10ms voor de flank; hier middelt een 5s-venster het tot een rapportage-
// cijfer. Geen app-ABI-wijziging: alleen een lezer erbij.
//
// De uitkomst is een percentage van de ÉÍGEN cores van het slot (0..100,
// SMP-genormaliseerd via CtrlCores) — precies de vorm die HOP's monitor als
// cpu_percent doorgeeft, zoals docker dat voor containers doet.

//go:build tamago

package slotmgr

import (
	"sync"
	"sync/atomic"
	"time"

	"hop-os/metal/abi/layout"
	"hop-os/metal/cpu/idle"
	"hop-os/metal/kern/slots"
)

const usageSample = 5 * time.Second

// usagePct[i] = laatste meting voor interne slot i; −1 = (nog) onbekend —
// slot leeg, eerste ijk-ronde, of een node zonder bruikbare CNTFRQ.
// SlotCap-gedimensioneerd (compile-time): MaxSlots is pas ná board-discovery
// definitief, de lus leest hem per ronde vers.
var usagePct [layout.SlotCap + 1]atomic.Int32

var usageOnce sync.Once

// startUsage begint de meting — vanuit New, dus pas als er echt een manager
// komt (en niet in een init die vóór de board-discovery valt).
func startUsage() {
	for i := range usagePct {
		usagePct[i].Store(-1)
	}
	go usageLoop()
}

// cpuPct geeft de meting voor een interne slot-index.
func cpuPct(i int) int {
	if i < 1 || i >= len(usagePct) {
		return -1
	}
	return int(usagePct[i].Load())
}

// usageLoop is het dvfs-sample-patroon (last/seen/eerst-ijken): delta's van
// de teller tegen het verwachte tempo. Draait als OS-taak op de HOP-core;
// ≤127 device-reads per 5s is ruis.
func usageLoop() {
	tickHz := idle.CounterHz()
	if tickHz == 0 {
		return // geen bruikbare CNTFRQ: dan geen cpu-cijfer — nooit een blokker
	}
	last := make([]uint64, layout.SlotCap+1)
	seen := make([]bool, layout.SlotCap+1)
	prev := time.Now()
	for {
		time.Sleep(usageSample)
		now := time.Now()
		expect := tickHz * uint64(now.Sub(prev)) / uint64(time.Second) // per core
		prev = now
		if expect == 0 {
			continue
		}
		for i := slots.HopReserved() + 1; i <= layout.MaxSlots; i++ {
			s := slots.Get(i)
			if !s.CoreOn || s.Cores == 0 {
				seen[i] = false
				usagePct[i].Store(-1)
				continue
			}
			n := s.IdleTicks
			if !seen[i] || n < last[i] {
				// Eerste ronde van deze huurder (of de page is geveegd
				// door een herstart): alleen ijken, nog geen cijfer.
				seen[i], last[i] = true, n
				usagePct[i].Store(-1)
				continue
			}
			d := n - last[i]
			last[i] = n
			full := expect * s.Cores // verwachte tikken bij volledig idle
			pct := int32(0)
			if d < full {
				pct = int32((full - d) * 100 / full)
			}
			// d ≥ full klemt op 0. QEMU-TCG heeft geen idle-model (WFE =
			// no-op → idle-tijd ≈ 0 → alles leest daar hoog); cpu% is een
			// ijzer-cijfer, net als alle cache/klok-metingen.
			usagePct[i].Store(pct)
		}
	}
}
