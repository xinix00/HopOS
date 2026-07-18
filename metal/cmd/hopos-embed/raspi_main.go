//go:build rpi4 || rpi5

package main

// Het gedeelde fase-P1-acceptatiedraaiboek van de Pi 4- en Pi 5-main
// (pi4_main.go / pi5_main.go). De secties 1-5 waren byte-identiek tussen
// beide boards op de markerprefix (HOPOS_PI4_/HOPOS_PI5_) en de core-naam
// (A72/A76) na — dat zijn nu de parameters. De main() zelf blijft per board
// (eigen banner/preamble, en de Pi 5 draait extra P2/P2b-secties — net/dvfs —
// die de Pi 4 niet heeft).

import (
	"fmt"
	"time"

	"hop-os/metal/abi/layout"
	"hop-os/metal/board"
	"hop-os/metal/kern/slots"
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
	// ── 1. Levenscyclus: start, ring-logs, heartbeat, coöperatieve stop. ──
	fmt.Println("start slot 1 (64MB)...")
	var logs1 int
	if err := slots.Start(1, app, 64<<20, 1, map[string]string{"ROLE": "pi-worker"}, nil, nil); err != nil {
		fail(prefix, "start", err)
	}
	go drainLogs(1, &logs1)
	if err := slots.WaitReady(1, 5*time.Second); err != nil {
		fail(prefix, "ready", err)
	}
	time.Sleep(900 * time.Millisecond)
	s := slots.Get(1)
	fmt.Printf("slot 1: core-on=%v app=%d hb=%d ram=%dMB logs=%d\n",
		s.CoreOn, s.App, s.Heartbeat, s.RAMSize>>20, logs1)
	if !s.CoreOn || s.App != layout.StatusReady || s.Heartbeat == 0 || s.RAMSize != 64<<20-layout.NetRingStride || logs1 == 0 {
		fail(prefix, "status", fmt.Errorf("slot 1 inconsistent"))
	}
	if err := slots.Stop(1, 3*time.Second); err != nil {
		fail(prefix, "stop", err)
	}
	fmt.Printf("HOPOS_%s_SLOTS_OK — app gestart, ring-logs en heartbeat gezien, coöperatief gestopt\n", prefix)

	// ── 2. Isolatie: de kooi op dit silicium. PROBE=hop laat de app ──
	// HOP-geheugen lezen (IPA 0x40000000 — nooit gemapt); de EL2-vector moet
	// rapporteren en de core uitzetten, zónder nette exit.
	fmt.Println("isolatietest: slot 1 start met PROBE=hop...")
	if err := slots.Start(1, app, 32<<20, 1, map[string]string{"PROBE": "hop"}, nil, nil); err != nil {
		fail(prefix, "iso-start", err)
	}
	go drainLogs(1, nil)
	if err := slots.WaitReady(1, 5*time.Second); err != nil {
		fail(prefix, "iso-ready", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for slots.Get(1).CoreOn {
		if time.Now().After(deadline) {
			fail(prefix, "isolatie", fmt.Errorf("app leest HOP-geheugen zonder fault"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	s = slots.Get(1)
	fmt.Printf("fault-rapport slot 1: vec=%d esr=%#x far=%#x\n", s.FaultVec, s.FaultESR, s.FaultFAR)
	if s.App == layout.StatusExited {
		fail(prefix, "isolatie", fmt.Errorf("app exitte netjes — fault verwacht"))
	}
	if s.FaultVec != layout.FaultSync || s.FaultFAR != layout.HopRAMStart {
		fail(prefix, "faultinfo", fmt.Errorf("verwacht vec=%d far=%#x", layout.FaultSync, uint64(layout.HopRAMStart)))
	}
	if err := slots.Stop(1, time.Second); err != nil {
		fail(prefix, "iso-teardown", err)
	}
	fmt.Printf("HOPOS_%s_ISOLATIE_OK — stage-2-kooi hard bewezen op de %s\n", prefix, core)

	// ── 3. Hard-kill: stage-2-intrekking op de echte front-end. ──
	// HANG=spin is een `for {}` (self-branch, géén geheugentoegang) — de
	// scherpste test: hertranslateert de front-end na de TLBI, dan faultt hij
	// op de genulde tabel en zet zichzelf uit. Dít kon QEMU niet bewijzen.
	fmt.Println("hard-kill: slot 1 start met HANG=spin...")
	if err := slots.Start(1, app, 32<<20, 1, map[string]string{"HANG": "spin"}, nil, nil); err != nil {
		fail(prefix, "hang-start", err)
	}
	go drainLogs(1, nil)
	if err := slots.WaitReady(1, 5*time.Second); err != nil {
		fail(prefix, "hang-ready", err)
	}
	time.Sleep(300 * time.Millisecond) // laat hem echt hangen
	if err := slots.Stop(1, time.Second); err != nil {
		fail(prefix, "hard-kill", err)
	}
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
	if err := slots.Start(2, app, 32<<20, 1, map[string]string{"ROLE": "reloc"}, nil, nil); err != nil {
		fail(prefix, "reloc-start", err)
	}
	go drainLogs(2, nil)
	if err := slots.WaitReady(2, 5*time.Second); err != nil {
		fail(prefix, "reloc-ready", err)
	}
	if err := slots.Stop(2, 3*time.Second); err != nil {
		fail(prefix, "reloc-stop", err)
	}
	if err := slots.Start(1, app, 48<<20, 1, map[string]string{"ROLE": "hergebruik"}, nil, nil); err != nil {
		fail(prefix, "reuse-start", err)
	}
	go drainLogs(1, nil)
	if err := slots.WaitReady(1, 5*time.Second); err != nil {
		fail(prefix, "reuse-ready", err)
	}
	if err := slots.Stop(1, 3*time.Second); err != nil {
		fail(prefix, "reuse-stop", err)
	}
	fmt.Printf("HOPOS_%s_RELOC_OK — canoniek artifact op meerdere slots + herstart op gebruikte partitie\n", prefix)

	// ── 5. SMP: één app op 2 cores, gedeelde heap, GC, nette teardown. ──
	fmt.Println("smp: slot 1 als 2-core app (gedeelde heap), core 2 secundair...")
	if err := slots.Start(1, app, 128<<20, 2, map[string]string{"SMP": "bench"}, nil, nil); err != nil {
		fail(prefix, "smp-start", err)
	}
	go drainLogs(1, nil)
	code, err := waitExit(1, 30*time.Second)
	if err != nil || code != 0 {
		fail(prefix, "smp", fmt.Errorf("exit=%d, err=%v", code, err))
	}
	if err := slots.Stop(1, 5*time.Second); err != nil {
		fail(prefix, "smp-teardown", err)
	}
	for _, c := range []int{1, 2} {
		if slots.Get(c).CoreOn {
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
