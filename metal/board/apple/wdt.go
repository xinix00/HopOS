//go:build tamago && arm64

// wdt.go — Apple's watchdog: van de firmware overnemen in plaats van uitzetten.
//
// DIT BLOK IS WAARAAN DE NODE 31-08 EEN AVOND LANG STIERF. iBoot laat de
// watchdog gewapend achter, en zolang m1n1 ertussen zat merkten we dat niet:
// zijn `main.c` roept `wdt_disable()` aan bij zijn eigen opstart, dus onder de
// loader stond hij altijd al uit. Zodra HopOS zélf het bootobject werd, aaide
// niemand hem meer — en dan reset hij de machine na ongeveer twee minuten.
// Het beeld daarvan was gemeen: de node boot volledig, netwerk werkt, de app
// draait, en dan is hij weg. Onder m1n1 was het niet te reproduceren, precies
// omdat m1n1 hem uitzet.
//
// Uitzetten zou ook kunnen (dat doen m1n1 en Linux), maar dat is hier de
// verkeerde keuze: HopOS wíl een node-watchdog. Het ontwerpprincipe is
// HOP-leven = node-leven — bevriest HOP, dan hoort de node te herstarten en
// niet stil te blijven staan met in-memory staat die niemand meer verzorgt.
// Het beleid staat al in cmd/hopos/watchdog.go; een board levert alleen deze
// twee regels: hoe wapen ik, hoe aai ik.
//
// Registers (m1n1 src/wdt.c, ADT /arm-io/wdt): COUNT 0x10, ALARM 0x14, CTL
// 0x1c. Aaien = COUNT op nul; wapenen = ALARM op de timeout en CTL bit 2. De
// teller loopt op de referentieklok van dit blok, en die is 24MHz — dat is de
// enige waarde die niet uit de boom te lezen valt, dus hij staat hier met de
// meting erbij in plaats van als aanname.
package apple

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

const (
	wdtCount = 0x10
	wdtAlarm = 0x14
	wdtCtl   = 0x1c

	// CTL bit 2 = "reset de machine als de teller de alarmwaarde haalt".
	// Dezelfde bit die m1n1's wdt_reboot zet om een herstart af te dwingen.
	wdtCtlReset = 1 << 2

	// De tellerklok. Apple's WDT loopt op 24MHz op elke generatie waar dit
	// gemeten is; de ADT noemt hem niet. Zit hij er ooit naast, dan is het
	// gevolg een timeout die een factor verschilt — vandaar dat de "armed"-
	// regel de gekozen waarde afdrukt, zodat een verkeerde aanname in de
	// console staat en niet in een stille reset.
	wdtHz = 24_000_000

	// Timeout en cadans. Ruim boven de aai-cadans van het beleid, en ruim
	// boven de 3s probe-timeout die daarin zit.
	wdtTimeout  = 30 * time.Second
	wdtPetEvery = 5 * time.Second
)

var wdtBase uintptr

// WDTQuiet zet ÉLKE watchdog van dit blok stil, onvoorwaardelijk.
//
// Er zijn er meer dan één, en dat is de val waar 31-08 een dag in ging zitten.
// Wij wapenden de primaire netjes en aaiden hem trouw — en de machine bleef
// resetten op exact 1:43, want de andere stond nog zoals iBoot hem achterliet
// en die aait niemand. Onder m1n1 is dat onzichtbaar: zijn wdt_disable() zet ze
// allemaal uit, dus met de loader ertussen merk je er niets van.
//
// m1n1 doet de tweede alleen bij `wdt-version` 2 of 3. Wij doen ELK venster dat
// de boom noemt, ongeacht wat er over de versie in staat: een watchdog die je
// per ongeluk laat staan reset je node, en een register nullen dat toch al nul
// was kost niets. De vorm komt van m1n1 (src/wdt.c): het primaire venster stop
// je met CTL (0x1c) op nul, de volgende met een nul op offset 0.
//
// Geeft de console-regel terug, en die moet ook in de FOUTGEVALLEN kloppen:
// lukt het stilzetten niet, dan reset deze node over twee minuten en is dit de
// regel waar iemand naar zit te staren. Dus niet "silenced" beweren als er
// niets stilgezet is.
func WDTQuiet() string {
	t, ok := ADT()
	if !ok {
		return "firmware watchdogs NOT silenced — no device tree"
	}
	chain, ok := t.PathTrace("/arm-io/wdt")
	if !ok {
		return "firmware watchdogs NOT silenced — no /arm-io/wdt in the device tree"
	}
	n := chain[len(chain)-1]
	out := ""
	for i := 0; ; i++ {
		base, _, ok := t.RegAt(chain, i)
		if !ok || base == 0 {
			if i == 0 {
				return "firmware watchdogs NOT silenced — no reg windows on /arm-io/wdt"
			}
			out += fmt.Sprintf(" (%d window(s), wdt-version %d)", i, t.U32(n, "wdt-version", 0))
			break
		}
		if i == 0 {
			dev.Write32(uintptr(base)+wdtCtl, 0)
			out += fmt.Sprintf("primary %#x", base)
		} else {
			dev.Write32(uintptr(base), 0)
			out += fmt.Sprintf(", reg[%d] %#x", i, base)
		}
		dev.MB()
	}
	return "firmware watchdogs silenced — " + out
}

// WDTArm wapent de watchdog. desc gaat naar de console-regel van het beleid.
func WDTArm() (string, bool) {
	base := ADTReg("/arm-io/wdt", 0)
	if base == 0 {
		return "no /arm-io/wdt in the device tree", false
	}
	wdtBase = uintptr(base)

	ticks := uint32(wdtTimeout / time.Second * wdtHz)
	dev.Write32(wdtBase+wdtCount, 0)
	dev.Write32(wdtBase+wdtAlarm, ticks)
	dev.Write32(wdtBase+wdtCtl, wdtCtlReset)
	dev.MB()

	// Teruglezen, want een gewapende watchdog die níét reset is erger dan geen
	// watchdog (de les van de Radxa): dan denk je dat je een vangnet hebt.
	if dev.Read32(wdtBase+wdtCtl)&wdtCtlReset == 0 {
		dev.Write32(wdtBase+wdtCtl, 0)
		return fmt.Sprintf("Apple WDT at %#x refuses to arm (CTL reads %#x)", base, dev.Read32(wdtBase+wdtCtl)), false
	}
	return fmt.Sprintf("Apple WDT at %#x, %s timeout at %dMHz", base, wdtTimeout, wdtHz/1_000_000), true
}

// WDTPet laadt de teller opnieuw.
func WDTPet() {
	if wdtBase == 0 {
		return
	}
	dev.Write32(wdtBase+wdtCount, 0)
	dev.MB()
}

// WDTPetEvery is de cadans die bij bovenstaande timeout hoort.
func WDTPetEvery() time.Duration { return wdtPetEvery }

// WDTReboot laat de watchdog NU afgaan: alarm op één tik, teller op nul,
// reset-bit aan — m1n1's wdt_reboot, en de hardware-helft van hopos.reboot=1.
// Keert niet terug; blijft er tóch iets over, dan zegt de aanroeper dat.
func WDTReboot() {
	base := ADTReg("/arm-io/wdt", 0)
	if base == 0 {
		return
	}
	dev.Write32(uintptr(base)+wdtAlarm, 1)
	dev.Write32(uintptr(base)+wdtCount, 0)
	dev.Write32(uintptr(base)+wdtCtl, wdtCtlReset)
	dev.MB()
	for {
	}
}

// WDTDisable is WDTQuiet onder de naam die het beleid kent: de uitweg voor
// bring-up (hopos.wd=off). Een bevroren node die blijft stáán is te
// onderzoeken, een node die reset-cyclet niet.
func WDTDisable() { WDTQuiet() }
