//go:build rk3566

// board_rk3566.go — de Radxa Zero 3E-kant van de agent-main (Rockchip RK3566,
// 4× Cortex-A55): board-registratie, de RAM-declaratie van de HOP-kern en de
// platform-config uit de bootargs die U-Boot meegaf. Zelfde vorm als
// board_rpi5.go / board_licheerv.go, ander silicium.
//
// Bouwen: image/radxa-zero3.sh (-tags rk3566). Boot-route: U-Boot booti met een
// arm64-Image; zie docs/archief/radxa-zero3.md voor de gemeten adressen.
package main

import (
	"fmt"
	"time"
	_ "unsafe"

	"github.com/xinix00/HopOS/metal/board/rk3566"
	rk3566hop "github.com/xinix00/HopOS/metal/board/rk3566/hop" // registreert het board (init)
)

// De RAM-declaratie van de HOP-kern: het venster uit het PA-plan
// (board/rk3566/plan.go). 64MB is ruim — HOP zelf zit op ~20MB van zijn
// venster — en het laat de rest van de 2GB aan de partitie-pool.
//
// De node-watchdog: beleid in cmd/hopos/watchdog.go (één keer voor alle
// boards), hier alleen de hardware-helft — zie de init onderaan.
//
// DE RESET-SCOPE IS GEMETEN (06-08) en dat is de toelatingseis voor de
// nodeWDT-bedrading: of een WDT-timeout op de RK3566 de HÉLE node reset is
// niet uit de Linux-bron te bewijzen — mainline heeft geen Rockchip-glue voor
// dit blok, geen reset-scope-bit, geen GRF-veld. En een gewapende watchdog die
// de node níét reset is erger dan geen watchdog: dan denk je dat je een
// vangnet hebt. Op de LicheeRV kostte precies die aanname een nacht (een kale
// DW-enable bleek daar niet genoeg, de reset-routering in het RTC-domein moest
// er eerst bij).
//
// WAT ER GEMETEN IS, met de probe op `hopos.wdtest=1`: kortste timeout gewapend,
// niet geaaid. Op de console volgde géén enkele "still alive"-regel maar direct
// `DDR V1.18 ...` — de DDR-init uit de boot-ROM, gevolgd door SPL, ATF en
// U-Boot. Dat is een volledige node-reset door de hele bootketen heen, niet een
// subsysteem dat omvalt. Daarmee is de bedrading verdiend.

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = rk3566.RamBase

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = 0x04000000 // 64MB

func init() {
	// Het PA-plan moet vóór alles staan (slots/stage2 lezen het bij hun eerste
	// gebruik) en leest zelf de DTB voor de pool.
	rk3566.SetupPlan()

	// De platform-config (hopos.node/cluster/apikey/...) komt hier uit de
	// bootargs die U-Boot uit de extlinux-APPEND doorgaf — GEMETEN 05-08 dat die
	// route werkt, net als de initrd-route voor een volle hopos.cfg. Zelfde
	// mechanisme als de cmdline op de Pi's, andere bron.
	bootParamAll = rk3566.BootParamAll

	// De netwerk-identiteit, vóórdat main de NIC opbrengt. Dit board heeft geen
	// MAC in een fuse waarvan wij de registerkaart gemeten hebben, dus zou élke
	// Radxa Zero 3E anders hetzelfde adres dragen en botsen twee bordjes op één
	// LAN. hopos.mac heeft voorrang; anders volgt het adres uit hopos.node, die
	// er toch al is. Zelfde mechanisme als op de LicheeRV (metal/net/nodemac).
	rk3566hop.UseNetIdentity(
		rk3566.BootParam("hopos.mac"),
		rk3566.BootParam("hopos.node"))

	// De hardware-helft van de node-watchdog; beleid in watchdog.go. De
	// gemeten timeout komt in de "armed"-regel (zie het meetverhaal boven);
	// PetEvery 20s past daar ruim onder.
	nodeWDT = &wdHardware{
		Arm: func() (string, bool) {
			secs, fixed := rk3566.WatchdogArm()
			return fmt.Sprintf("DW-WDT, measured %.1fs, fixed-top %v", secs, fixed), true
		},
		Pet:      rk3566.WatchdogPet,
		PetEvery: 20 * time.Second,
	}

	// De DRBG van de jitter-seed naar het hardware-TRNG tillen.
	//
	// WAAROM DIT EEN GOROUTINE IS en niet gewoon een aanroep hier: init() draait
	// nog vóór main, en dáár hoort geen MMIO naar een blok waarvan de klok niet
	// bewezen open is — dat kostte op 06-08 een boot die stierf vóór de banner
	// (een ongeklokt Rockchip-blok geeft geen abort maar houdt de bus vast; zie
	// board/rk3566/rng.go). Een goroutine die hier gestart wordt loopt pas als de
	// scheduler draait, dus na main.
	//
	// WAAROM ONVOORWAARDELIJK en niet achter een sleutel zoals de watchdog: het
	// verschil is dat deze meting WÉL groen is. Drie boots op rij gaven twee
	// onafhankelijke rondes uit het blok; de watchdog-reset-scope is daarentegen
	// nog nooit gemeten. Faalt het TRNG alsnog, dan blijft de jitter-seed staan
	// en zegt de melding dat — nul-entropie komt er nooit door (zie Fill).
	go func() {
		if src, ok := rk3566.UseHardwareRNG(); ok {
			fmt.Printf("rng: DRBG reseeded from %s (boot seed was timing jitter)\n", src)
		} else {
			fmt.Printf("rng: hardware TRNG unusable (%s) — DRBG stays on the jitter seed\n", src)
		}
	}()

	// Node-identiteit-terugval: het board-serial uit de DTB. Twee nodes op één
	// LAN mogen nooit allebei "hopos-1" heten.
	nodeSerial = func() string {
		if s := rk3566.SerialSuffix(); s != "" {
			return "hopos-" + s
		}
		return ""
	}
}
