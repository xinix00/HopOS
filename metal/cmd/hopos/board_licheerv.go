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
	"time"
	_ "unsafe"

	"github.com/xinix00/HopOS/metal/board/licheerv"
	licheervhop "github.com/xinix00/HopOS/metal/board/licheerv/hop" // registreert het board (init) + basis-hooks
	"github.com/xinix00/HopOS/metal/cmd/hopos/cfgblob"
	"github.com/xinix00/HopOS/metal/fw/bootcfg"
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
// waarvan de bouwer niets gezegd had zette de auth-poort open. De ingebakken
// default (image/hopos-headless.cfg — de headless-template van álle boards)
// kiest die opt-out expliciet — dat is waar zo'n beslissing hoort te staan, in
// tekst die een operator leest.
//
// Zodra de FAT wél gelezen kan worden hoort dié de voorkeur te krijgen (een kaart
// in een laptop aanpassen is makkelijker dan een image bouwen) en blijft de
// ingebakken config de terugval.
func init() {
	// Dit board heeft geen hardware-entropiebron, en sinds de agent over TLS
	// artifacts haalt is dat een operator-beslissing in plaats van een voetnoot.
	// Daarachter de probe van de kleine core: die MOET op dat hart zelf draaien
	// (hartprobe.go) en hoort dus bij wat dit board bij boot over zichzelf
	// vaststelt — vóór het eerste slot-werk, dat via HartTimer op de uitslag
	// leunt, en vóór de hop hieronder, die er zijn go/no-go uit haalt.
	boardWarn = func() {
		if licheervhop.LotteryRescued() {
			fmt.Println("lottery: WARNING — HopCore never came up; running on the firmware core with swapped bookkeeping, placements WILL fail. HOPOS_LOTTERY_RESCUED")
		}
		licheerv.Warn()
		licheervhop.ProbeSmallCore()
	}

	// De rolwissel (HOP op de kleine core) loopt sinds het loterij-voorstel
	// niet meer via een teleport maar via de boot-hart-loterij
	// (board/licheerv/hop/lottery.go): de wissel is dan al gebeurd vóór de
	// eerste Go-instructie. NOG TE BEDRADEN (integratiekaart, ledger r.48):
	// hartLottery in het cpuinit-pad + de adoptie van de geparkeerde grote
	// core (slots.HopHandoff(1) → switcher-entry in het adoptie-woord).
	// Tot die naad af is staat HopHart op 0 en is dit een no-op.

	// De netwerk-identiteit uit de ingebakken config, vóórdat main de NIC
	// opbrengt. Dit board heeft geen MAC in een fuse, dus zonder deze regel zou
	// élke LicheeRV hetzelfde adres dragen en botsen twee bordjes op één LAN —
	// precies wat de gemengde fleet onmogelijk maakt. hopos.mac heeft voorrang;
	// anders volgt het adres uit hopos.node, die er toch al is.
	licheervhop.UseNetIdentity(
		bootcfg.First(cfgblob.All("hopos.mac")),
		bootcfg.First(cfgblob.All("hopos.node")))

	bootParamAll = cfgblob.All

	// De hardware-helft van de node-watchdog; het beleid woont in watchdog.go,
	// één keer voor alle boards. Twee dingen zijn hier boards-eigen: het
	// WDT-blok moet door de hart-1-probe bewezen zijn (WdtUsable — HOP zelf
	// overleeft een MMIO-fout op een dood blok niet, dat hart wel; main start
	// de canary ná boardWarn, dus die uitslag is er dan), en de trage cadans
	// bij de lange timeout: de DW-WDT loopt hier op ~86s, dus 20s aaien laat
	// drie gemiste levenstekens zien vóór de reset.
	nodeWDT = &wdHardware{
		Arm: func() (string, bool) {
			if !licheervhop.WdtUsable() {
				return "WDT block unproven by the hart probe", false
			}
			licheerv.WatchdogArm()
			return "DW-WDT via RTC glue, ~86s", true
		},
		Pet:      licheerv.WatchdogPet,
		PetEvery: 20 * time.Second,
	}
}

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = licheerv.HopBase

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = licheerv.HopSize
