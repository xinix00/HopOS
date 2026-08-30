//go:build qemuvirt

package main

// schedbench.go — de meetbank voor de core-deling. Geen regressie maar een
// INSTRUMENT: hij zet de standen van de RX-slaap tegen elkaar in één boot, op
// dezelfde machine, met dezelfde apps. Aan te zetten met image/qemu-run.sh
// bench (-X main.benchMode=1); zonder die vlag draait de demo zoals altijd.
//
// Wat er gemeten wordt, en waarom precies dit:
//
//   - RTT tussen twee bewoners van ÉÉN core (slot 1 echo, slot 2 ping). Elke
//     round-trip is minstens twee wissels, dus dit getal ís de hop-latency
//     zoals een app hem voelt.
//   - Daarna een STIL venster: wekken/s per slot. Dat is wat "niets doen"
//     kost, en op een gedeelde core is elke wek een volledige context-wissel.
//
// LET OP bij het lezen (QEMU-TCG): WFE is hier een no-op, dus de EL2-rotatie
// spint warm als niemand due is en het cpu-percentage is daardoor ruis. De
// WEKKEN zijn wél hard — die volgen uit de timers van de app in gasttijd — en
// de RTT is een verhoudingsgetal: absolute waarden zijn TCG-traag, de
// onderlinge verhouding is de uitspraak. Het oordeel valt op ijzer.

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/cpu/idle"
	"github.com/xinix00/HopOS/metal/kern/slots"
)

// benchMode wordt door de linker gezet (-X main.benchMode=1).
var benchMode string

// benchRounds: de standen die we vergelijken. De eerste is de stand van
// vandaag — elke meting moet tegen de bestaande code kunnen worden gelegd.
var benchRounds = []struct{ rxpoll, what string }{
	{"300us", "300us vast (de oude default, ontsnappingsklep)"},
	{"", "300us→1s:4 (de default sinds de doorbell)"},
	// Met de doorbell (idle/rxdoor.go + de rotatie-peek) is de cap alleen nog
	// de bodem voor als de bel ooit dooft — de wek loopt via de drempel op de
	// control-page. Een reusachtige cap hoort dus dezelfde RTT te meten als
	// de 300µs-poll, bij een handvol wekken per seconde. Vóór de doorbell
	// (gemeten 29-08) was deze stand: koud p50 157ms bij cap 100ms.
	{"300us:10s:4", "doorbell: cap 10s — de bel draagt de latency"},
}

func schedBench() {
	fmt.Println("")
	fmt.Println("SCHEDBENCH: slot 1 (echo) en slot 2 (ping) op ÉÉN core — RTT + idle-tempo per stand")
	for _, r := range benchRounds {
		benchRound(r.rxpoll, r.what)
	}
	fmt.Println("SCHEDBENCH_DONE")
	for {
		time.Sleep(time.Hour)
	}
}

// benchRound draait één stand: paar starten, RTT laten klokken, dan een stil
// venster meten, dan opruimen.
func benchRound(rxpoll, what string) {
	fmt.Printf("\nSCHEDBENCH ronde: %s (RXPOLL=%q)\n", what, rxpoll)
	stopIfOn("bench-stop", 1, 2)

	peer := fmt.Sprintf("%s:9000", layout.IP4Str(layout.SlotIP4(1)))
	mustStart("bench-echo", 1, 64<<20, 1,
		map[string]string{"BENCH": "echo", "RXPOLL": rxpoll}, nil, nil, nil)
	mustReady("bench-echo", 1, 10*time.Second)
	mustStartShared("bench-ping", 1, 2, 64<<20,
		map[string]string{"BENCH": "ping", "RXPOLL": rxpoll, "BENCH_PEER": peer})
	mustReady("bench-ping", 2, 10*time.Second)
	if !slots.Get(2).Shared || !slots.Get(1).Shared {
		fail("bench-share", fmt.Errorf("slot 1/2 delen geen core (shared=%v/%v)",
			slots.Get(1).Shared, slots.Get(2).Shared))
	}

	// De ping-ronde loopt (heet + koud); daarna wordt slot 2 stil. Ruim
	// wachten is hier goedkoper dan een tweede signaalpad.
	time.Sleep(35 * time.Second)
	benchIdle(5*time.Second, 1, 2)
	stopIfOn("bench-stop", 1, 2)
}

// benchIdle meet een stil venster: wekken/s en de eigen tijd per wek. De
// idle-teller is tijd (counter-ticks), dus (venster − idle)/wekken is wat één
// scheduler-ronde deze app kost.
func benchIdle(d time.Duration, slotNums ...int) {
	type snap struct{ idle, wakes uint64 }
	before := make(map[int]snap, len(slotNums))
	for _, s := range slotNums {
		st := slots.Get(s)
		before[s] = snap{st.IdleTicks, st.Wakes}
	}
	t0 := time.Now()
	time.Sleep(d)
	el := time.Since(t0).Seconds()
	hz := float64(idle.CounterHz())

	for _, s := range slotNums {
		st := slots.Get(s)
		wakes := float64(st.Wakes-before[s].wakes) / el
		busy := el - float64(st.IdleTicks-before[s].idle)/hz
		per := 0.0
		if wakes > 0 {
			per = busy / (wakes * el) * 1e6
		}
		fmt.Printf("SCHEDBENCH_IDLE slot=%d wakes/s=%.0f cpu=%.1f%% us-per-wake=%.1f\n",
			s, wakes, 100*busy/el, per)
	}
}
