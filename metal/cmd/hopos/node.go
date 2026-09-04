package main

import (
	"fmt"
	"github.com/xinix00/HopOS/metal/v2/net/hopswitch"
	"runtime"
	"time"

	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/cpu/idle"
	"github.com/xinix00/HopOS/metal/v2/cpu/irq"
	"github.com/xinix00/HopOS/metal/v2/kern/slots"
	"github.com/xinix00/HopOS/metal/v2/net/hopnet"
)

// Wat de agent-main over zijn eigen cores moet weten, arch-neutraal: hoe HOP
// een éigen extra core opbrengt en hoe die op de console heet. Hier stonden
// twee helften (node_arm64.go / node_riscv64.go) met elk hun eigen vocabulaire
// — CPUOn/AffinityInfo tegenover HartOn/HartState; sinds board.Cores is het
// één contract en dus één bestand. De boot-weigering (Privilege) en de
// firmware-regel (Firmware) vraagt de main rechtstreeks aan het board.

// nodeDispatch is hoe HOP een éigen extra core opbrengt: rechtstreeks via de
// Start-poot van het board (hij ís HOP — er is geen HOP-boven-HOP die op
// CtrlSMPReq luistert), naar de gedeelde EL2-trampoline die smp.ConfigureNode
// meegeeft. Dezelfde poot als een app-core krijgt in kern/slots.
func nodeDispatch(core int, entry, ctx uint64) {
	k := board.Current().Cores()
	phys, ok := k.Phys(core)
	if !ok {
		fmt.Printf("hop: node-core %d: no such core on this board\n", core)
		return
	}
	if err := k.Start(phys, entry, ctx); err != nil {
		fmt.Printf("hop: node-core %d: %v\n", core, err)
	}
}

// nodeCoreState is de console-weergave van een opgekomen node-core.
func nodeCoreState(core int) string {
	k := board.Current().Cores()
	phys, ok := k.Phys(core)
	if !ok {
		return "no such core"
	}
	return fmt.Sprintf("state=%s", k.State(phys))
}

// idleStat is de meetlat van HOP's eigen core: hoe vaak zijn scheduler per
// seconde wakker wordt en welk deel van de tijd hij slaapt — hetzelfde paar
// dat een app op zijn control-page publiceert (CtrlWakes/CtrlIdle), maar HOP
// heeft geen control-page. Aan met hopos.idlestat=1; één regel per 10s.
// Dít is het getal waarop de NIC-interrupt (cpu/irq) afgerekend wordt:
// gepold ~3.300 wekken/s bij stilte, op de interrupt ~100 (de vangrail).
func idleStat() {
	hz := float64(idle.CounterHz())
	w0, t0, i0, r0, at := idle.Wakes(), idle.Ticks(), irq.Fired(), hopnet.RXIdleRounds(), time.Now()
	wr0, wa0, wk0 := slots.WakerStats()
	dk0 := slots.DirectRXKicks()
	sw0 := idle.WorkWoken.Load()
	nr0, wi0 := idle.WorkNotReady.Load(), idle.WorkIdle.Load()
	s0 := slots.EL2Sleeps()
	for {
		time.Sleep(10 * time.Second)
		w1, t1, i1, r1, now := idle.Wakes(), idle.Ticks(), irq.Fired(), hopnet.RXIdleRounds(), time.Now()
		wr1, wa1, wk1 := slots.WakerStats()
		dk1 := slots.DirectRXKicks()
		sw1 := idle.WorkWoken.Load()
		nr1, wi1 := idle.WorkNotReady.Load(), idle.WorkIdle.Load()
		s1 := slots.EL2Sleeps()
		dt := now.Sub(at).Seconds()
		fmt.Printf("idle: %.0f wakes/s, %.1f%% idle, %.0f irq/s, %.0f empty rx rounds/s; waker %.0f rounds/s, asleep %.0f/s, fallback kicks %.0f/s, direct-rx %.0f/s; switch wakes %.0f/s (not-ready %.0f/s, idle %.0f/s); app cores %.0f el2 sleeps/s HOPOS_IDLESTAT\n",
			float64(w1-w0)/dt, 100*float64(t1-t0)/hz/dt, float64(i1-i0)/dt, float64(r1-r0)/dt,
			float64(wr1-wr0)/dt, float64(wa1-wa0)/dt, float64(wk1-wk0)/dt, float64(dk1-dk0)/dt,
			float64(sw1-sw0)/dt, float64(nr1-nr0)/dt, float64(wi1-wi0)/dt, float64(s1-s0)/dt)
		fmt.Printf("idle: cores%s\n", slots.CoreDump())
		ts := hopnet.Stats()
		fmt.Printf("idle: hop tcp retrans %d, fast %d, persist %d, zero-window %d; segs out %d (%d B) in %d (%d B); drops bad %d short %d noport %d replyfull %d\n", ts.TCPRetransmits, ts.TCPFastRetransmits, ts.TCPPersistProbes, ts.TCPZeroWindows, ts.TCPSegsOut, ts.TCPBytesOut, ts.TCPSegsIn, ts.TCPBytesIn, ts.DropBadFrame, ts.DropShortFrame, ts.DropNoPort, ts.DropReplyFull)
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		fmt.Printf("idle: switch work by door %d, by failsafe timer %d; rx full %d, rx drops %d, nat oversize %d; hop gc %d\n", hopswitch.WorkByDoor.Load(), hopswitch.WorkByTimer.Load(), hopswitch.RXFull.Load(), hopswitch.RXDrops.Load(), hopswitch.NATOversize.Load(), ms.NumGC)

		w0, t0, i0, r0, at = w1, t1, i1, r1, now
		wr0, wa0, wk0, dk0, sw0, nr0, wi0, s0 = wr1, wa1, wk1, dk1, sw1, nr1, wi1, s1
	}
}
