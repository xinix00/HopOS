//go:build rpi4 || rpi5 || rk3566 || apple

package main

// Het gedeelde fase-P1-acceptatiedraaiboek van de board-mains (pi4_main.go /
// pi5_main.go / rk3566_main.go). De secties 1-5 waren byte-identiek tussen de
// Pi's op de markerprefix (HOPOS_PI4_/HOPOS_PI5_) en de core-naam (A72/A76)
// na — dat zijn de parameters. De main() zelf blijft per board (eigen banner,
// en de Pi 5 draait extra P2/P2b-secties — net/dvfs — die de andere niet
// hebben).
//
// Heette tot 05-08 raspi_main.go. De inhoud was toen al board-neutraal (alles
// via board.Current(), kern/slots en abi/layout), en met de Radxa Zero 3E
// (RK3566) erbij zou die naam gaan liegen: dit is het gedeelde draaiboek, niet
// de Pi-main. Dát het ongewijzigd op een derde silicium draait ís het bewijs
// dat de board-naad houdt.

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/board"
	"github.com/xinix00/HopOS/metal/kern/slots"
)

// fail rapporteert een gefaalde acceptatiestap mét het slot-dumprapport
// (fault-registratie van de EL2-vectoren plus powertoestand — zodat élke
// fail meetdata is) en blijft dan stilstaan.
func fail(prefix, what string, err error) {
	fmt.Printf("FAIL %s: %v\n", what, err)
	for i := 1; i <= slots.NumSlots(); i++ {
		s := slots.Get(i)
		fmt.Printf("  slot %d: core=%s app=%d hb=%d vec=%d esr=%#x far=%#x\n",
			i, board.Current().AffinityInfo(uint64(i)), s.App, s.Heartbeat,
			s.FaultVec, s.FaultESR, s.FaultFAR)
	}
	fmt.Printf("HOPOS_%s_MULTIKERNEL_FAIL\n", prefix)
	for {
		time.Sleep(time.Hour)
	}
}

// acceptance draait de vijf multikernel-acceptatiesecties op echt silicium —
// precies wat alleen het board kan bewijzen (QEMU/TCG verhult cache- en
// front-end-gedrag): levenscyclus, stage-2-isolatie, hard-kill via
// stage-2-intrekking, relocatie + cache-discipline, en SMP met gedeelde heap.
func acceptance(prefix, core string, app []byte) {
	// De must*-helpers (helpers.go) falen hier met het board-prefix en
	// draaien op dít app-blob.
	failf = func(what string, err error) { fail(prefix, what, err) }
	demoApp = app

	// ── 1. Levenscyclus: start, ring-logs, heartbeat, coöperatieve stop. ──
	fmt.Println("start slot 1 (64MB)...")
	var logs1 int
	mustStart("start", 1, 64<<20, 1, map[string]string{"ROLE": "pi-worker"}, nil, nil, &logs1)
	mustReady("ready", 1, 5*time.Second)
	time.Sleep(900 * time.Millisecond)
	s := slots.Get(1)
	fmt.Printf("slot 1: core-on=%v app=%d hb=%d ram=%dMB logs=%d\n",
		s.CoreOn, s.App, s.Heartbeat, s.RAMSize>>20, logs1)
	if !s.CoreOn || s.App != layout.StatusReady || s.Heartbeat == 0 || s.RAMSize != 64<<20-layout.AbiTail || logs1 == 0 {
		fail(prefix, "status", fmt.Errorf("slot 1 inconsistent"))
	}
	mustStop("stop", 1, 3*time.Second)
	fmt.Printf("HOPOS_%s_SLOTS_OK — app gestart, ring-logs en heartbeat gezien, coöperatief gestopt\n", prefix)

	// ── 2. Isolatie: de kooi op dit silicium. PROBE=hop laat de app ──
	// HOP-geheugen lezen (IPA 0x40000000 — nooit gemapt); de EL2-vector moet
	// rapporteren en de core uitzetten, zónder nette exit.
	fmt.Println("isolatietest: slot 1 start met PROBE=hop...")
	s = mustFault("isolatie", 1, 32<<20, map[string]string{"PROBE": "hop"})
	fmt.Printf("fault-rapport slot 1: vec=%d esr=%#x far=%#x\n", s.FaultVec, s.FaultESR, s.FaultFAR)
	if s.FaultVec != layout.FaultSync || s.FaultFAR != layout.HopRAMStart {
		fail(prefix, "faultinfo", fmt.Errorf("verwacht vec=%d far=%#x", layout.FaultSync, uint64(layout.HopRAMStart)))
	}
	mustStop("iso-teardown", 1, time.Second)
	fmt.Printf("HOPOS_%s_ISOLATIE_OK — stage-2-kooi hard bewezen op de %s\n", prefix, core)

	// ── 3. Hard-kill: stage-2-intrekking op de echte front-end. ──
	// HANG=spin is een `for {}` (self-branch, géén geheugentoegang) — de
	// scherpste test: hertranslateert de front-end na de TLBI, dan faultt hij
	// op de genulde tabel en zet zichzelf uit. Dít kon QEMU niet bewijzen.
	fmt.Println("hard-kill: slot 1 start met HANG=spin...")
	mustStart("hang-start", 1, 32<<20, 1, map[string]string{"HANG": "spin"}, nil, nil, nil)
	mustReady("hang-ready", 1, 5*time.Second)
	time.Sleep(300 * time.Millisecond) // laat hem echt hangen
	mustStop("hard-kill", 1, time.Second)
	s = slots.Get(1)
	fmt.Printf("hard-kill-rapport slot 1: vec=%d (verwacht %d=stage-2-fault)\n", s.FaultVec, layout.FaultSync)
	if s.App == layout.StatusExited {
		fail(prefix, "hard-kill", fmt.Errorf("app exitte netjes — hij hoorde te hangen"))
	}
	if s.FaultVec != layout.FaultSync {
		fail(prefix, "hard-kill", fmt.Errorf("vec=%d, verwacht %d (stage-2-fault)", s.FaultVec, layout.FaultSync))
	}
	fmt.Printf("HOPOS_%s_HARDKILL_OK — for{}-spin geveld door stage-2-intrekking op de %s\n", prefix, core)

	// ── 4. Relocatie + cache-discipline: zelfde artifact op een ander slot, ──
	// en herstart op een zojuist gebruikte partitie (stale-line-test: zonder
	// de CleanInv in het loadpad is dít waar het op echt silicium misgaat).
	fmt.Println("relocatie: zelfde artifact op slot 2, daarna herstart op slot 1...")
	mustStart("reloc-start", 2, 32<<20, 1, map[string]string{"ROLE": "reloc"}, nil, nil, nil)
	mustReady("reloc-ready", 2, 5*time.Second)
	mustStop("reloc-stop", 2, 3*time.Second)
	mustStart("reuse-start", 1, 48<<20, 1, map[string]string{"ROLE": "hergebruik"}, nil, nil, nil)
	mustReady("reuse-ready", 1, 5*time.Second)
	mustStop("reuse-stop", 1, 3*time.Second)
	fmt.Printf("HOPOS_%s_RELOC_OK — canoniek artifact op meerdere slots + herstart op gebruikte partitie\n", prefix)

	// ── 5. SMP: één app op 2 cores, gedeelde heap, GC, nette teardown. ──
	fmt.Println("smp: slot 1 als 2-core app (gedeelde heap), core 2 secundair...")
	mustStart("smp-start", 1, 128<<20, 2, map[string]string{"SMP": "bench"}, nil, nil, nil)
	mustExit("smp", 1, 30*time.Second, 0)
	// Teardown: alle cores van de SMP-app moeten afgaan. CoreIdle toetst de
	// CORE-mailbox (Get.CoreOn is sinds de core-deling de slot-staat, en
	// "core 2" is hier een core, geen slot) — zelfde toets als virt_main.
	mustStop("smp-teardown", 1, 5*time.Second)
	for _, c := range []int{1, 2} {
		if !slots.CoreIdle(c) {
			fail(prefix, "smp-teardown", fmt.Errorf("core %d nog aan na teardown", c))
		}
	}
	fmt.Printf("HOPOS_%s_SMP_OK — één app op twee %s-cores, gedeelde heap, GC en teardown bewezen\n", prefix, core)
}

// preamble is de gedeelde boot-rapportage vóór de acceptatie: EL2-invariant
// (zonder EL2 geen stage-2-kooi), PSCI-versie, DRAM-meting en slot-telling.
func preamble(prefix string) {
	if el := board.Current().BootEL(); el < 2 {
		fail(prefix, "boot", fmt.Errorf("EL%d-boot: HopOS vereist EL2 (TF-A/armstub op EL3)", el))
	}
	major, minor := board.Current().PSCIVersion()
	fmt.Printf("PSCI versie %d.%d (boot-EL%d, conduit SMC)\n", major, minor, board.Current().BootEL())

	// Meetpunt voor de pool-uitbreiding naar het volle DRAM (vervolgstap): het
	// door de firmware gerapporteerde totaal. De bring-up-pool is bewust
	// conservatief (board/rpi4.go / rpi5.go).
	if total := board.Current().MemTotal(); total > 0 {
		fmt.Printf("DRAM volgens DTB: %d MB (pool nu: %d MB — uitbreiden is de vervolgstap)\n",
			total>>20, slots.PoolBytes()>>20)
	} else {
		fmt.Println("DRAM-detectie: geen DTB gevonden (x0-pad) — pool blijft conservatief")
	}
	fmt.Printf("app-slots: %d (PSCI-probe)\n", slots.NumSlots())
}
