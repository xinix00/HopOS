//go:build rpi5

// pi5_main — de fase-P1-acceptatie op de Raspberry Pi 5: de multikernel op
// echt silicium. Zelfde slots-machinerie als virt_main (de code ís gedeeld:
// metal/el2-trampolines, stage2, slots, smp); het draaiboek zelf (secties
// 1-5: levenscyclus, isolatie, hard-kill, relocatie, SMP) is gedeeld met de
// Pi 4 (raspi_main.go: acceptance) — dit bestand draagt alleen de
// rpi5-preamble en de extra P2/P2b-secties (net, dvfs).
//
// Rapportage via de debug-UART (JST-SH). Bouwen/flashen: image/rpi5-hopos.sh.
package main

import (
	_ "embed"
	"fmt"
	"runtime"
	"time"

	raspihop "hop-os/metal/board/raspi/hop"
	"hop-os/metal/board/rpi5"
	_ "hop-os/metal/board/rpi5/hop" // registreert het board (init); de basis levert de tamago-hooks
	"hop-os/metal/kern/slots"
	"hop-os/metal/net/hopnet"
)

// Zelfde canonieke app als op QEMU (slot-1-IPA), alleen met rpi5-runtime-hooks
// gebouwd (-tags rpi5). Eén artifact draait op elk slot — de stage-2 is de
// relocatie, ook hier.
//
//go:embed app5.elf
var app []byte

func main() {
	fmt.Println("")
	fmt.Println("HopOS (rpi5): bare-metal multikernel op de Pi 5 — geen Linux aan boord")
	fmt.Printf("runtime %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	// EL2-invariant + PSCI/DRAM/slots-rapport (gedeeld, raspi_main.go).
	preamble("PI5")

	// Klokbeleid (P2b, docs/archief/plan-p2b-soak.md) — vroeg gestart zodat de
	// acceptatiesecties meteen de flanken bewijzen: de SMP-bench brandt echt
	// (→ "druk"), de stiltes erna klokken terug. Verbose: elke flank op de UART.
	raspihop.StartDVFS(uintptr(rpi5.VCMailBase))

	// ── 1-5: het gedeelde acceptatiedraaiboek (raspi_main.go). ──
	acceptance("PI5", "A76", app)

	fmt.Println("HOPOS_PI5_MULTIKERNEL_OK — fase P1: de multikernel draait op echt silicium")

	// ── 6. P2: netwerk. De hele keten is van HOP zelf — PCIe-RC-training,
	// RP1, GEM-DMA, DHCP — en daarboven de gVisor-stack in Go's net-package
	// (hopnet, zelfde code als QEMU). NTP als levend bewijs: een échte
	// UDP-roundtrip het internet op, en de node kent daarna de wandkloktijd.
	fmt.Println("net: PCIe→RP1→GEM opbrengen + DHCP (kabel erin!)...")
	if err := hopnet.Up(); err != nil {
		fmt.Printf("net: %v — node draait door zonder netwerk\n", err)
	} else {
		if err := hopnet.SyncTime("pool.ntp.org:123"); err != nil {
			fmt.Printf("ntp: %v\n", err)
		} else {
			fmt.Printf("ntp: kloktijd gezet — %s\n", time.Now().Format(time.RFC3339))
		}
		fmt.Println("HOPOS_PI5_NET_OK — fase P2-netwerk: de node praat met de wereld")
	}

	// ── 7. dvfs: het flank-bewijs (P2b). Eerst 35s stilte → de wachter
	// klokt naar de vloer; dan een app die echt rekent (de SMP-bench) → de
	// druk-flank hoort binnen ~10ms te vuren (zie de dvfs-regels).
	fmt.Println("dvfs-test: 35s stilte voor de laag-flank...")
	time.Sleep(35 * time.Second)
	fmt.Println("dvfs-test: rekenende app starten — verwacht een druk-flank...")
	if err := slots.Start(1, app, 128<<20, 2, map[string]string{"SMP": "bench"}, nil, nil); err != nil {
		fail("PI5", "dvfs-start", err)
	}
	go drainLogs(1, nil)
	if _, err := waitExit(1, 30*time.Second); err != nil {
		fail("PI5", "dvfs-bench", err)
	}
	if err := slots.Stop(1, 5*time.Second); err != nil {
		fail("PI5", "dvfs-stop", err)
	}
	fmt.Println("HOPOS_PI5_DVFS_OK — flanken zichtbaar in de dvfs-regels hierboven")

	// Klaar met de acceptatie — de node blijft draaien (in P2/P3 komt hier de
	// agent + HopRunner, jobs via de leader-API).
	select {}
}
