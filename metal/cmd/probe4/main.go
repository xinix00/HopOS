// probe4 — de fase-P-"verifieer eerst"-image voor de Raspberry Pi 4: het
// eerste dat op het board geflasht wordt. Zelfde draaiboek als probe5
// (de Pi 5-variant), rapporteert via de PL011 op GPIO14/15 alles waar het
// HopOS-ontwerp op leunt, in oplopende spanning:
//
//  1. de tamago Go-runtime leeft (banner, goroutines, heap);
//  2. boot-EL (verwacht 2: TF-A), MPIDR (A72: core in aff0!), klokfrequentie;
//  3. de generic timer loopt op wandkloksnelheid (gemeten slaap);
//  4. PSCI: versie, AFFINITY_INFO van cores 1-3 — de probe kondigt de SMC
//     aan vóór hij hem doet: de stock armstub8 heeft GÉÉN PSCI (spin-table)
//     en dan verdwijnt de SMC in een lege EL3-vector. TF-A bl31.bin als
//     armstub is op dit board verplicht (sd-rpi4/LEESMIJ.txt);
//  5. CPU_ON per core naar geplante park-code → levensteken per core;
//  6. GENET-kiekje (P2-voorwerk): SYS_REV_CTRL + achtergelaten MAC.
//
// Bouwen/flashen: image/rpi4-probe.sh → sd-rpi4/.
package main

import (
	"fmt"
	"runtime"
	"time"
	_ "unsafe"

	"hop-os/metal/board"
	"hop-os/metal/board/raspi"
	"hop-os/metal/board/rpi4"
	"hop-os/metal/cpu/psci"
	"hop-os/metal/dev"
)

// RAM-declaratie: 128MB vanaf de kernel-load — ruim binnen elk Pi 4-model
// (1-8GB). Zelfde plan als de Pi 5 ná de laadadres-ontdekking (2026-07-09):
// laden op de Pi-default 0x80000, text op +0x10000 = 0x90000, en géén
// kernel_address in config.txt — de Pi 5 negeert die optie toch en zo laden
// beide boards identiek. De DTB ligt met device_tree_address búíten dit
// bereik.
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = raspi.HopKernelStart

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = raspi.HopKernelSize

// parkCode: per instructie via dev.Write32 op raspi.ParkBase geplant (MMU van
// de doelcore staat uit; adres onder onze RAM-declaratie → ongecachet
// geschreven). De core meldt zich met '0'+ctx op de UART, zet een teller op
// raspi.ParkCount + ctx*8, en parkeert in een WFE-lus (gedeelde generator:
// board/raspi/park.go).
var parkCode = raspi.ParkCode(rpi4.UART0Base)

// GENET-registeroffsets (alleen voor het read-only kiekje; bcmgenet is de
// registerreferentie): SYS_REV_CTRL op +0x0, UMAC_MAC0/1 op +0x80C/+0x810.
const (
	genetRev  = 0x000
	genetMAC0 = 0x80C
	genetMAC1 = 0x810
)

func main() {
	fmt.Println("")
	fmt.Println("HopOS probe4: bare-metal Go op de Raspberry Pi 4 — geen Linux aan boord")
	fmt.Printf("runtime %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	b := board.Current()
	fmt.Printf("boot-EL: %d (verwacht 2: TF-A bl31 op EL3)\n", b.BootEL())
	fmt.Printf("MPIDR: %#x → core %d (A72: aff0-nummering — anders dan de Pi 5!)\n", raspi.MPIDR(), b.CoreID())

	// Goroutines + heap: bewijs dat de runtime echt draait.
	done := make(chan int, 4)
	for i := range 4 {
		go func() {
			time.Sleep(time.Duration(i+1) * 50 * time.Millisecond)
			done <- i
		}()
	}
	for range 4 {
		<-done
	}
	fmt.Println("goroutines: OK")

	// Timer: een 500ms-slaap moet ~500ms wandklok duren (CNTFRQ van de
	// firmware klopt dan; BCM2711: 54MHz).
	t0 := time.Now()
	time.Sleep(500 * time.Millisecond)
	fmt.Printf("timer: 500ms slaap duurde %v\n", time.Since(t0))

	// Aankondiging vóór de eerste SMC: zonder TF-A (stock armstub8) is er
	// geen EL3-handler en hangt het hier — dan wijst deze regel de dader aan.
	fmt.Println("PSCI: versie opvragen via SMC (vereist TF-A bl31.bin als armstub —")
	fmt.Println("      blijft het hier stil: stock armstub8 zonder PSCI, zie LEESMIJ)...")
	major, minor := b.PSCIVersion()
	fmt.Printf("PSCI versie %d.%d (conduit SMC)\n", major, minor)

	for core := uint64(1); core <= 3; core++ {
		fmt.Printf("AFFINITY_INFO core %d: %s\n", core, b.AffinityInfo(core))
	}

	// Park-code planten en tellers vegen.
	for i, ins := range parkCode {
		dev.Write32(uintptr(raspi.ParkBase)+uintptr(i)*4, ins)
	}
	dev.Clear(uintptr(raspi.ParkCount), 4*8)
	dev.MB()

	// CPU_ON per core: het beslispunt van deze probe.
	ok := true
	for core := uint64(1); core <= 3; core++ {
		ret := b.CPUOn(core, uint64(raspi.ParkBase), core)
		fmt.Printf("CPU_ON core %d: ret=%d", core, ret)
		if ret == psci.ALREADY_ON {
			fmt.Println(" (ALREADY_ON — parkeert de armstub niet via PSCI? TF-A correct geladen? zie docs/rpi4.md)")
			ok = false
			continue
		}
		if ret != psci.SUCCESS {
			fmt.Println(" (FOUT)")
			ok = false
			continue
		}
		time.Sleep(200 * time.Millisecond)
		alive := dev.Read64(uintptr(raspi.ParkCount) + uintptr(core)*8)
		fmt.Printf(" → levensteken=%d, AFFINITY_INFO=%s\n", alive, b.AffinityInfo(core))
		if alive != 1 {
			ok = false
		}
	}

	if ok {
		fmt.Println("HOPOS_PI4_PROBE_OK — EL2-boot, PSCI en secundaire cores bewezen op de Pi 4")
	} else {
		fmt.Println("HOPOS_PI4_PROBE_DEELS — zie de regels hierboven; dit is meetdata, geen crash")
	}

	// ── Netprobe (fase P2-voorwerk): de Pi 4-NIC is de geïntegreerde GENET
	// (geen RP1/PCIe zoals de Pi 5 — metal/driver/nic/gem geldt hier dus niet; P2 wordt
	// een eigen GENET-driver). Read-only, aangekondigd vóór elke read.
	fmt.Println("netprobe 1: GENET SYS_REV_CTRL lezen op 0xfd580000 (hangt dit: blok niet geklokt)...")
	rev := dev.Read32(uintptr(rpi4.GENETBase) + genetRev)
	fmt.Printf("netprobe 1: GENET rev=%#x (major-nibble 6 ⇒ GENET v5, zoals bcmgenet 'm leest)\n", rev)

	fmt.Println("netprobe 2: UMAC MAC-registers (door de bootloader achtergelaten MAC?)...")
	fmt.Printf("netprobe 2: MAC0=%#x MAC1=%#x\n",
		dev.Read32(uintptr(rpi4.GENETBase)+genetMAC0), dev.Read32(uintptr(rpi4.GENETBase)+genetMAC1))
	fmt.Println("HOPOS_PI4_NETPROBE_KLAAR — stuur deze regels door; hiermee start de GENET-driver (P2)")

	// PID-1-regel: main keert nooit terug.
	for {
		time.Sleep(time.Hour)
	}
}
