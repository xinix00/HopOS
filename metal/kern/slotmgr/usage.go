// usage.go: per-slot CPU-benutting uit de idle-teller — Dereks vraag
// (18-07): "kunnen we CPU per app achterhalen door te zien hoe idle hij
// runt?" Ja, en de meting bestond al: elke app publiceert zijn idle-TIJD
// (generic-timer-ticks, metal/cpu/idle) op de control-page (CtrlIdle) —
// een idle core accumuleert ~idle.CounterHz per seconde, een rekenende
// core staat stil. Het dvfs-klokbeleid op de Pi leest ditzelfde signaal op
// 10ms voor de flank; hier middelt een 5s-venster het tot een rapportage-
// cijfer. Geen app-ABI-wijziging: alleen een lezer erbij.
//
// De uitkomst is een percentage van de cores waarop het slot draait (0..100,
// SMP-genormaliseerd via CtrlCores) — precies de vorm die HOP's monitor als
// cpu_percent doorgeeft, zoals docker dat voor containers doet.
//
// WAAROM DIT OOK VOOR GEDEELDE BEWONERS KLOPT (nagekeken 31-07, want het ziet
// eruit als een gat en is het niet). Een app is op elk moment precies één van
// twee dingen: hij draait zijn eigen code, of hij zit in de yield van zijn
// idle-governor. En die yield MEET de hele periode waarin hij weg was: zowel
// hvcYield (arm64) als ecallYield (riscv64) lezen de tellerstand vóór de trap en
// erna, dus de tijd waarin de MEDEBEWONER draaide en de tijd waarin de core sliep
// zitten allebei in de idle-tijd van deze app. Er geldt dus per slot
//
//	idle_i = T − eigen_runtijd_i        (T = wandkloktijd van het venster)
//
// en daarmee is (T − idle_i)/T exact de eigen runtijd van díé app als fractie van
// het fysieke hart. Descheduled tijd wordt hier dus NIET als busy geteld — precies
// de eis. Drie volledig bezige medebewoners lezen daardoor ~33% elk en niet 100%
// elk: het cijfer telt op tot de core, wat een monitor-cijfer hoort te doen.
//
// Wat er WÉL niet gemeten kan worden staat hieronder bij accounts(): een
// architectuur waar een dedicated core zijn idle-tijd niet bijhoudt. Daar geven we
// -1 (onbekend) in plaats van een cijfer dat 100% zou zeggen.

//go:build tamago

package slotmgr

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/cpu/idle"
	"github.com/xinix00/HopOS/metal/kern/slots"
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

// accounts meldt of de idle-teller van dít slot een cijfer waard is. Een
// GEDEELDE bewoner meet altijd (de yield beslaat zijn hele descheduled-periode,
// op beide architecturen); een DEDICATED slot hangt aan de idle-governor van
// zijn arch. Sinds 01-08 meten beide architecturen ook dáár (ARM WFE't, RISC-V
// yieldt óók als hij alleen woont), dus vandaag is dit altijd waar — de check
// blijft staan voor de arch waar dat ooit niet zo is, want zonder hem las een
// niet-metend dedicated slot 100%, en dat is geen meting maar een meetgat.
func accounts(s slots.Status) bool { return s.Shared || idle.AccountsDedicated() }

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
			if !s.CoreOn || s.Cores == 0 || !accounts(s) {
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
