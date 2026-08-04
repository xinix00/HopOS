//go:build licheerv

// board_licheerv.go — de LicheeRV Nano-kant van de agent-main (Sophgo SG2002,
// XuanTie C906): board-registratie en de RAM-declaratie van de HOP-kern, het
// enige dat per board verschilt. Zelfde vorm als board_rpi5.go / board_uefi.go,
// ander silicium — en de eerste niet-ARM.
//
// Bouwen: image/licheerv-agent.sh (GOARCH=riscv64, -tags licheerv).
package main

import (
	"fmt"
	"net"
	"time"
	_ "unsafe"

	"hop-os/metal/board/licheerv"
	licheervhop "hop-os/metal/board/licheerv/hop" // registreert het board (init) + basis-hooks
	"hop-os/metal/cmd/hopos/cfgblob"
	"hop-os/metal/fw/bootcfg"
)

// De platform-config komt op dit board uit het image zelf. Elders komt hij van
// buiten — de Pi's uit de device-tree van de firmware, de UEFI-stub uit
// hopos.cfg op de ESP — maar hier is er geen van beide: de vendor-FSBL geeft ons
// niets mee, en de FAT-bootpartitie waar fip.bin op staat kunnen we zelf niet
// lezen (dat vraagt een SDHCI- plus FAT-driver). Dus bakt
// image/licheerv-agent.sh de config in (-tags embedcfg, zie cfgblob) en is hij
// deel van het ondertekende image: nieuw image = nieuwe config.
//
// Zonder ingebakken config is er dus geen config, en dat is precies wat de main
// dan hoort te zien: hij parkeert zijn API luid (HOPOS_API_NO_AUTH) en de node
// blijft leven. Een `-tags licheerv`-build ZONDER embedcfg synthetiseerde hier
// eerder hopos.insecure=1, en dat holde die fail-closed-controle uit: een build
// waarvan de bouwer niets gezegd had zette de auth-poort open. De voorbeeldconfig
// (image/hopos-licheerv.cfg) kiest die opt-out expliciet — dat is waar zo'n
// beslissing hoort te staan, in tekst die een operator leest.
//
// Zodra de FAT wél gelezen kan worden hoort dié de voorkeur te krijgen (een kaart
// in een laptop aanpassen is makkelijker dan een image bouwen) en blijft de
// ingebakken config de terugval.
func init() {
	// Dit board heeft geen hardware-entropiebron, en sinds de agent over TLS
	// artifacts haalt is dat een operator-beslissing in plaats van een voetnoot.
	// Daarachter de wekker-probe van het app-hart: die MOET op dat hart zelf
	// draaien (hartprobe.go) en hoort dus bij wat dit board bij boot over
	// zichzelf vaststelt — vóór het eerste slot-werk, dat via HartWaker op de
	// uitslag leunt.
	boardWarn = func() {
		licheerv.Warn()
		licheervhop.ProbeAppHart()
	}

	// De netwerk-identiteit uit de ingebakken config, vóórdat main de NIC
	// opbrengt. Dit board heeft geen MAC in een fuse, dus zonder deze regel zou
	// élke LicheeRV hetzelfde adres dragen en botsen twee bordjes op één LAN —
	// precies wat de gemengde fleet onmogelijk maakt. hopos.mac heeft voorrang;
	// anders volgt het adres uit hopos.node, die er toch al is.
	licheervhop.UseNetIdentity(
		bootcfg.First(cfgblob.All("hopos.mac")),
		bootcfg.First(cfgblob.All("hopos.node")))

	bootParamAll = cfgblob.All

	// De node-watchdog: HOP-leven = node-leven, en het levensteken is de les
	// van 02-08. De gemeten doofheid (nieuwe verbindingen en ICMP dood,
	// gevestigde flows en álle HOP-lussen kerngezond) was voor een
	// onvoorwaardelijke aaier à la de Pi onzichtbaar geweest — dus aait deze
	// canary alleen als de node zijn levensteken haalt: een nieuwe verbinding
	// naar de eigen agent-poort, dwars door dezelfde gvisor-accept-laag waar
	// de doofheid zat. Stopt dat, dan stopt het aaien en reset de DW-WDT de
	// hele SoC (~86s later); de ctx-veeg maakt van die reset een schone start.
	go nodeCanary()
}

// nodeCanary bewaakt het levensteken en voedt de hardware-watchdog. Volgorde
// en vangnetten:
//
//   - gewapend wordt er pas ná het éérste geslaagde levensteken: een kapotte
//     bring-up blijft staan voor de operator in plaats van eeuwig te cyclen;
//   - het WDT-blok moet door de hart-1-probe bewezen zijn (WdtUsable) — HOP
//     zelf overleeft een MMIO-fout op een dood blok niet, dat hart wel;
//   - hopos.wd=off schakelt de hele canary uit, dezelfde knop als op de Pi.
//
// Het doeladres is het vaste gateway-adres (10.100.0.1) met de agent-poort:
// DHCP-onafhankelijk, en de agent luistert wildcard dus lokaal bezorgbaar.
// Beperking, eerlijk genoteerd: deze self-dial loopt niet door de
// NIC-inbound-demux — een doofheid die uitsluitend dáár huist, mist hij.
func nodeCanary() {
	if bootcfg.First(cfgblob.All("hopos.wd")) == "off" {
		return
	}
	const target = "10.100.0.1:8080" // agentboot's vaste agent-poort
	probe := func(timeout time.Duration) bool {
		c, err := net.DialTimeout("tcp", target, timeout)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
	for !probe(2 * time.Second) {
		time.Sleep(5 * time.Second)
	}
	if !licheervhop.WdtUsable() {
		fmt.Println("watchdog: WDT block unproven by the hart probe — node liveness is UNGUARDED")
		return
	}
	licheerv.WatchdogArm()
	misses := 0
	for {
		time.Sleep(20 * time.Second)
		if probe(3 * time.Second) {
			licheerv.WatchdogPet()
			misses = 0
			continue
		}
		misses++
		fmt.Printf("watchdog: liveness probe failed (%d in a row) — withholding the pet; hardware reset follows unless the node recovers HOPOS_CANARY_MISS\n", misses)
	}
}

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = licheerv.HopBase

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = licheerv.SlotBase - licheerv.HopBase
