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
	"hop-os/metal/fbcons"
	"hop-os/metal/fdt"
	"hop-os/metal/gem"
	"hop-os/metal/psci"
	"hop-os/metal/vcmbox"
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
// geschreven). De core zet een teller op raspi.ParkCount + ctx*8 en parkeert
// in een WFE-lus (gedeelde generator: board/raspi/park.go). UART-teken uit
// (arg 0) zolang er geen debug-sessie is — zie uartLive in console.go; met
// Debug Probe: raspi.ParkCode(rpi5.UART0Base).
var parkCode = raspi.ParkCode(0)

// blink geeft n korte pulsen op de groene ACT-LED. Gebruikt time.Sleep —
// dus pas ná de timer-diagnose inzetten; daarvóór is busyBlink het kanaal.
func blink(n int) {
	for range n {
		rpi5.LED(true)
		time.Sleep(120 * time.Millisecond)
		rpi5.LED(false)
		time.Sleep(120 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond)
}

// busyBlink knippert klokvrij: MMIO-reads als tijdsbasis (device-memory is
// per definitie ongecachet, ~100ns per read → ~0,3s per fase), bruikbaar
// zolang de generic timer verdacht is (boot-meting 2026-07-08: main hing in
// z'n eerste Sleep). De mailbox-statusread is side-effect-vrij.
func busyBlink(n int) {
	wait := func() {
		for range 3_000_000 {
			dev.Read32(uintptr(rpi5.MboxBase) + 0x18)
		}
	}
	for range n {
		rpi5.LED(true)
		wait()
		rpi5.LED(false)
		wait()
	}
	wait()
	wait()
}

// splash: het HDMI-levensteken (gewenst door Derek, docs/rpi5.md). Bewust
// ASCII-only — fbcons degradeert multibyte-UTF-8 tot '?'.
func splash() {
	fbcons.SetColor(0xFF7CE87C) // zacht groen; G-kanaal is byteorder-neutraal
	fmt.Println()
	fmt.Println(`   (\(\`)
	fmt.Println(`   ( -.-)     HopOS v0.1.0 - probe5`)
	fmt.Println(`   o_(")(")   aarch64 - pure Go - geen Linux aan boord`)
	fbcons.SetColor(0xFFFFFFFF)
	fmt.Println()
}

func main() {
	// GEEN time.Sleep en geen goroutines vóór de timer-diagnose op het
	// scherm staat: boot-meting 2026-07-08 — main hing in z'n eerste
	// Sleep (LED bleef stokstijf aan), dus de klok is verdacht tot het
	// scherm anders bewijst. LED continu aan = "Go-main draait".
	rpi5.LEDInit()
	rpi5.LED(true)

	fmt.Println("")
	fmt.Println("HopOS probe5: bare-metal Go op de Raspberry Pi 5 — geen Linux aan boord")
	fmt.Printf("runtime %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	// Framebuffer éérst (vcmbox is klokvrij begrensd): het scherm is het
	// rapportkanaal voor alles hierna. Twee routes: de mailbox (Pi 4-stijl;
	// op de Pi 5 vermoedelijk dood — geen start.elf-runtime meer) en de
	// firmware-simplefb uit de DTB (/chosen — wat Linux' early console ook
	// gebruikt). LED-taal: 3× busy = framebuffer beet en ga tekenen;
	// 5× busy = mailbox faalde (dan volgt de DTB-poging).
	fmt.Println("hdmi: VideoCore-mailbox (0x107c013880) → framebuffer 1280x720/32bpp...")
	mbox := &vcmbox.Chan{Base: rpi5.MboxBase, Buf: raspi.MboxScratch}
	fbUp := false
	if fb, ok := mbox.FBInit(1280, 720); ok {
		busyBlink(3)
		fbcons.Init(fb.Base, fb.Width, fb.Height, fb.Pitch)
		fbUp = true
		splash() // vanaf hier spiegelt printk álles naar het scherm
		fmt.Printf("hdmi: framebuffer %dx%d @ %#x, pitch %d, %d KB (mailbox)\n",
			fb.Width, fb.Height, fb.Base, fb.Pitch, fb.Size>>10)
	} else {
		busyBlink(5)
		fmt.Println("hdmi: mailbox-fb faalde — firmware-simplefb uit de DTB proberen (/chosen)...")
		// Eerst de x0-pointer (cpuinit → DTBPtr); zet de firmware die niet in
		// bare-metal-modus, dan het adres dat wijzelf kozen (config.txt:
		// device_tree_address=0x0f000000).
		sfb, ok := fdt.Framebuffer(uintptr(dev.Read64(rpi5.DTBPtr)))
		if !ok {
			sfb, ok = fdt.Framebuffer(0x0f000000)
		}
		if ok && sfb.BPP == 32 {
			busyBlink(3)
			fbcons.Init(uintptr(sfb.Base), int(sfb.Width), int(sfb.Height), int(sfb.Stride))
			fbUp = true
			splash()
			fmt.Printf("hdmi: firmware-simplefb %dx%d @ %#x, stride %d (DTB /chosen)\n",
				sfb.Width, sfb.Height, sfb.Base, sfb.Stride)
		} else {
			fmt.Println("hdmi: ook geen simplefb in de DTB — scherm blijft donker, LED-taal is het kanaal")
		}
	}
	if fbUp {
		rpi5.LED(false) // scherm werkt: LED weer vrij als signaal
	}

	// ── Timer-diagnose, klokvrij gemeten, elk punt aangekondigd vóór de
	// mogelijk-fatale stap (bevriest het scherm: de laatste regel = dader).
	fmt.Printf("timer: CNTFRQ_EL0 = %d Hz (verwacht 54000000; 0 = firmware zette 'm niet → Sleep hangt)\n", raspi.CNTFRQ())
	fmt.Println("timer: CNTPCT_EL0 lezen (trapt dit, dan staat EL1PCTEN uit)...")
	c0 := raspi.CNTPCT()
	for i := 0; i < 1000; i++ {
		_ = raspi.CNTPCT()
	}
	c1 := raspi.CNTPCT()
	fmt.Printf("timer: CNTPCT %d → %d (delta %d; 0 = counter staat stil)\n", c0, c1, c1-c0)
	fmt.Println("timer: time.Now() proberen...")
	t0 := time.Now()
	fmt.Printf("timer: time.Now() = %v\n", t0)
	fmt.Println("timer: time.Sleep(100ms) proberen (hangt dit: tamago-timerpad stuk ondanks CNTFRQ)...")
	s0 := raspi.CNTPCT()
	time.Sleep(100 * time.Millisecond)
	s1 := raspi.CNTPCT()
	fmt.Printf("timer: Sleep(100ms) duurde %d counter-ticks (verwacht ~5400000 bij 54MHz)\n", s1-s0)
	blink(2) // vanaf hier mag de LED weer knipperen: timer bewezen

	// Bonus-meetdata over hetzelfde kanaal — met als hoofdprijs de échte
	// VC-carveout-grens waar het P1-slot-plan om vraagt (docs/rpi5.md).
	if rev, ok := mbox.FirmwareRev(); ok {
		fmt.Printf("mailbox: firmware-rev %#x\n", rev)
	}
	if rev, ok := mbox.BoardRev(); ok {
		fmt.Printf("mailbox: board-rev %#x\n", rev)
	}
	if armBase, armSize, vcBase, vcSize, ok := mbox.MemSplit(); ok {
		fmt.Printf("mailbox: ARM-geheugen %#x + %d MB, VC-carveout %#x + %d MB (P1-plangrens)\n",
			armBase, armSize>>20, vcBase, vcSize>>20)
	}

	b := board.Current()
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
		fmt.Println("geheugen: x0 = 0 — firmware gaf geen DTB-pointer (val terug op mailbox, P2b)")
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

	// Timer: een 500ms-slaap moet ~500ms wandklok duren (CNTFRQ van de
	// firmware klopt dan).
	t1 := time.Now()
	time.Sleep(500 * time.Millisecond)
	fmt.Printf("timer: 500ms slaap duurde %v\n", time.Since(t1))

	major, minor := b.PSCIVersion()
	fmt.Printf("PSCI versie %d.%d (conduit %s)\n",
		major, minor, map[bool]string{true: "SMC", false: "HVC"}[b.BootEL() >= 2])

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
		ret := rpi5.CPUOn(core, uint64(raspi.ParkBase), core)
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

	// PID-1-regel: main keert nooit terug. De ACT-LED wordt de hartslag:
	// knippert hij op 1Hz, dan is de probe tot híér gekomen en leeft de
	// runtime nog — stopt hij, dan wijst de laatste schermregel de dader aan.
	fmt.Println("klaar — ACT-LED knippert nu als hartslag (1Hz); dit scherm blijft staan")
	for {
		rpi5.LED(true)
		time.Sleep(500 * time.Millisecond)
		rpi5.LED(false)
		time.Sleep(500 * time.Millisecond)
	}
}
