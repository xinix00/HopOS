// probe7 — de GENET-netprobe voor de Raspberry Pi 4 (fase P2): het hele
// netwerkpad in oplopende spanning, elk punt aangekondigd vóór de mogelijk
// fatale stap (bevriest de UART, dan wijst de laatste regel de dader aan):
//
//  1. SYS_REV_CTRL — versie-nibble hoort 6 te zijn (zo meldt v5 zich);
//  2. MAC-reset (U-Boot-sequence) → MDIO leeft → PHY-scan (BCM54213PE op
//     adres 1, zelfde chip als de Pi 5 — hier zonder reset-GPIO);
//  3. autonegotiatie (kabel erin!);
//  4. ring-16-DMA in de plan-regio (layout.NetDMAPA) — descriptors in
//     registerruimte, buffers in ongecachet DRAM;
//  5. de volledige DHCP-handshake (metal/dhcp, DORA) — een lease is het
//     bewijs dat TX én RX door de hele keten werken.
//
// Slaagt 5, dan is rpi4.ProbeNIC een formaliteit en volgt de HOP-agent.
// Bouwen/flashen: image/rpi4-probe7.sh → sd-rpi4/.
package main

import (
	"fmt"
	"runtime"
	"time"
	_ "unsafe"

	"hop-os/metal/board/raspi"
	"hop-os/metal/board/rpi4"
	"hop-os/metal/dhcp"
	"hop-os/metal/genet"
	"hop-os/metal/layout"
)

// RAM-declaratie: zie probe4 — load 0x80000, text +0x10000, 128MB.
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = 0x00080000

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = 0x08000000

func main() {
	fmt.Println("")
	fmt.Println("HopOS probe7: GENET-netprobe op de Raspberry Pi 4")
	fmt.Printf("runtime %s %s/%s — MPIDR %#x\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, raspi.MPIDR())

	nic := &genet.Net{
		Base: uintptr(rpi4.GENETBase),
		MAC:  [6]byte{0x02, 0x48, 0x4f, 0x50, 0x00, 0x07}, // vaste probe-MAC ("HOP" 07)
	}

	fmt.Println("netprobe 1: SYS_REV_CTRL lezen op 0xFD580000...")
	rev := nic.Rev()
	fmt.Printf("netprobe 1: rev=%#x — versie-nibble %d (verwacht 6 = GENET v5)\n", rev, rev>>24&0xF)

	fmt.Println("netprobe 2: MAC-reset (U-Boot-sequence: sw-reset met loopback) + poort-mux...")
	nic.Reset()
	fmt.Println("netprobe 2: reset klaar — MDIO-scan (BCM54213PE = id1 0x600d, verwacht op adres 1)...")
	addr, id1, id2, found := nic.PHYScan()
	if !found {
		fmt.Println("netprobe 2: geen PHY gevonden — stuur de regels hierboven door")
		fmt.Println("HOPOS_PI4_NETPROBE_KLAAR")
		hang()
	}
	fmt.Printf("netprobe 2: PHY op adres %d: id1=%#x id2=%#x\n", addr, id1, id2)

	fmt.Println("netprobe 3: autonegotiatie (kabel erin = link; max 8s)...")
	speed, fd, err := nic.AutoNeg(addr, 8*time.Second)
	if err != nil {
		fmt.Printf("netprobe 3: %v (geen kabel? de scan telt)\n", err)
		fmt.Println("HOPOS_PI4_NETPROBE_KLAAR")
		hang()
	}
	fmt.Printf("netprobe 3: link %dMbps full-duplex=%v\n", speed, fd)

	fmt.Printf("netprobe 4: ring-16-DMA init (buffers op %#x, ongecachet)...\n", layout.NetDMAPA())
	if err := nic.Init(layout.NetDMAPA(), layout.NetDMASize, speed, fd); err != nil {
		fmt.Printf("netprobe 4: %v\n", err)
		fmt.Println("HOPOS_PI4_NETPROBE_KLAAR")
		hang()
	}
	fmt.Println("netprobe 4: DMA aan — TX_EN|RX_EN gezet")

	fmt.Println("netprobe 5: DHCP-handshake (DISCOVER→OFFER→REQUEST→ACK, max 15s)...")
	if lease, err := dhcp.Acquire(nic, nic.MAC, 15*time.Second); err == nil {
		fmt.Printf("netprobe 5: LEASE — %s via %s (dns %s, server %s)\n",
			lease.CIDR(), lease.GWString(), lease.DNSString(), lease.ServerString())
		fmt.Println("HOPOS_PI4_NET_PAKKET — TX én RX bewezen; de Pi 4 heeft een IP")
	} else {
		fmt.Printf("netprobe 5: %v\n", err)
		fmt.Println("netprobe 5: geen lease — TX of RX nog niet rond; regels hierboven doorsturen")
	}
	fmt.Println("HOPOS_PI4_NETPROBE_KLAAR")
	hang()
}

// hang houdt de runtime levend (PID-1-regel: main keert nooit terug).
func hang() {
	for {
		time.Sleep(time.Hour)
	}
}
