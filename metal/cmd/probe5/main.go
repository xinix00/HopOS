// probe5 — de fase-P-"verifieer eerst"-image voor de Raspberry Pi 5: het
// eerste dat op het board geflasht wordt. Rapporteert via de debug-UART
// (JST-SH-connector, Raspberry Pi Debug Probe) alles waar het HopOS-ontwerp
// op leunt, in oplopende spanning:
//
//  1. de tamago Go-runtime leeft (banner, goroutines, heap);
//  2. boot-EL (verwacht 2), MPIDR (A76: core in aff1), DTB/RAM via x0;
//  3. de generic timer: CNTFRQ, lopende counter, time.Sleep (boot-meting
//     2026-07-08: main hing in z'n eerste Sleep — dus expliciet verhoord,
//     elk punt aangekondigd vóór de mogelijk-fatale stap);
//  4. CPU: effectieve kloksnelheid (firmware-default) en temperatuur (AVS);
//  5. PSCI: versie, AFFINITY_INFO, CPU_ON per core → levensteken per core;
//  6. PCIe: externe poort (pciex1 — NVMe/HAT) en RP1 (netprobe → GEM/PHY).
//
// Uitkomst 5 is hét beslispunt: meldt de standaard armstub ALREADY_ON, dan
// bouwen we upstream TF-A als bl31.bin (armstub= in config.txt) — zie
// docs/rpi5.md. Bouwen/flashen: image/rpi5-probe.sh. LET OP config.txt:
// os_check=0 is op de Pi 5 verplicht voor niet-Linux-kernels.
//
// De ACT-LED is het kabelvrije levensteken: 3× knipperen = cpuinit draait
// (plus 1 korte puls = EL1 bereikt), 2× = timer bewezen, 1Hz-hartslag =
// probe compleet.
package main

import (
	"fmt"
	"runtime"
	"time"
	_ "unsafe"

	"hop-os/metal/board"
	"hop-os/metal/board/raspi"
	"hop-os/metal/board/rpi5"
	_ "hop-os/metal/board/rpi5/hop" // board.Board-registratie (board.Current in de probe)
	"hop-os/metal/cpu/psci"
	"hop-os/metal/dev"
	"hop-os/metal/driver/fb"
	"hop-os/metal/driver/nic/gem"
)

// RAM-declaratie: 128MB vanaf de kernel-load. ONTDEKT 2026-07-09 (sessie 2,
// XN-kooi + ADRP-analyse): de Pi 5-EEPROM-bootloader NEGEERT kernel_address
// en laadt raw images altijd op 0x80000 — drie dagen "MMU-wedge" bleek een
// image dat op 0x80000 draaide terwijl alles op 0x200000 gelinkt was (PC-
// relatieve asm werkte, absolute Go-funcvals/literals wezen 0x180000 ernaast,
// en de multi-level map markeerde de échte code-pagina's als device+XN).
// Dus: linken op de werkelijkheid — load 0x80000, text +0x10000 = 0x90000.
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = raspi.HopKernelStart

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = raspi.HopKernelSize

// parkCode: per instructie via dev.Write32 op raspi.ParkBase geplant (MMU van
// de doelcore staat uit; adres onder onze RAM-declaratie → ongecachet
// geschreven). De core meldt zich met '0'+ctx op de UART, zet een teller op
// raspi.ParkCount + ctx*8 en parkeert in een WFE-lus (gedeelde generator:
// board/raspi/park.go).
var parkCode = raspi.ParkCode(rpi5.UART0Base)

// blink geeft n korte pulsen op de groene ACT-LED. Gebruikt time.Sleep —
// dus pas inzetten nadat de timer-diagnose de klok bewezen heeft.
func blink(n int) {
	for range n {
		rpi5.LED(true)
		time.Sleep(120 * time.Millisecond)
		rpi5.LED(false)
		time.Sleep(120 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
}

func main() {
	// LED continu aan = "Go-main draait" — vóór de eerste print, want een
	// dode UART mag de boot niet verhullen. Géén Sleep vóór de timer-diagnose.
	rpi5.LEDInit()
	rpi5.LED(true)

	fmt.Println("")
	fmt.Println("HopOS probe5: bare-metal Go op de Raspberry Pi 5 — geen Linux aan boord")
	fmt.Printf("runtime %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	b := board.Current()

	// HDMI-log (metal/driver/fb): gebruikt het beeld dat de firmware al aanzette
	// (DT-simplefb) — meteen een meetpunt: laat de firmware in bare-metal-
	// modus (os_check=0) een framebuffer achter? Vanaf Init spiegelt printk.
	if desc, ok := b.Framebuffer(); ok {
		fb.Init(desc)
		fmt.Printf("fb: firmware-framebuffer %dx%d @ %#x (stride %d, %d bpp) — log ook op HDMI\n",
			desc.Width, desc.Height, desc.Base, desc.Stride, desc.BPP)
	} else {
		fmt.Println("fb: geen simplefb in de DTB — log alleen op de UART")
	}

	fmt.Printf("boot-EL: %d (verwacht 2: TF-A/armstub op EL3)\n", b.BootEL())
	fmt.Printf("MPIDR: %#x → core %d (A76: aff1-nummering)\n", raspi.MPIDR(), b.CoreID())

	// Geheugen: bewijst het universele pad — de Pi-firmware hoort x0 = DTB
	// mee te geven (ARM64 Linux-boot-protocol), cpuinit legde 'm op DTBPtr.
	dtb := dev.Read64(uintptr(rpi5.DTBPtr))
	fmt.Printf("DTB-pointer (x0 bij boot): %#x\n", dtb)
	if n := b.MemTotal(); n > 0 {
		fmt.Printf("geheugen: %d MB DRAM (uit /memory in de DTB) — x0-pad werkt op de Pi!\n", n>>20)
	} else if dtb != 0 {
		fmt.Printf("geheugen: DTB-detectie faalde — magic-woord @ ptr = %#x (DTB elders?)\n", dev.Read32(uintptr(dtb)))
	} else {
		fmt.Println("geheugen: x0 = 0 — firmware gaf geen DTB-pointer (bare-metal-modus? DTB ligt op 0x0f000000)")
	}

	// ── Timer-diagnose: elk punt aangekondigd vóór de mogelijk-fatale stap
	// (bevriest de output, dan is de laatste regel de dader).
	fmt.Printf("timer: CNTFRQ_EL0 = %d Hz (verwacht 54000000; 0 = firmware zette 'm niet → Sleep hangt)\n", raspi.CNTFRQ())
	fmt.Println("timer: CNTPCT_EL0 lezen (trapt dit, dan staat EL1PCTEN uit)...")
	c0 := raspi.CNTPCT()
	for i := 0; i < 1000; i++ {
		_ = raspi.CNTPCT()
	}
	c1 := raspi.CNTPCT()
	fmt.Printf("timer: CNTPCT %d → %d (delta %d; 0 = counter staat stil)\n", c0, c1, c1-c0)
	fmt.Println("timer: time.Sleep(100ms) proberen (hangt dit: tamago-timerpad stuk ondanks CNTFRQ)...")
	s0 := raspi.CNTPCT()
	time.Sleep(100 * time.Millisecond)
	s1 := raspi.CNTPCT()
	fmt.Printf("timer: Sleep(100ms) duurde %d counter-ticks (verwacht ~5400000 bij 54MHz)\n", s1-s0)
	blink(2) // timer bewezen: LED mag weer knipperen

	// CPU-klok: afhankelijke SUBS-lus (~1 cycle/iter) tegen de 54MHz-counter.
	// Dit is de firmware-default — het vertrekpunt voor klokbeleid (P2b).
	const spinIters = 100_000_000
	k0 := raspi.CNTPCT()
	raspi.Spin(spinIters)
	k1 := raspi.CNTPCT()
	if d := k1 - k0; d > 0 {
		fmt.Printf("cpu: ~%d MHz effectief (SUBS-lus, ±dual-issue-marge; A76-default vaak 1500)\n",
			spinIters*54/d)
	}

	// Temperatuur: AVS-monitor (0x107d542000 + 0x200), bcm2711-thermal-
	// formule met de coëfficiënten uit de BCM2712-DTB: t = 450000 − 550×raw
	// (milligraden; geldig-bits 16|10).
	fmt.Println("temp: AVS_RO_TEMP_STATUS lezen op 0x107d542200...")
	raw := dev.Read32(uintptr(rpi5.AVSMonBase) + 0x200)
	if raw&(1<<16|1<<10) != 0 {
		mC := 450000 - 550*int64(raw&0x3FF)
		fmt.Printf("temp: %d.%d °C (raw %#x)\n", mC/1000, (mC%1000)/100, raw)
	} else {
		fmt.Printf("temp: sensor meldt ongeldig (raw %#x)\n", raw)
	}

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
			fmt.Println(" (ALREADY_ON — standaard armstub houdt cores vast: bouw TF-A bl31.bin, zie docs/rpi5.md)")
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
		fmt.Println("HOPOS_PI5_PROBE_OK — EL2-boot, PSCI en secundaire cores bewezen op de Pi 5")
	} else {
		fmt.Println("HOPOS_PI5_PROBE_DEELS — zie de regels hierboven; dit is meetdata, geen crash")
	}

	// ── PCIe-probe (fase P2-voorwerk, read-only): de externe poort (pciex1,
	// de FFC voor NVMe/HAT's — brcm,bcm2712-pcie, brcmstb-registerlayout).
	// Config-reads alleen bij een actieve link (anders completion-timeout).
	fmt.Println("nvmeprobe 1: pciex1-status lezen op 0x1000114068 (brcmstb MISC_PCIE_STATUS)...")
	st := dev.Read32(uintptr(rpi5.PCIeX1Base) + 0x4068)
	fmt.Printf("nvmeprobe 1: status=%#x — PHY-link=%v, data-link=%v (firmware traint deze poort normaal níét)\n",
		st, st&0x10 != 0, st&0x20 != 0)
	fmt.Printf("nvmeprobe 1: RC vendor/device = %#x (verwacht 0x14e4xxxx, Broadcom)\n",
		dev.Read32(uintptr(rpi5.PCIeX1Base)))
	if st&0x20 != 0 {
		// EXT_CFG: bus 1, dev 0, fn 0 → endpoint-ID (NVMe? Hailo 0x1e60?).
		dev.Write32(uintptr(rpi5.PCIeX1Base)+0x9000, 1<<20)
		dev.MB()
		fmt.Printf("nvmeprobe 2: endpoint vendor/device = %#x\n",
			dev.Read32(uintptr(rpi5.PCIeX1Base)+0x8000))
	} else {
		fmt.Println("nvmeprobe 2: link down — RC-bring-up is P2-werk (zelfde NVMe-driver als QEMU/O6N)")
	}

	// ── Netprobe (fase P2-voorwerk): read-only metingen voor de GEM-driver
	// (metal/driver/nic/gem). Elke stap kondigt zich aan vóór de read: blijft de output
	// daar steken, dan wijst de laatste regel de boosdoener aan.
	fmt.Println("netprobe 1: RP1-venster — sysinfo lezen op 0x1f00000000 (hangt dit: PCIe-link niet actief)...")
	chipID := dev.Read32(uintptr(rpi5.RP1SysInfo))
	platform := dev.Read32(uintptr(rpi5.RP1SysInfo + 4))
	fmt.Printf("netprobe 1: RP1 CHIP_ID=%#x PLATFORM=%#x (0xffffffff = dode link)\n", chipID, platform)

	fmt.Println("netprobe 2: root-complex inbound-window (DMA-offset voor de GEM)...")
	fmt.Printf("netprobe 2: RC_BAR2_CONFIG lo=%#x hi=%#x (Linux-conventie: PCIe 0x10_0000_0000 → DRAM 0)\n",
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
	fmt.Println("HOPOS_PI5_NETPROBE_KLAAR — stuur deze regels door; hiermee kalibreren we metal/driver/nic/gem")

	// PID-1-regel: main keert nooit terug. De ACT-LED wordt de hartslag:
	// knippert hij op 1Hz, dan is de probe tot híér gekomen en leeft de
	// runtime nog — stopt hij, dan wijst de laatste UART-regel de dader aan.
	fmt.Println("klaar — ACT-LED knippert nu als hartslag (1Hz)")
	for {
		rpi5.LED(true)
		time.Sleep(500 * time.Millisecond)
		rpi5.LED(false)
		time.Sleep(500 * time.Millisecond)
	}
}
