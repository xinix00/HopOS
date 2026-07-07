// probe5 — de fase-P-"verifieer eerst"-image voor de Raspberry Pi 5: het
// eerste dat op het board geflasht wordt. Rapporteert via de debug-UART
// alles waar het HopOS-ontwerp op leunt, in oplopende spanning:
//
//  1. de tamago Go-runtime leeft (banner, goroutines, heap);
//  2. boot-EL (verwacht 2), MPIDR (A76: core in aff1), klokfrequentie;
//  3. de generic timer loopt op wandkloksnelheid (gemeten slaap);
//  4. PSCI: versie, AFFINITY_INFO van cores 1-3;
//  5. CPU_ON per core naar geplante park-code → levensteken per core.
//
// Uitkomst 5 is hét beslispunt: meldt de standaard armstub ALREADY_ON, dan
// bouwen we upstream TF-A als bl31.bin (armstub= in config.txt) — zie
// docs/rpi5.md. Bouwen/flashen: image/rpi5-probe.sh.
package main

import (
	"fmt"
	"runtime"
	"time"
	_ "unsafe"

	"hop-os/metal/board"
	"hop-os/metal/board/raspi"
	"hop-os/metal/board/rpi5"
	"hop-os/metal/dev"
	"hop-os/metal/gem"
)

// RAM-declaratie: 128MB vanaf de kernel-load (0x200000) — ruim binnen elke
// Pi 5-variant, onder het VC/firmware-gebied bovenin en boven TF-A onderin.
// De DTB is met device_tree_address bewust búíten dit bereik gelegd.
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = 0x00200000

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = 0x08000000

// parkCode: per instructie via dev.Write32 op raspi.ParkBase geplant (MMU van
// de doelcore staat uit; adres onder onze RAM-declaratie → ongecachet
// geschreven). De core meldt zich met '0'+ctx op de UART, zet een teller op
// raspi.ParkCount + ctx*8, en parkeert in een WFE-lus (gedeelde generator:
// board/raspi/park.go).
var parkCode = raspi.ParkCode(rpi5.UART0Base)

func main() {
	fmt.Println("")
	fmt.Println("HopOS probe5: bare-metal Go op de Raspberry Pi 5 — geen Linux aan boord")
	fmt.Printf("runtime %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	b := board.Current()
	fmt.Printf("boot-EL: %d (verwacht 2: TF-A/armstub op EL3)\n", b.BootEL())
	fmt.Printf("MPIDR: %#x → core %d (A76: aff1-nummering)\n", raspi.MPIDR(), b.CoreID())

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
	// firmware klopt dan).
	t0 := time.Now()
	time.Sleep(500 * time.Millisecond)
	fmt.Printf("timer: 500ms slaap duurde %v\n", time.Since(t0))

	major, minor := b.PSCIVersion()
	fmt.Printf("PSCI versie %d.%d (conduit %s)\n",
		major, minor, map[bool]string{true: "SMC", false: "HVC"}[b.BootEL() >= 2])

	for core := uint64(1); core <= 3; core++ {
		fmt.Printf("AFFINITY_INFO core %d: %s\n", core, powstr(b.AffinityInfo(core)))
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
		ret := rpi5.CPUOn(core, uint64(raspi.ParkBase), core)
		fmt.Printf("CPU_ON core %d: ret=%d", core, ret)
		if ret == raspi.PSCI_ALREADY_ON {
			fmt.Println(" (ALREADY_ON — standaard armstub houdt cores vast: bouw TF-A bl31.bin, zie docs/rpi5.md)")
			ok = false
			continue
		}
		if ret != raspi.PSCI_SUCCESS {
			fmt.Println(" (FOUT)")
			ok = false
			continue
		}
		time.Sleep(200 * time.Millisecond)
		alive := dev.Read64(uintptr(raspi.ParkCount) + uintptr(core)*8)
		fmt.Printf(" → levensteken=%d, AFFINITY_INFO=%s\n", alive, powstr(b.AffinityInfo(core)))
		if alive != 1 {
			ok = false
		}
	}

	if ok {
		fmt.Println("HOPOS_PI5_PROBE_OK — EL2-boot, PSCI en secundaire cores bewezen op de Pi 5")
	} else {
		fmt.Println("HOPOS_PI5_PROBE_DEELS — zie de regels hierboven; dit is meetdata, geen crash")
	}

	// ── Netprobe (fase P2-voorwerk): read-only metingen voor de GEM-driver
	// (metal/gem). Elke stap kondigt zich aan vóór de read: blijft de output
	// daar steken, dan wijst de laatste regel de boosdoener aan.
	fmt.Println("netprobe 1: RP1-venster — sysinfo lezen op 0x1f00000000 (hangt dit: PCIe-link niet actief)...")
	chipID := dev.Read32(uintptr(rpi5.RP1SysInfo))
	platform := dev.Read32(uintptr(rpi5.RP1SysInfo + 4))
	fmt.Printf("netprobe 1: RP1 CHIP_ID=%#x PLATFORM=%#x (0xffffffff = dode link)\n", chipID, platform)

	fmt.Println("netprobe 2: root-complex inbound-window (DMA-offset voor de GEM)...")
	fmt.Printf("netprobe 2: RC_BAR2_CONFIG lo=%#x hi=%#x (Linux-conventie: PCIe 0x10_00000000 → DRAM 0)\n",
		dev.Read32(uintptr(rpi5.RCBar2ConfigLo)), dev.Read32(uintptr(rpi5.RCBar2ConfigHi)))

	fmt.Println("netprobe 3: GEM module-ID op 0x1f00100000...")
	nic := &gem.Net{Base: uintptr(rpi5.RP1EthBase)}
	fmt.Printf("netprobe 3: GEM module-ID=%#x (verwacht: Cadence GEM-revisie, hoge helft 0x0002xxxx)\n", nic.ModuleID())
	fmt.Printf("netprobe 3: SPADDR1 (door bootloader achtergelaten MAC?) = %#x %#x\n",
		dev.Read32(uintptr(rpi5.RP1EthBase)+0x88), dev.Read32(uintptr(rpi5.RP1EthBase)+0x8C))
	fmt.Printf("netprobe 3: eth_cfg CLKGEN=%#x (bit7=ENABLE, 1:0=speed-override)\n",
		dev.Read32(uintptr(rpi5.EthCfgClkGen)))

	fmt.Println("netprobe 4: MDIO-scan (PHY zoeken; BCM54213PE = OUI-id1 0x600d)...")
	nic.MDIOEnable()
	if addr, id1, id2, found := nic.PHYScan(); found {
		fmt.Printf("netprobe 4: PHY op adres %d: id1=%#x id2=%#x\n", addr, id1, id2)
		fmt.Println("netprobe 5: autonegotiatie (kabel erin = link; max 8s)...")
		if speed, fd, err := nic.AutoNeg(addr, 8*time.Second); err == nil {
			fmt.Printf("netprobe 5: link %dMbps full-duplex=%v\n", speed, fd)
		} else {
			fmt.Printf("netprobe 5: %v (geen kabel? prima — de scan telt)\n", err)
		}
	} else {
		fmt.Println("netprobe 4: geen PHY gevonden (reset via RP1-GPIO nodig? → volgende iteratie)")
	}
	fmt.Println("HOPOS_PI5_NETPROBE_KLAAR — stuur deze regels door; hiermee kalibreren we metal/gem")

	// PID-1-regel: main keert nooit terug.
	for {
		time.Sleep(time.Hour)
	}
}

func powstr(s board.PowerState) string {
	switch s {
	case board.PowerOn:
		return "ON"
	case board.PowerOff:
		return "OFF"
	case board.PowerOnPending:
		return "ON_PENDING"
	}
	return fmt.Sprintf("?%d", int(s))
}
