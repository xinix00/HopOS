// Package dvfs is HopOS' klokbeleid — een OS-taak, geen HOP-taak: de
// orchestrator is volledig oblivious (zelfde principe als bij SMP). Het
// beleid is met Derek vastgelegd in docs/plan-p2b-soak.md (2026-07-11):
//
//   - het signaal is de idle-tik-teller (metal/cpu/idle): een idle core tikt
//     op event-stream-tempo (~1,2ms), een drukke core tikt niet — apps
//     publiceren hem op hun control-page (CtrlIdle), de HOP-core telt
//     intern (idle.Ticks);
//   - de wachter sampelt elke ~10ms: íéts onder tempo → klok DIRECT vol
//     (~10ms schakeltijd); álles ~30s op vol tempo → klok laag;
//   - de firmware-mailbox-call (metal/driver/vcmail) alleen op de flank;
//     de firmware-throttle op 85°C blijft het vangnet.
//
// Long-running services die op requests wachten slapen in timers/polls →
// hoog tiktempo → laag geklokt; het eerste echte werk stalt de teller en
// klokt de node binnen ~10ms op. Een liegende app kost stroom, geen
// isolatie. Telemetrie (temp/klok) hoort bij dit beleid en logt hier.
package dvfs

import (
	"fmt"
	"time"

	"hop-os/metal/abi/layout"
	"hop-os/metal/cpu/idle"
	"hop-os/metal/dev"
	"hop-os/metal/driver/vcmail"
)

// Config is de board-invoer voor Start.
type Config struct {
	Mbox    *vcmail.Mbox // de firmware-mailbox van dit board
	LowHz   uint32       // de "stil"-klok (bv. 600MHz)
	TickHz  uint64       // verwacht tiktempo per idle core = CNTFRQ / 65536
	Slots   int          // layout.MaxSlots (aantal te bewaken control-pages)
	Verbose bool         // true: log elke flank (soak-diagnose)
}

// Het "vol"-plafond is GEEN veld hier: dvfs volgt gewoon het firmware-maximum
// (MaxClockRate). Een thermische cap zet je in config.txt met arm_freq_max —
// de firmware rapporteert dat dan als max en dvfs pakt het vanzelf op (nul
// code). Zo bleef een fanloze Pi 5 onder de 85°C-throttle in de 24u-soak
// (2026-07-11: 2400MHz liep binnen minuten naar 84°C, arm_freq_max=1800 niet).

const (
	sample   = 10 * time.Millisecond // reactietijd omhoog
	cooldown = 30 * time.Second      // hysterese omlaag
	telemetr = 60 * time.Second      // telemetrie-interval
	// busyFrac: onder dit deel van het verwachte tempo geldt een bron als
	// druk (70% — ruim onder de event-stream-jitter, ruim boven "half werk").
	busyNum, busyDen = 7, 10
)

// Start meet het firmware-maximum, zet de node op "stil" (er draait nog
// niets) en start de wachter-goroutine. Faalt de mailbox, dan wordt er
// alleen gelogd — de node draait dan gewoon op de firmware-klok.
func Start(cfg Config) {
	max, ok := cfg.Mbox.MaxClockRate(vcmail.ClockARM)
	if !ok {
		fmt.Println("dvfs: mailbox not responding — clock policy disabled (staying on firmware default)")
		return
	}
	// De firmware klemt op zijn eigen minimum (GEMETEN 2026-07-11, Pi 5:
	// SetClockRate(600M) werd stilzwijgend de 1500MHz-arm_freq_min-vloer) —
	// dus de stil-stand daarop klemmen en het eerlijk melden.
	if min, ok := cfg.Mbox.MinClockRate(vcmail.ClockARM); ok && min > cfg.LowHz {
		cfg.LowHz = min
	}
	cur, _ := cfg.Mbox.ClockRate(vcmail.ClockARM)
	fmt.Printf("dvfs: ARM %d MHz (firmware min/max %d/%d) — policy: clock follows idle, quiet floor %d MHz\n",
		cur/1_000_000, cfg.LowHz/1_000_000, max/1_000_000, cfg.LowHz/1_000_000)
	go watch(cfg, max)
}

// watch is de wachter: samplen, flanken schakelen, telemetrie.
func watch(cfg Config, maxHz uint32) {
	last := make([]uint64, layout.MaxSlots+1) // [0] = HOP-core, [1..] = slots
	seen := make([]bool, layout.MaxSlots+1)   // eerste sample per actief slot = ijken
	quiet := time.Now()                       // sinds wanneer alles idle is
	lastTele := time.Now()

	set := func(hz uint32, why string) {
		if actual, ok := cfg.Mbox.SetClockRate(vcmail.ClockARM, hz); ok {
			if cfg.Verbose {
				fmt.Printf("dvfs: → %d MHz (%s)\n", actual/1_000_000, why)
			}
		} else {
			fmt.Println("dvfs: SetClockRate failed — skipping this transition")
		}
	}

	// Toestand niet aannemen maar zetten (GEMETEN 2026-07-11: met een
	// arm_freq_min-vloer boot de firmware op de vloer, niet op vol — de hele
	// P1-acceptatie draaide per ongeluk op 800MHz): boot-werk verdient de
	// volle klok, daarna regeert het beleid.
	high := true
	set(maxHz, "boot")

	for {
		time.Sleep(sample)
		expect := cfg.TickHz * uint64(sample) / uint64(time.Second) // per core, per sample

		busy := false
		// Bron 0: de HOP-core zelf (agent-drukte klokt ook op).
		n := idle.Ticks()
		if d := n - last[0]; d*busyDen < expect*busyNum {
			busy = true
		}
		last[0] = n
		// Bronnen 1..MaxSlots: actieve app-slots (CtrlIdle op hun page;
		// CtrlCores deelt het verwachte tempo bij SMP). Het eerste sample
		// na een start ijkt alleen — daarna telt óók een teller die op 0
		// blijft staan (een app die vanaf seconde één 100% brandt) als druk.
		for i := 1; i <= cfg.Slots; i++ {
			page := layout.CtrlPagePA(i)
			cores := dev.Read64(page + layout.CtrlCores)
			if cores == 0 || dev.Read64(page+layout.CtrlStatus) != layout.StatusReady {
				seen[i] = false
				continue
			}
			n := dev.Read64(page + layout.CtrlIdle)
			if seen[i] {
				if d := n - last[i]; d*busyDen < expect*cores*busyNum {
					busy = true
				}
			}
			seen[i] = true
			last[i] = n
		}

		switch {
		case busy && !high:
			high = true
			set(maxHz, "busy")
		case busy:
			quiet = time.Now()
		case high && time.Since(quiet) > cooldown:
			high = false
			set(cfg.LowHz, "idle 30s")
		}

		if time.Since(lastTele) >= telemetr {
			lastTele = time.Now()
			mC, _ := cfg.Mbox.Temp()
			hz, _ := cfg.Mbox.ClockRate(vcmail.ClockARM)
			fmt.Printf("dvfs: telemetry — %d.%d°C, ARM %d MHz, state=%s\n",
				mC/1000, mC%1000/100, hz/1_000_000, map[bool]string{true: "full", false: "quiet"}[high])
		}
	}
}
