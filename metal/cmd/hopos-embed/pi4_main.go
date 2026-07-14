//go:build rpi4

// pi4_main — de fase-P1-acceptatie op de Raspberry Pi 4: dezelfde multikernel
// als op de Pi 5, op de oudere Cortex-A72. De code ís gedeeld (metal/cpu/el2,
// stage2, slots, smp); dit draaiboek bewijst op dít silicium: stage-2-isolatie,
// de hard-kill via stage-2-intrekking, core-recycling via het parkeer-model
// (geen firmware-roundtrip) en SMP met gedeelde heap. Enige board-verschil:
// de A72 nummert cores in aff0 (de Pi 5-A76 in aff1) en de stock-firmware
// levert géén PSCI, dus TF-A bl31.bin is als armstub verplicht (image/
// rpi4-hopos.sh, config.txt armstub=bl31.bin).
//
// Rapportage via de PL011 op GPIO14/15. Bouwen/flashen: image/rpi4-hopos.sh.
package main

import (
	_ "embed"
	"fmt"
	"runtime"
	"time"

	"hop-os/metal/abi/layout"
	"hop-os/metal/board"
	_ "hop-os/metal/board/rpi4/hop" // registreert het board (init) + basis-hooks
	"hop-os/metal/kern/slots"
)

// Zelfde canonieke app als op QEMU/Pi 5 (slot-1-IPA), met rpi4-runtime-hooks
// gebouwd (-tags rpi4). Eén artifact draait op elk slot — de stage-2 is de
// relocatie.
//
//go:embed app4.elf
var app []byte

func fail(what string, err error) {
	fmt.Printf("FAIL %s: %v\n", what, err)
	for i := 1; i <= slots.NumSlots(); i++ {
		s := slots.Get(i)
		fmt.Printf("  slot %d: core=%s app=%d hb=%d vec=%d esr=%#x far=%#x\n",
			i, board.Current().AffinityInfo(uint64(i)), s.App, s.Heartbeat,
			s.FaultVec, s.FaultESR, s.FaultFAR)
	}
	fmt.Println("HOPOS_PI4_MULTIKERNEL_FAIL")
	for {
		time.Sleep(time.Hour)
	}
}

// drainLogs en waitExit zijn gedeeld met de Pi 5-main (metal/raspi_main.go).

func main() {
	fmt.Println("")
	fmt.Println("HopOS (rpi4): bare-metal multikernel op de Pi 4 — geen Linux aan boord")
	fmt.Printf("runtime %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	if el := board.Current().BootEL(); el < 2 {
		fail("boot", fmt.Errorf("EL%d-boot: HopOS vereist EL2 (TF-A bl31 als armstub)", el))
	}
	major, minor := board.Current().PSCIVersion()
	fmt.Printf("PSCI versie %d.%d (boot-EL%d, conduit SMC)\n", major, minor, board.Current().BootEL())

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
	fmt.Println("HOPOS_PI4_SLOTS_OK — app gestart, ring-logs en heartbeat gezien, coöperatief gestopt")

	// ── 2. Isolatie: de kooi op de A72. ──
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
	fmt.Println("HOPOS_PI4_ISOLATIE_OK — stage-2-kooi hard bewezen op de A72")

	// ── 3. Hard-kill: stage-2-intrekking op de A72-front-end. ──
	fmt.Println("hard-kill: slot 1 start met HANG=spin...")
	if err := slots.Start(1, app, 32<<20, 1, map[string]string{"HANG": "spin"}, nil, nil); err != nil {
		fail("hang-start", err)
	}
	go drainLogs(1, nil)
	if err := slots.WaitReady(1, 5*time.Second); err != nil {
		fail("hang-ready", err)
	}
	time.Sleep(300 * time.Millisecond)
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
	fmt.Println("HOPOS_PI4_HARDKILL_OK — for{}-spin geveld door stage-2-intrekking op de A72")

	// ── 4. Relocatie + cache-discipline: herstart op een gebruikte partitie. ──
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
	fmt.Println("HOPOS_PI4_RELOC_OK — canoniek artifact op meerdere slots + herstart op gebruikte partitie")

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
	fmt.Println("HOPOS_PI4_SMP_OK — één app op twee A72-cores, gedeelde heap, GC en teardown bewezen")

	fmt.Println("HOPOS_PI4_MULTIKERNEL_OK — fase P1: de multikernel draait op de Pi 4")

	select {}
}
