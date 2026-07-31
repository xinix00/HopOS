//go:build licheerv

// board_licheerv.go — de LicheeRV Nano-kant van de agent-main (Sophgo SG2002,
// XuanTie C906): board-registratie en de RAM-declaratie van de HOP-kern, het
// enige dat per board verschilt. Zelfde vorm als board_rpi5.go / board_uefi.go,
// ander silicium — en de eerste niet-ARM.
//
// Bouwen: image/licheerv-agent.sh (GOARCH=riscv64, -tags licheerv).
package main

import (
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
	boardWarn = licheerv.Warn

	// De netwerk-identiteit uit de ingebakken config, vóórdat main de NIC
	// opbrengt. Dit board heeft geen MAC in een fuse, dus zonder deze regel zou
	// élke LicheeRV hetzelfde adres dragen en botsen twee bordjes op één LAN —
	// precies wat de gemengde fleet onmogelijk maakt. hopos.mac heeft voorrang;
	// anders volgt het adres uit hopos.node, die er toch al is.
	licheervhop.UseNetIdentity(
		bootcfg.First(cfgblob.All("hopos.mac")),
		bootcfg.First(cfgblob.All("hopos.node")))

	bootParamAll = cfgblob.All
}

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = licheerv.HopBase

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = licheerv.SlotBase - licheerv.HopBase
