//go:build rpi5

// pi5_main — de fase-P1-acceptatie op de Raspberry Pi 5: de multikernel op
// echt silicium. Zelfde slots-machinerie als virt_main (de code ís gedeeld:
// metal/el2-trampolines, stage2, slots, smp), maar dit draaiboek bewijst
// precies de drie dingen die alleen het board kan bewijzen:
//
//  1. ISOLATIE — de stage-2-kooi op de A76: een app die buiten zijn partitie
//     grijpt wordt door de EL2-vector geveld (fault-rapport + CPU_OFF).
//  2. HARD-KILL — stage-2-intrekking velt een `for {}`-spin óók op de echte
//     A76-front-end (QEMU/TCG kon dat niet bewijzen).
//  3. SMP — één app op 2 cores met gedeelde heap, cross-core GC, en de
//     teardown die beide cores uitzet. Plus: de cache-maintenance
//     (dev.CleanInv in het loadpad en om de stage-2-tabellen) doet op de A76
//     écht werk — herhaalde starts op dezelfde partitie zijn de test.
//
// Rapportage via de debug-UART (JST-SH); ACT-LED-hartslag als kabelvrij
// levensteken na een groene run. Bouwen/flashen: image/rpi5-hopos.sh.
package main

import (
	_ "embed"
	"fmt"
	"runtime"
	"time"

	"hop-os/metal/board"
	"hop-os/metal/board/rpi5"
	"hop-os/metal/layout"
	"hop-os/metal/slots"
)

// Zelfde canonieke app als op QEMU (slot-1-IPA), alleen met rpi5-runtime-hooks
// gebouwd (-tags rpi5). Eén artifact draait op elk slot — de stage-2 is de
// relocatie, ook hier.
//
//go:embed app5.elf
var app []byte

func fail(what string, err error) {
	fmt.Printf("FAIL %s: %v\n", what, err)
	// Sectie-rapport vóór de stilstand: de fault-registratie van de
	// EL2-vectoren plus de powertoestand — zodat élke fail meetdata is.
	for i := 1; i <= slots.NumSlots(); i++ {
		s := slots.Get(i)
		fmt.Printf("  slot %d: core=%s app=%d hb=%d vec=%d esr=%#x far=%#x\n",
			i, board.Current().AffinityInfo(uint64(i)), s.App, s.Heartbeat,
			s.FaultVec, s.FaultESR, s.FaultFAR)
	}
	fmt.Println("HOPOS_PI5_MULTIKERNEL_FAIL")
	for {
		time.Sleep(time.Hour)
	}
}

func drainLogs(slot int, count *int) {
	for line := range slots.Logs(slot) {
		fmt.Printf("[slot%d] %s\n", slot, line)
		if count != nil {
			*count++
		}
	}
}

func waitExit(slot int, timeout time.Duration) (uint64, error) {
	deadline := time.Now().Add(timeout)
	for slots.Get(slot).App != layout.StatusExited {
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("slot %d meldt geen exit", slot)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return slots.Get(slot).ExitCode, nil
}

func main() {
	rpi5.LEDInit()
	fmt.Println("")
	fmt.Println("HopOS (rpi5): bare-metal multikernel op de Pi 5 — geen Linux aan boord")
	fmt.Printf("runtime %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	// EL2-boot is de invariant — zonder EL2 geen stage-2-kooi.
	if el := board.Current().BootEL(); el < 2 {
		fail("boot", fmt.Errorf("EL%d-boot: HopOS vereist EL2 (TF-A/armstub op EL3)", el))
	}
	major, minor := board.Current().PSCIVersion()
	fmt.Printf("PSCI versie %d.%d (boot-EL%d, conduit SMC)\n", major, minor, board.Current().BootEL())

	// Meetpunt voor de pool-uitbreiding naar de volle 8GB (vervolgstap): het
	// door de firmware gerapporteerde DRAM. De bring-up-pool is bewust
	// conservatief 512MB..2GB (board/rpi5/rpi5.go).
	if total := board.Current().MemTotal(); total > 0 {
		fmt.Printf("DRAM volgens DTB: %d MB (pool nu: %d MB — uitbreiden is de vervolgstap)\n",
			total>>20, slots.PoolBytes()>>20)
	} else {
		fmt.Println("DRAM-detectie: geen DTB gevonden (x0-pad) — pool blijft conservatief")
	}
	fmt.Printf("app-slots: %d (PSCI-probe)\n", slots.NumSlots())

	// ── 1. Levenscyclus: start, ring-logs, heartbeat, coöperatieve stop. ──
	fmt.Println("start slot 1 (64MB)...")
	var logs1 int
	if err := slots.Start(1, app, 64<<20, 1, map[string]string{"ROLE": "pi-worker"}, nil, nil); err != nil {
		fail("start", err)
	}
	go drainLogs(1, &logs1)
	if err := slots.WaitReady(1, 5*time.Second); err != nil {
		fail("ready", err)
	}
	time.Sleep(900 * time.Millisecond)
	s := slots.Get(1)
	fmt.Printf("slot 1: core-on=%v app=%d hb=%d ram=%dMB logs=%d\n",
		s.CoreOn, s.App, s.Heartbeat, s.RAMSize>>20, logs1)
	if !s.CoreOn || s.App != layout.StatusReady || s.Heartbeat == 0 || s.RAMSize != 64<<20 || logs1 == 0 {
		fail("status", fmt.Errorf("slot 1 inconsistent"))
	}
	if err := slots.Stop(1, 3*time.Second); err != nil {
		fail("stop", err)
	}
	fmt.Println("HOPOS_PI5_SLOTS_OK — app gestart, ring-logs en heartbeat gezien, coöperatief gestopt")

	// ── 2. Isolatie: de kooi op de A76. PROBE=hop laat de app HOP-geheugen ──
	// lezen (IPA 0x40000000 — nooit gemapt); de EL2-vector moet rapporteren
	// en de core uitzetten, zónder nette exit.
	fmt.Println("isolatietest: slot 1 start met PROBE=hop...")
	if err := slots.Start(1, app, 32<<20, 1, map[string]string{"PROBE": "hop"}, nil, nil); err != nil {
		fail("iso-start", err)
	}
	go drainLogs(1, nil)
	if err := slots.WaitReady(1, 5*time.Second); err != nil {
		fail("iso-ready", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for slots.Get(1).CoreOn {
		if time.Now().After(deadline) {
			fail("isolatie", fmt.Errorf("app leest HOP-geheugen zonder fault"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	s = slots.Get(1)
	fmt.Printf("fault-rapport slot 1: vec=%d esr=%#x far=%#x\n", s.FaultVec, s.FaultESR, s.FaultFAR)
	if s.App == layout.StatusExited {
		fail("isolatie", fmt.Errorf("app exitte netjes — fault verwacht"))
	}
	if s.FaultVec != layout.FaultSync || s.FaultFAR != layout.HopRAMStart {
		fail("faultinfo", fmt.Errorf("verwacht vec=%d far=%#x", layout.FaultSync, uint64(layout.HopRAMStart)))
	}
	if err := slots.Stop(1, time.Second); err != nil {
		fail("iso-teardown", err)
	}
	fmt.Println("HOPOS_PI5_ISOLATIE_OK — stage-2-kooi hard bewezen op de A76")

	// ── 3. Hard-kill: stage-2-intrekking op de echte A76-front-end. ──
	// HANG=spin is een `for {}` (self-branch, géén geheugentoegang) — de
	// scherpste test: hertranslateert de A76-front-end na de TLBI, dan faultt
	// hij op de genulde tabel en zet zichzelf uit. Dít kon QEMU niet bewijzen.
	fmt.Println("hard-kill: slot 1 start met HANG=spin...")
	if err := slots.Start(1, app, 32<<20, 1, map[string]string{"HANG": "spin"}, nil, nil); err != nil {
		fail("hang-start", err)
	}
	go drainLogs(1, nil)
	if err := slots.WaitReady(1, 5*time.Second); err != nil {
		fail("hang-ready", err)
	}
	time.Sleep(300 * time.Millisecond) // laat hem echt hangen
	if err := slots.Stop(1, time.Second); err != nil {
		fail("hard-kill", err)
	}
	s = slots.Get(1)
	fmt.Printf("hard-kill-rapport slot 1: vec=%d (verwacht %d=stage-2-fault)\n", s.FaultVec, layout.FaultSync)
	if s.App == layout.StatusExited {
		fail("hard-kill", fmt.Errorf("app exitte netjes — hij hoorde te hangen"))
	}
	if s.FaultVec != layout.FaultSync {
		fail("hard-kill", fmt.Errorf("vec=%d, verwacht %d (stage-2-fault)", s.FaultVec, layout.FaultSync))
	}
	fmt.Println("HOPOS_PI5_HARDKILL_OK — for{}-spin geveld door stage-2-intrekking op de A76")

	// ── 4. Relocatie + cache-discipline: zelfde artifact op een ander slot, ──
	// en herstart op een zojuist gebruikte partitie (stale-line-test: zonder
	// de CleanInv in het loadpad is dít waar het op echt silicium misgaat).
	fmt.Println("relocatie: zelfde artifact op slot 2, daarna herstart op slot 1...")
	if err := slots.Start(2, app, 32<<20, 1, map[string]string{"ROLE": "reloc"}, nil, nil); err != nil {
		fail("reloc-start", err)
	}
	go drainLogs(2, nil)
	if err := slots.WaitReady(2, 5*time.Second); err != nil {
		fail("reloc-ready", err)
	}
	if err := slots.Stop(2, 3*time.Second); err != nil {
		fail("reloc-stop", err)
	}
	if err := slots.Start(1, app, 48<<20, 1, map[string]string{"ROLE": "hergebruik"}, nil, nil); err != nil {
		fail("reuse-start", err)
	}
	go drainLogs(1, nil)
	if err := slots.WaitReady(1, 5*time.Second); err != nil {
		fail("reuse-ready", err)
	}
	if err := slots.Stop(1, 3*time.Second); err != nil {
		fail("reuse-stop", err)
	}
	fmt.Println("HOPOS_PI5_RELOC_OK — canoniek artifact op meerdere slots + herstart op gebruikte partitie")

	// ── 5. SMP: één app op 2 cores, gedeelde heap, GC, nette teardown. ──
	fmt.Println("smp: slot 1 als 2-core app (gedeelde heap), core 2 secundair...")
	if err := slots.Start(1, app, 128<<20, 2, map[string]string{"SMP": "bench"}, nil, nil); err != nil {
		fail("smp-start", err)
	}
	go drainLogs(1, nil)
	code, err := waitExit(1, 30*time.Second)
	if err != nil || code != 0 {
		fail("smp", fmt.Errorf("exit=%d, err=%v", code, err))
	}
	if err := slots.Stop(1, 5*time.Second); err != nil {
		fail("smp-teardown", err)
	}
	for _, c := range []int{1, 2} {
		if slots.Get(c).CoreOn {
			fail("smp-teardown", fmt.Errorf("core %d nog aan na teardown", c))
		}
	}
	fmt.Println("HOPOS_PI5_SMP_OK — één app op twee A76-cores, gedeelde heap, GC en teardown bewezen")

	fmt.Println("HOPOS_PI5_MULTIKERNEL_OK — fase P1: de multikernel draait op echt silicium")

	// Kabelvrij levensteken: 1Hz-hartslag op de ACT-LED.
	for {
		rpi5.LED(true)
		time.Sleep(100 * time.Millisecond)
		rpi5.LED(false)
		time.Sleep(900 * time.Millisecond)
	}
}
