// probe6 — de PCIe-bring-up-probe voor de Raspberry Pi 5 (fase P2).
//
// GEMETEN met probe5 (2026-07-10): de firmware traint geen enkele PCIe-link.
// RP1 (netwerk) én pciex1 (NVMe) zijn bij boot onbereikbaar: alle reads door
// het venster gaven 0xdeaddead, PHYLINKUP=DL_ACTIVE=0. Deze probe doet wat
// Linux elke boot doet (metal/brcmpcie, recept uit pcie-brcmstb.c):
//
//  1. RESCAL-kalibratie (één gedeeld analoog blok voor alle controllers);
//  2. pcie2 (RP1): bridge-reset, SerDes wekken, windows, en dé sleutel —
//     de PHY-PLL via MDIO omzetten naar de 54MHz-kristalrefclk (af fabriek
//     verwacht hij 100MHz: dáárom trainde er nooit iets);
//  3. PERST# lossen, link pollen → HET beslispunt van deze probe;
//  4. RP1 enumereren (verwacht 0x1de4:0x0001 — de firmware heeft RP1 al
//     via I²C van firmware voorzien, het wacht op ons), BAR's opmeten en
//     toewijzen (BAR1 → PCIe 0x0: Linux-conventie én DMA-loopback-eis);
//  5. de netprobe-metingen herhalen tegen een nu levende RP1: CHIP_ID,
//     GEM-module-ID, MDIO-PHY-scan, autonegotiatie (kabel erin!);
//  6. dezelfde sequence op pcie1 (de FFC: M.2/NVMe) — zit er niets in het
//     slot, dan traint die link gewoon niet; ook dat is meetdata.
//
// Elke stap kondigt zich aan vóór de mogelijk-fatale actie: bevriest de
// UART, dan wijst de laatste regel de dader aan. Bouwen/flashen:
// image/rpi5-probe6.sh → sd-rpi5/. ACT-LED: 1Hz-hartslag = probe compleet.
package main

import (
	"fmt"
	"runtime"
	"time"
	_ "unsafe"

	"hop-os/metal/board/raspi"
	"hop-os/metal/board/rpi5"
	"hop-os/metal/brcmpcie"
	"hop-os/metal/dev"
	"hop-os/metal/gem"
)

// RAM-declaratie: zie probe5 — de Pi 5-EEPROM laadt raw images op 0x80000.
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = raspi.HopKernelStart

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = raspi.HopKernelSize

// barAssign meet en programmeert de memory-BAR's van één endpoint via het
// gedeelde brcmpcie.AssignBARs (zelfde codepad als de rpi5-board-bring-up) en
// print de meting — dit is een probe, dus de groottes en toewijzingen komen op
// de UART. Geeft het PCIe-adres per BAR-index (^0 = niet aanwezig).
func barAssign(rc *brcmpcie.RC, bus, devno int) [6]uint64 {
	addr, size, is64 := rc.AssignBARs(bus, devno)
	for i := 0; i < 6; i++ {
		if size[i] == 0 {
			continue
		}
		fmt.Printf("  BAR%d: size=%#x 64-bit=%v\n", i, size[i], is64[i])
		if is64[i] {
			i++
		}
	}
	for i := 0; i < 6; i++ {
		if size[i] == 0 {
			continue
		}
		fmt.Printf("  BAR%d → PCIe %#x\n", i, addr[i])
		if is64[i] {
			i++
		}
	}
	return addr
}

// dmaTest is netprobe 5: het eerste echte pakket. GEM-ringen in DRAM búíten
// onze RAM-declaratie (0x18000000 → device-gemapt → ongecachet → coherent
// met de RP1-DMA zonder cache-onderhoud), BusOff = het inbound-window van de
// root-complex (PCIe 0x10_0000_0000 → CPU 0x0). Dan een handgebouwde DHCP
// DISCOVER de kabel op (bewijst TX-DMA) en 10s rauw meelezen (bewijst
// RX-DMA); een OFFER terug is de kers: het hele pad werkt, beide richtingen.
func dmaTest(nic *gem.Net, speed int, fd bool) {
	// MAC: lokaal beheerd (bit 1 van byte 0) — de bootloader liet niets in
	// SPADDR achter (gemeten run 2); de echte board-MAC (uit serial/OTP) is
	// P2-integratiewerk, voor de meting telt alleen een geldig uniek adres.
	nic.MAC = [6]byte{0x02, 0x48, 0x4f, 0x50, 0x00, 0x06} // "HOP" + probe6
	nic.BusOff = 0x10_0000_0000

	// Hardware-eigenschappen printen vóór de sprong: DBWDEF moet ≥2 zijn
	// (64-bit AXI → NCFGR.DBW=1, dé fix na run 4), queue-mask 0 (alleen
	// queue 0 — anders wedgen ongebruikte queues de DMA), DAW64=1.
	d1, d6 := nic.DesignCfg()
	fmt.Printf("netprobe 5: DCFG1=%#x (DBWDEF=%d) DCFG6=%#x (queues=%#x, DAW64=%v)\n",
		d1, d1>>25&0x7, d6, d6&0xff, d6&(1<<23) != 0)

	fmt.Println("netprobe 5: GEM-DMA init (ringen op 0x18000000, BusOff 0x10_0000_0000)...")
	if err := nic.Init(0x18000000, 0x100000, speed, fd); err != nil {
		fmt.Printf("netprobe 5: %v\n", err)
		return
	}

	fmt.Println("netprobe 5: DHCP DISCOVER versturen (broadcast)...")
	if err := nic.Transmit(dhcpDiscover(nic.MAC)); err != nil {
		fmt.Printf("netprobe 5: TX faalt: %v\n", err)
		return
	}
	time.Sleep(100 * time.Millisecond)
	fmt.Printf("netprobe 5: TX-status=%#x (bit3=TGO/actief, bit5=compleet)\n", nic.TxStatus())

	// 10s meelezen: élk frame is bewijs dat RX-DMA in ons DRAM schrijft;
	// een broadcast-LAN is nooit stil (ARP/mDNS), dus 0 frames = meetdata.
	buf := make([]byte, 1536)
	frames, shown, offer := 0, 0, false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		n, _ := nic.Receive(buf)
		if n == 0 {
			time.Sleep(time.Millisecond)
			continue
		}
		frames++
		if shown < 8 {
			fmt.Printf("netprobe 5: RX %s\n", inspect(buf[:n]))
			shown++
		}
		if isOffer(buf[:n]) {
			offer = true
			break
		}
	}
	fmt.Printf("netprobe 5: %d frame(s) ontvangen in 10s\n", frames)
	switch {
	case offer:
		fmt.Println("HOPOS_PI5_NET_PAKKET — TX én RX bewezen; een DHCP-server antwoordde HopOS")
	case frames > 0:
		fmt.Println("HOPOS_PI5_NET_RX — RX-DMA bewezen (geen OFFER; DHCP-server traag/afwezig?)")
	default:
		fmt.Println("netprobe 5: geen RX — check TX-status hierboven; DMA-pad nog niet rond")
	}
}

// dhcpDiscover bouwt een minimale maar valide DHCP DISCOVER (ethernet-
// broadcast, IPv4 0.0.0.0→255.255.255.255, UDP 68→67, BOOTP + optie 53;
// UDP-checksum 0 = uitgezet, mag bij IPv4). Broadcast-flag aan zodat de
// OFFER als broadcast terugkomt — onafhankelijk van het RX-unicast-filter.
func dhcpDiscover(mac [6]byte) []byte {
	f := make([]byte, 14+20+8+300)
	for i := range 6 {
		f[i] = 0xff
	}
	copy(f[6:12], mac[:])
	f[12], f[13] = 0x08, 0x00

	ip := f[14:34]
	ip[0], ip[8], ip[9] = 0x45, 64, 17 // IHL 5, TTL, UDP
	tot := len(f) - 14
	ip[2], ip[3] = byte(tot>>8), byte(tot)
	ip[16], ip[17], ip[18], ip[19] = 255, 255, 255, 255
	cs := ipChecksum(ip)
	ip[10], ip[11] = byte(cs>>8), byte(cs)

	udp := f[34:42]
	udp[1], udp[3] = 68, 67 // src/dst-poort (hoge bytes 0)
	ul := tot - 20
	udp[4], udp[5] = byte(ul>>8), byte(ul)

	bp := f[42:]
	bp[0], bp[1], bp[2] = 1, 1, 6                      // BOOTREQUEST, ethernet, hlen
	copy(bp[4:8], []byte{'H', 'O', 'P', '6'})          // xid
	bp[10] = 0x80                                      // broadcast-flag
	copy(bp[28:34], mac[:])                            // chaddr
	copy(bp[236:240], []byte{99, 130, 83, 99})         // DHCP-magic
	copy(bp[240:], []byte{53, 1, 1, 55, 2, 1, 3, 255}) // DISCOVER; vraag subnet+router
	return f
}

// ipChecksum is de standaard 16-bit one's-complement over de IP-header.
func ipChecksum(h []byte) uint16 {
	var s uint32
	for i := 0; i < len(h); i += 2 {
		s += uint32(h[i])<<8 | uint32(h[i+1])
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}

// inspect vat één ontvangen frame samen (bewijsregel per frame).
func inspect(f []byte) string {
	if len(f) < 14 {
		return fmt.Sprintf("kort frame (%d bytes)", len(f))
	}
	return fmt.Sprintf("src %02x:%02x:%02x:%02x:%02x:%02x type %02x%02x len %d",
		f[6], f[7], f[8], f[9], f[10], f[11], f[12], f[13], len(f))
}

// isOffer herkent een DHCP-antwoord (BOOTREPLY op UDP-poort 68) en print
// het aangeboden IP.
func isOffer(f []byte) bool {
	if len(f) < 14+20+8+240 || f[12] != 0x08 || f[13] != 0 || f[23] != 17 {
		return false
	}
	ihl := int(f[14]&0xf) * 4
	udp := f[14+ihl:]
	if len(udp) < 8+240 || udp[2] != 0 || udp[3] != 68 {
		return false
	}
	bp := udp[8:]
	if bp[0] != 2 { // BOOTREPLY
		return false
	}
	fmt.Printf("netprobe 5: DHCP-ANTWOORD — aangeboden IP %d.%d.%d.%d (server %d.%d.%d.%d)\n",
		bp[16], bp[17], bp[18], bp[19], f[26], f[27], f[28], f[29])
	return true
}

// bringup doorloopt Setup → StartLink → OpenBridge voor één controller en
// rapporteert elk beslispunt. Geeft true bij een getrainde link.
func bringup(naam string, rc *brcmpcie.RC) bool {
	fmt.Printf("%s: Setup (bridge-reset, SerDes, windows, 54MHz-PLL via MDIO)...\n", naam)
	rcMode, mdioOK := rc.Setup()
	if !rcMode {
		fmt.Printf("%s: controller strapt NIET als root-complex (status=%#x) — dood spoor\n", naam, rc.Status())
		return false
	}
	fmt.Printf("%s: RC-modus ok, MDIO-PLL-writes bevestigd=%v\n", naam, mdioOK)

	fmt.Printf("%s: PERST# lossen en link pollen (100ms Tpvperl + ≤200ms)...\n", naam)
	phy, dl := rc.StartLink()
	fmt.Printf("%s: PHYLINKUP=%v DL_ACTIVE=%v (status=%#x)\n", naam, phy, dl, rc.Status())
	if !dl {
		return false
	}
	speed, width := rc.LinkSpeedWidth()
	fmt.Printf("%s: link GETRAIND — gen%d x%d\n", naam, speed, width)
	rc.OpenBridge()
	return true
}

func main() {
	rpi5.LEDInit()
	rpi5.LED(true)

	fmt.Println("")
	fmt.Println("HopOS probe6: PCIe-root-complex-bring-up op de Raspberry Pi 5")
	fmt.Printf("runtime %s %s/%s — MPIDR %#x\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, raspi.MPIDR())

	// ── Stap 1: RESCAL (gedeeld, één keer, vóór álle bridge-resets).
	fmt.Println("pcieprobe 1: RESCAL-kalibratie op 0x1000119500 (START → poll STATUS)...")
	if brcmpcie.Rescal(uintptr(rpi5.PCIeRescal)) {
		fmt.Println("pcieprobe 1: RESCAL ok")
	} else {
		fmt.Println("pcieprobe 1: RESCAL bevestigde NIET (meetdata — we proberen door)")
	}

	// ── Stap 2+3: pcie2 → RP1. Inbound: BAR1 = de 4MB-DMA-loopback (RP1's
	// eigen peripherals via PCIe 0x0 — vereist BAR1-toewijzing op 0x0),
	// BAR2 = al het DRAM (PCIe 0x10_0000_0000 → CPU 0x0, 64GB; Linux-
	// conventie, en al de aanname in gem.Net.BusOff).
	rp1 := &brcmpcie.RC{
		Base:     uintptr(rpi5.PCIe2Base),
		SWInit:   uintptr(rpi5.PCIeSWInit),
		SWInitID: rpi5.PCIe2SWInit,
		Gen:      2,
		Out:      brcmpcie.OutWin{CPU: rpi5.RP1Base, PCIe: 0, Size: 0x1000_0000},
		In: []brcmpcie.InWin{
			{PCIe: 0, CPU: rpi5.RP1Base, Size: 0x40_0000},
			{PCIe: 0x10_0000_0000, CPU: 0, Size: 0x10_0000_0000},
		},
	}
	up := bringup("pcieprobe 2 (RP1)", rp1)

	if up {
		// ── Stap 4: enumereren + BAR's. Config-access pas ná DL_ACTIVE.
		fmt.Println("pcieprobe 3: RP1 enumereren (bus 1, dev 0) — verwacht 0x1de4:0x0001...")
		id := rp1.CfgRead32(1, 0, 0, 0)
		fmt.Printf("pcieprobe 3: vendor/device = %04x:%04x (raw %#x)\n", id&0xffff, id>>16, id)

		fmt.Println("pcieprobe 3: BAR's opmeten en toewijzen (BAR1 → PCIe 0x0)...")
		barAssign(rp1, 1, 0)
		rp1.CfgWrite32(1, 0, 0, 0x04, rp1.CfgRead32(1, 0, 0, 0x04)|0x6) // mem+master
		dev.MB()

		// ── Stap 5: de netprobe-metingen, nu tegen een levende RP1.
		fmt.Println("netprobe 1: RP1 CHIP_ID lezen op 0x1f00000000 (vorige meting: 0xdeaddead)...")
		fmt.Printf("netprobe 1: CHIP_ID=%#x PLATFORM=%#x\n",
			dev.Read32(uintptr(rpi5.RP1SysInfo)), dev.Read32(uintptr(rpi5.RP1SysInfo+4)))

		fmt.Println("netprobe 2: GEM module-ID op 0x1f00100000...")
		nic := &gem.Net{Base: uintptr(rpi5.RP1EthBase)}
		fmt.Printf("netprobe 2: GEM module-ID=%#x (verwacht 0x0002xxxx, Cadence)\n", nic.ModuleID())
		fmt.Printf("netprobe 2: SPADDR1 (MAC?) = %#x %#x, CLKGEN=%#x\n",
			dev.Read32(uintptr(rpi5.RP1EthBase)+0x88), dev.Read32(uintptr(rpi5.RP1EthBase)+0x8C),
			dev.Read32(uintptr(rpi5.EthCfgClkGen)))

		// GEMETEN (probe6-run 1, 2026-07-10): link+GEM ok maar géén PHY op de
		// MDIO-bus — de BCM54213PE hangt in reset aan RP1-GPIO32 (actief-laag,
		// 5ms; bcm2712-rpi-5-b.dts phy-reset-gpios). Eerst lossen dus.
		fmt.Println("netprobe 3: PHY-reset via RP1-GPIO32 (laag → 5ms → hoog → 50ms)...")
		rpi5.RP1GPIOOut(32, false)
		time.Sleep(10 * time.Millisecond)
		rpi5.RP1GPIOOut(32, true)
		time.Sleep(50 * time.Millisecond)

		fmt.Println("netprobe 3: MDIO-scan (BCM54213PE = id1 0x600d, verwacht op adres 1)...")
		nic.MDIOEnable()
		if addr, id1, id2, found := nic.PHYScan(); found {
			fmt.Printf("netprobe 3: PHY op adres %d: id1=%#x id2=%#x\n", addr, id1, id2)
			fmt.Println("netprobe 4: autonegotiatie (kabel erin = link; max 8s)...")
			if speed, fd, err := nic.AutoNeg(addr, 8*time.Second); err == nil {
				fmt.Printf("netprobe 4: link %dMbps full-duplex=%v\n", speed, fd)
				dmaTest(nic, speed, fd)
			} else {
				fmt.Printf("netprobe 4: %v (geen kabel? de scan telt)\n", err)
			}
		} else {
			fmt.Println("netprobe 3: geen PHY gevonden (reset via RP1-GPIO nodig? → volgende meting)")
		}
	} else {
		fmt.Println("pcieprobe 3-5: overgeslagen — link kwam niet op; stuur de regels hierboven door")
	}

	// ── Stap 6: pcie1 (FFC → M.2/NVMe). Zit er geen device in, dan traint
	// hij niet — dat onderscheidt "onze sequence faalt" van "slot is leeg":
	// RP1 hierboven is de controle-meting met een device dat er zéker is.
	nvme := &brcmpcie.RC{
		Base:     uintptr(rpi5.PCIeX1Base),
		SWInit:   uintptr(rpi5.PCIeSWInit),
		SWInitID: rpi5.PCIe1SWInit,
		Gen:      2,
		Out:      brcmpcie.OutWin{CPU: rpi5.PCIe1Window, PCIe: 0, Size: 0x1000_0000},
		In: []brcmpcie.InWin{
			{PCIe: 0x10_0000_0000, CPU: 0, Size: 0x10_0000_0000},
		},
	}
	if bringup("nvmeprobe 1 (pciex1)", nvme) {
		id := nvme.CfgRead32(1, 0, 0, 0)
		class := nvme.CfgRead32(1, 0, 0, 0x08) >> 8
		fmt.Printf("nvmeprobe 2: endpoint %04x:%04x klasse %06x (NVMe = 010802)\n",
			id&0xffff, id>>16, class)
	} else {
		fmt.Println("nvmeprobe 2: geen link — leeg slot, of meetdata als RP1 wél trainde")
	}

	fmt.Println("HOPOS_PI5_PCIE_KLAAR — stuur alle regels door; dit kalibreert metal/brcmpcie")
	fmt.Println("klaar — ACT-LED knippert nu als hartslag (1Hz)")
	for {
		rpi5.LED(true)
		time.Sleep(500 * time.Millisecond)
		rpi5.LED(false)
		time.Sleep(500 * time.Millisecond)
	}
}
