// Package dvfs is HopOS' klokbeleid — een OS-taak, geen HOP-taak: de
// orchestrator is volledig oblivious (zelfde principe als bij SMP). Het
// beleid is met Derek vastgelegd in docs/archief/plan-p2b-soak.md (2026-07-11):
//
//   - het signaal is de idle-teller (metal/cpu/idle): idle-TIJD in
//     generic-timer-ticks — een idle core accumuleert ~CNTFRQ per seconde,
//     een drukke core staat stil. Apps publiceren hem op hun control-page
//     (CtrlIdle), de HOP-core telt intern (idle.Ticks);
//   - de wachter sampelt elke ~10ms en oordeelt over de laatste 50ms: íéts
//     onder tempo → klok vol (~20ms bij aanhoudende last, zie history);
//     álles ~30s op vol tempo → klok laag;
//   - de knop (Knob) alleen op de flank: op de Pi de firmware-mailbox
//     (metal/driver/vcmail), op de O6N het _CPC-fastchannel per domein;
//     de firmware-throttle blijft het vangnet.
//
// Long-running services die op requests wachten slapen in timers/polls →
// hoog tiktempo → laag geklokt; het eerste echte werk stalt de teller en
// klokt de node binnen ~10ms op. Een liegende app kost stroom, geen
// isolatie. Telemetrie (temp/klok) hoort bij dit beleid en logt hier.
package dvfs

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/cpu/idle"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/vcmail"
)

// SlotCtrl geeft de fysieke control page van app-slot i, of ok=false als dat
// slot niets draait. Gezet door kern/slots (init): de control page van een slot
// woont in de staart van zijn partitie (layout: de slot-ABI), dus er is geen
// vast adres per slot meer en alleen de slotlaag weet waar hij ligt. Default
// "geen slot" — dan meet de governor alleen de node-core, wat precies de
// situatie is op een node zonder apps.
var SlotCtrl = func(i int) (uintptr, bool) { return 0, false }

// SlotRunning meldt of slot i op dit moment rekent (niet geyield). Een idle-
// teller die in een sample niet steeg telt alleen als druk als dit waar is:
// de teller loopt pas bij als de app uit zijn yield terugkomt, en op een
// yield-idle-board (O6N) slaapt een stille app daar tientallen ms in — zonder
// deze vraag las elke slapende app als 100% bezig en zakte de klok nooit.
// Default "altijd": het oude gedrag, voor boards waar de app in zijn eigen
// WFE slaapt en de teller elke event-stream-tik bijloopt (Pi).
var SlotRunning = func(i int) bool { return true }

// Knob is de klok van één board, teruggebracht tot wat het beleid kent: twee
// standen. Het beleid is overal hetzelfde (de idle-teller bestaat op elk
// board); alleen de knop verschilt — de Pi vraagt de VideoCore-firmware, de
// O6N schrijft per DVFS-domein een perf-woord in het _CPC-fast-channel.
type Knob interface {
	Full() (string, bool)  // naar het plafond; de string is de logregel ("1800 MHz")
	Quiet() (string, bool) // naar de stil-stand; false = mislukt, beleid blijft staan
	Telemetry() string     // temperatuur en klok, voor de minuutregel
}

// Run start de wachter op deze knop. Hij zet eerst zelf "vol" (boot-werk
// verdient de volle klok) en regeert daarna.
func Run(k Knob) {
	active.Store(&k)
	go watch(k)
}

// Meetknop: de console-vraag `clock full|quiet|auto` pint de stand, zodat
// één kern beide standen kan meten zonder flip (het beleid zet de klok
// anders zelf vol zodra de meting begint). Geen config-sleutel: een pin
// hoort niet in een node die draait zonder dat iemand kijkt.
const (
	holdNone = iota
	holdFull
	holdQuiet
)

var (
	hold   atomic.Int32
	active atomic.Pointer[Knob]
	state  atomic.Bool // true = vol

	// De laatste drukke bron, voor de console: 0 = HOP-core, i = slot i. Een
	// governor die niet zakt moet kunnen zeggen wie hem wakker houdt.
	busySrc  atomic.Int32
	busyAt   atomic.Int64 // nanotime van dat sample
	busyIdle atomic.Int32 // idle in promille van het verwachte tempo
)

// Query beantwoordt de console-vraag `clock [full|quiet|auto]`.
func Query(arg string) string {
	kp := active.Load()
	if kp == nil {
		return "clock: no clock policy running on this node"
	}
	switch arg {
	case "full":
		hold.Store(holdFull)
	case "quiet":
		hold.Store(holdQuiet)
	case "auto":
		hold.Store(holdNone)
	case "":
	default:
		return "clock: usage: clock [full|quiet|auto]"
	}
	mode := [...]string{"policy", "held full", "held quiet"}[hold.Load()]
	src := "HOP core"
	if n := busySrc.Load(); n > 0 {
		src = fmt.Sprintf("slot %d", n)
	}
	last := "never"
	if at := busyAt.Load(); at != 0 {
		last = fmt.Sprintf("%s at %d‰ idle, %v ago", src, busyIdle.Load(), time.Since(time.Unix(0, at)).Round(time.Millisecond))
	}
	slotsInfo := ""
	for i := 1; i <= layout.MaxSlots; i++ {
		if page, live := SlotCtrl(i); live {
			slotsInfo += fmt.Sprintf(" [slot %d: idle counter %d, cores %d, status %d, running %v]", i,
				ctrl(page, layout.CtrlIdle), ctrl(page, layout.CtrlCores), ctrl(page, layout.CtrlStatus), SlotRunning(i))
		}
	}
	return fmt.Sprintf("clock: %s — %s, state=%s, last busy sample: %s, counter %d (a change applies within %v)%s",
		mode, (*kp).Telemetry(), map[bool]string{true: "full", false: "quiet"}[state.Load()], last, dev.Counter(), sample, slotsInfo)
}

// Config is de Pi-invoer voor Start. Het verwachte idle-tempo en het
// aantal slots zijn GEEN velden: de teller telt idle-tijd, dus het tempo is
// per definitie idle.CounterHz (CNTFRQ), en de control-pages zijn er
// layout.MaxSlots — parameters met maar één juiste waarde zijn geen
// parameters.
type Config struct {
	Mbox  *vcmail.Mbox // de firmware-mailbox van dit board
	LowHz uint32       // de "stil"-klok (bv. 600MHz)
}

// Het "vol"-plafond is GEEN veld hier: dvfs volgt gewoon het firmware-maximum
// (MaxClockRate). Een thermische cap zet je in config.txt met arm_freq_max —
// de firmware rapporteert dat dan als max en dvfs pakt het vanzelf op (nul
// code). Zo bleef een fanloze Pi 5 onder de 85°C-throttle in de 24u-soak
// (2026-07-11: 2400MHz liep binnen minuten naar 84°C, arm_freq_max=1800 niet).

const (
	sample   = 10 * time.Millisecond // samplerate
	window   = 5                     // samples per oordeel (50ms): zie busyOf
	cooldown = 30 * time.Second      // hysterese omlaag
	telemetr = 60 * time.Second      // telemetrie-interval
	// busyFrac: onder dit deel van het verwachte tempo geldt een bron als
	// druk (70% — ruim onder de event-stream-jitter, ruim boven "half werk").
	busyNum, busyDen = 7, 10
)

// Start is de Pi-ingang: meet het firmware-maximum en start het beleid op de
// mailbox. Faalt de mailbox, dan wordt er alleen gelogd — de node draait dan
// gewoon op de firmware-klok.
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
	Run(mailbox{mb: cfg.Mbox, low: cfg.LowHz, max: max})
}

// mailbox is de Pi-knop: één ARM-klok voor alle cores, via de firmware.
type mailbox struct {
	mb       *vcmail.Mbox
	low, max uint32
}

func (m mailbox) set(hz uint32) (string, bool) {
	actual, ok := m.mb.SetClockRate(vcmail.ClockARM, hz)
	return fmt.Sprintf("%d MHz", actual/1_000_000), ok
}

func (m mailbox) Full() (string, bool)  { return m.set(m.max) }
func (m mailbox) Quiet() (string, bool) { return m.set(m.low) }

func (m mailbox) Telemetry() string {
	mC, _ := m.mb.Temp()
	hz, _ := m.mb.ClockRate(vcmail.ClockARM)
	return fmt.Sprintf("%d.%d°C, ARM %d MHz", mC/1000, mC%1000/100, hz/1_000_000)
}

// history is het glijdende venster van één bron: de idle-tikken en het
// verwachte tempo van de laatste `window` samples. Eén sample van 10ms was
// het oordeel tot 23-09, en dat hield de O6N voorgoed vol: een Go-app die
// 99,7% idle is heeft toch losse samples van 0-60% idle (runtime-klusjes van
// ~1ms die op de stille klok 3,25× langer duren, en een teller die pas bij
// terugkomst uit de yield bijloopt en dus in klonters binnenkomt). Over 50ms
// middelt dat weg; echte aanhoudende last haalt de grens na twee volle
// samples, dus klokt binnen ~20ms op in plaats van 10.
type history struct {
	d, want [window]uint64
	k       int
	full    bool
}

// add schuift een sample in het venster en geeft de venstersommen terug.
// Tot het venster vol is telt alleen wat er is: een verse app wordt dus
// meteen beoordeeld, niet pas na 50ms.
func (h *history) add(d, want uint64) (sumD, sumWant uint64) {
	h.d[h.k], h.want[h.k] = min(d, want*4), want // een klonter van een lange slaap telt hoogstens voor vier samples
	h.k = (h.k + 1) % window
	if h.k == 0 {
		h.full = true
	}
	n := h.k
	if h.full {
		n = window
	}
	for j := 0; j < n; j++ {
		sumD += h.d[j]
		sumWant += h.want[j]
	}
	return sumD, sumWant
}

// ctrl leest een woord van een control page zoals kern/slots dat doet: eerst
// de cachelijn weg (dev.Pull), dan lezen. Een kale Read64 zag op de O6N, waar
// de partitie cacheable gemapt is, zijn eigen verouderde kopie — de idle-
// teller van de app stond voor de governor stil ("0‰ idle") en de klok zakte
// nooit (23-09). Op de Pi viel het niet op.
func ctrl(page, off uintptr) uint64 {
	dev.Pull(page+off, 8)
	return dev.Read64(page + off)
}

// watch is de wachter: samplen, flanken schakelen, telemetrie.
func watch(k Knob) {
	last := make([]uint64, layout.MaxSlots+1) // [0] = HOP-core, [1..] = slots
	seen := make([]bool, layout.MaxSlots+1)   // eerste sample per actief slot = ijken
	hist := make([]history, layout.MaxSlots+1)
	quiet := time.Now() // sinds wanneer alles idle is
	lastTele := time.Now()

	// Elke flank logt (dat was een Verbose-knop die overal aanstond): flanken
	// zijn zeldzaam en de regel is de soak-diagnose.
	set := func(to func() (string, bool), why string) bool {
		if got, ok := to(); ok {
			fmt.Printf("dvfs: → %s (%s)\n", got, why)
			return true
		}
		fmt.Println("dvfs: clock change failed — retaining the previous policy state")
		return false
	}

	// Toestand niet aannemen maar zetten (GEMETEN 2026-07-11: met een
	// arm_freq_min-vloer boot de firmware op de vloer, niet op vol — de hele
	// P1-acceptatie draaide per ongeluk op 800MHz): boot-werk verdient de
	// volle klok, daarna regeert het beleid.
	high := set(k.Full, "boot")
	state.Store(high)

	tickHz := idle.CounterHz()
	for {
		time.Sleep(sample)
		expect := tickHz * uint64(sample) / uint64(time.Second) // per core, per sample

		busy := false
		mark := func(src int, d, want uint64) {
			busy = true
			busySrc.Store(int32(src))
			busyAt.Store(time.Now().UnixNano())
			busyIdle.Store(int32(d * 1000 / max(want, 1)))
		}
		// Bron 0: de HOP-core zelf (agent-drukte klokt ook op).
		n := idle.Ticks()
		if d, want := hist[0].add(n-last[0], expect); d*busyDen < want*busyNum {
			mark(0, d, want)
		}
		last[0] = n
		// Bronnen 1..MaxSlots: actieve app-slots (CtrlIdle op hun page;
		// CtrlCores deelt het verwachte tempo bij SMP). Het eerste sample
		// na een start ijkt alleen — daarna telt óók een teller die op 0
		// blijft staan (een app die vanaf seconde één 100% brandt) als druk.
		for i := 1; i <= layout.MaxSlots; i++ {
			page, live := SlotCtrl(i)
			if !live {
				seen[i] = false
				continue
			}
			cores := ctrl(page, layout.CtrlCores)
			if cores == 0 || ctrl(page, layout.CtrlStatus) != layout.StatusReady {
				seen[i] = false
				continue
			}
			n := ctrl(page, layout.CtrlIdle)
			if !seen[i] {
				hist[i] = history{}
			} else {
				d := n - last[i]
				if !SlotRunning(i) {
					d = max(d, expect*cores) // slaapt in zijn yield: dit sample was idle
				}
				if d, want := hist[i].add(d, expect*cores); d*busyDen < want*busyNum {
					mark(i, d, want)
				}
			}
			seen[i] = true
			last[i] = n
		}

		if h := hold.Load(); h != holdNone {
			if want := h == holdFull; want != high {
				if want {
					high = set(k.Full, "held")
				} else if set(k.Quiet, "held") {
					high = false
				}
			}
			quiet = time.Now() // loslaten begint niet meteen met "idle 30s"
			busy = false
		}

		switch {
		case hold.Load() != holdNone:
		case busy && !high:
			quiet = time.Now() // anders valt de klok één stil sample later
			// alweer terug ("idle 30s" één tel na "busy" — gemeten 19-07)
			high = set(k.Full, "busy")
		case busy:
			quiet = time.Now()
		case high && time.Since(quiet) > cooldown:
			if set(k.Quiet, "idle 30s") {
				high = false
			}
		}

		state.Store(high)

		if time.Since(lastTele) >= telemetr {
			lastTele = time.Now()
			fmt.Printf("dvfs: telemetry — %s, state=%s\n",
				k.Telemetry(), map[bool]string{true: "full", false: "quiet"}[high])
		}
	}
}
