//go:build apple

// board_apple.go — de Apple-silicon-kant van de agent-main (Mac mini M4,
// t8132, geboot via m1n1): board-registratie, de RAM-declaratie van de HOP-kern
// en het PA-plan. Zelfde vorm als board_rk3566.go, ander silicium en een andere
// boot-route (geen U-Boot, geen DTB: het param-blok van de loader,
// board/apple/params.go).
//
// Bouwen: AGENT=1 image/apple-m4.sh (-tags "apple linkcpuinit highram",
// -asmflags -D VHE). Laden: image/apple/boot-cycle.sh. Zie
// docs/archief/apple-m4.md.
package main

import (
	"fmt"
	_ "unsafe"

	"github.com/xinix00/HopOS/metal/board/apple"
	_ "github.com/xinix00/HopOS/metal/board/apple/hop" // registreert het board (init)
	"github.com/xinix00/HopOS/metal/cmd/hopos/cfgblob"
)

// De RAM-declaratie van de HOP-kern: 256MB vanaf apple.RamBase (1TiB + 4GB).
// Ruim — HOP zit op ~20MB — maar dit board heeft 24GB en de TLS-apps en
// per-dial buffers van eerder (memory: OOM op een HOP van 64MB) verdienen lucht.
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = apple.RamBase

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = apple.HopRAMSize

func init() {
	// Het PA-plan moet vóór alles staan (slots/stage2 lezen het bij hun eerste
	// gebruik).
	apple.SetupPlan()
	if s := apple.HopStatus(); s != "" {
		fmt.Println(s)
	}

	// De platform-config (hopos.node/cluster/apikey/...): van de loader
	// (CFG=pad image/apple/load-probe.py → apple.CfgBase), of ingebakken als
	// die er niet is. Dezelfde `key=waarde`-regels als hopos.cfg op de Pi's,
	// gelezen door fw/bootcfg. Geen bootargs, geen initrd op dit board.
	//
	// Zodra wíj het bootobject zijn is er geen loader meer om die tekst neer te
	// leggen, en dit board kan zijn bootmedium (nog) niet zelf lezen — dus dan
	// reist de config mee ín het image, precies zoals op de LicheeRV
	// (cmd/hopos/cfgblob, -tags embedcfg). De loader wint als hij er is: een
	// kaart of een kabel wijzigen is makkelijker dan een image bouwen.
	bootParamAll = func(key string) []string {
		if v := apple.BootParamAll(key); len(v) > 0 {
			return v
		}
		return cfgblob.All(key)
	}

	// Node-identiteit-terugval: het serienummer van de machine. Twee nodes op
	// één LAN mogen nooit allebei "hopos-1" heten. Bij voorkeur uit de boom
	// zelf — dat is de bron, en het werkt ook zonder loader; hopos.serial uit
	// de config blijft als terugval staan.
	nodeSerial = func() string {
		s := apple.Serial()
		if len(s) < 8 {
			s = apple.BootParam("hopos.serial")
		}
		if len(s) >= 8 {
			return "hopos-" + s[len(s)-8:]
		}
		return ""
	}

	// De node-watchdog. iBoot laat hem GEWAPEND achter en m1n1 zette hem stil
	// bij zijn eigen opstart (src/main.c, wdt_disable) — dus zolang de loader
	// ertussen zat merkten we niets. Zodra HopOS zelf het bootobject werd aaide
	// niemand hem nog en resette hij de node na ~2 minuten: volledig geboot,
	// netwerk op, app draaiend, en dan weg. Onder m1n1 niet te reproduceren,
	// precies omdat m1n1 hem uitzet (GEMETEN 31-08).
	//
	// Dus nemen we hem over in plaats van hem uit te zetten: HOP-leven =
	// node-leven, en dit board heeft nu hetzelfde vangnet als de Pi's. Het
	// beleid (boot-guard, dan aaien op bewezen leven) staat in watchdog.go;
	// hopos.wd=off blijft de uitweg voor een postmortem.
	nodeWDT = &wdHardware{
		Arm:      apple.WDTArm,
		Pet:      apple.WDTPet,
		PetEvery: apple.WDTPetEvery(),
		Reboot:   apple.WDTReboot,
	}

	// De klokwachter: meldt alleen wanneer een cluster van p-state verandert.
	// Dat is het antwoord op "klokt hij zelf op en neer?" — een vraag die je
	// niet met een aanname mag beantwoorden, ook niet als je de
	// hardware-governor zelf hebt aangezet (Derek, 01-09). Zwijgt volledig op
	// een node die op zijn plafond blijft staan.
	boardExtra = func() { go apple.PStateWatch() }
}
