// watchdog.go — HET beleid van de node-watchdog, één keer voor élk board.
//
// Dit bestand bestaat omdat er vier kopieën van dit beleid waren gegroeid, en
// ze waren uit elkaar gelopen (12-08 gezien): de Pi's en UEFI wapenden vroeg
// maar aaiden BLIND — precies de aaier die de gemeten doofheid van 02-08
// (nieuwe verbindingen en ICMP dood, álle interne lussen kerngezond) nooit
// gezien zou hebben. De LicheeRV en de Radxa aaiden wél op bewezen leven, maar
// wapenden pas laat en dreven onderling af (de een meldde luid dat hij nog
// niet gewapend was, de ander zweeg). Beleid hoort één keer te bestaan; per
// board blijven alleen de twee hardware-regels over: hoe wapen ik, hoe aai ik.
//
// Het ene beleid, in twee fasen:
//
//  1. BOOT-GUARD — wapenen zo vroeg als de hardware het toelaat, en blind
//     aaien tot het eerste levensteken. Een node die tijdens de boot volledig
//     bevriest (ook de aai-goroutine) reset zichzelf — de Pi-eigenschap, nu
//     overal. Een bring-up die lééft maar geen netwerk krijgt (geen kabel,
//     geen lease) blijft gewoon staan: het blinde aaien gaat door en de
//     wachtregel hieronder zegt periodiek waarom er nog geen echt vangnet is.
//  2. LEVENSTEKEN — vanaf de eerste geslaagde probe aait de canary alleen nog
//     op bewijs: een NIEUWE verbinding naar de eigen agent-poort, dwars door
//     dezelfde accept-laag waar de doofheid van 02-08 zat. Stopt dat, dan
//     stopt het aaien en reset de hardware de node (HOP-leven = node-leven).
//
// Het probe-adres is het EIGEN externe IP (vers uit board.Net() per poging,
// dus lease-wissels volgen vanzelf). Niet 10.100.0.1: dat adres is vanuit de
// kérn onbezorgbaar (locdev's GwFromHost is per ontwerp slot≥1 — het is het
// app-model) en heeft daardoor op de LicheeRV twee boots lang stil niet
// gewapend. En niet via een hairpin door de netstack: die routeert
// off-subnet via de gáteway, en een watchdog die aan de gateway hangt maakt
// van een dode router een reboot-loop op een verder kerngezonde node. Het
// eigen IP loopt door locdev's self-dial-naad (ARP naar het eigen adres
// beantwoordt locdev zelf) — nul externe afhankelijkheden, en het is exact
// het pad waarmee de leader jobs naar de agent dispatcht, dus op ijzer
// doorlopend bewezen. Beperking, eerlijk genoteerd: deze self-dial loopt
// niet door de NIC-inbound-demux — een doofheid die uitsluitend dáár huist,
// mist hij.
//
// hopos.wd=off schakelt alles uit, op elk board dezelfde knop: voor een
// JTAG/UART-postmortem moet een bevroren node blijven stáán.
package main

import (
	"fmt"
	"net"
	"time"

	"github.com/xinix00/HopOS/metal/v2/board"
)

// wdHardware is de hardware-helft die een board in zijn init() aanlevert.
// nil (de default) betekent: dit board bedraadt geen watchdog — QEMU virt,
// en elk board waarvan het blok nog niet gemeten is. De reset-scope MOET
// gemeten zijn vóór een board dit zet (de les van de Radxa, 06-08: een
// gewapende watchdog die de node níét reset is erger dan geen watchdog,
// want dan denk je dat je een vangnet hebt).
type wdHardware struct {
	// Arm wapent de hardware, één keer. desc komt in de "armed"-regel te
	// staan (blok + timeout, zodat de console zegt wát er waakt); ok=false
	// betekent onbruikbaar (blok niet aanwezig, probe-uitslag negatief) en
	// de reden staat dan in desc.
	Arm func() (desc string, ok bool)
	// Pet laadt de hardware-teller opnieuw.
	Pet func()
	// PetEvery is de aai-cadans, gekozen bij de hardware-timeout: ruim
	// eronder (cadans + probe-timeout van 3s moet er comfortabel in passen),
	// niet zó strak dat één trage probe al een reset is.
	PetEvery time.Duration
	// Reboot laat de hardware NU resetten (optioneel): de weg van
	// hopos.reboot=1. Ontbreekt hij, dan wapent rebootNow en aait niet.
	Reboot func()
}

// rebootNow is de "CRASH-object"-knop (Derek, 02-09): een kern-flip naar een
// image met hopos.reboot=1 is een herstart-verzoek op afstand — de geflipte
// kern doet niets anders dan de watchdog laten afgaan, en de node komt schoon
// terug in zijn geïnstalleerde image. Voor een meetbank zonder iemand bij de
// knop; het enige wat een flip anders niet kan is een core die vast staat
// weer vrij krijgen.
func rebootNow() {
	if nodeWDT == nil {
		fmt.Println("hopos.reboot=1 but this board wires no hardware watchdog — cannot reboot, continuing HOPOS_REBOOT_UNAVAILABLE")
		return
	}
	if nodeWDT.Reboot != nil {
		fmt.Println("hopos.reboot=1: resetting the node via the watchdog NOW HOPOS_REBOOT")
		nodeWDT.Reboot()
	} else if desc, ok := nodeWDT.Arm(); ok {
		fmt.Printf("hopos.reboot=1: watchdog armed and never petted (%s) — reset follows HOPOS_REBOOT\n", desc)
	} else {
		fmt.Printf("hopos.reboot=1 but the watchdog refuses (%s) — continuing HOPOS_REBOOT_UNAVAILABLE\n", desc)
		return
	}
	for {
		time.Sleep(time.Second)
	}
}

// nodeWDT wordt door de board-init gezet; main start de canary.
var nodeWDT *wdHardware

// nodeCanary voert het beleid uit. Gestart vanuit main ná boardWarn (de
// boards die hun blok eerst moeten bewijzen — de hart-probe op de LicheeRV —
// hebben dat dan gedaan).
func nodeCanary() {
	if bootParam("hopos.wd") == "off" {
		fmt.Println("watchdog: not armed (hopos.wd=off) — node liveness is UNGUARDED, " +
			"a frozen node stays up for a post-mortem instead of reset-cycling.")
		return
	}
	if nodeWDT == nil {
		fmt.Println("watchdog: this board wires no hardware watchdog — node liveness is UNGUARDED")
		return
	}
	desc, ok := nodeWDT.Arm()
	if !ok {
		fmt.Printf("watchdog: %s — node liveness is UNGUARDED\n", desc)
		return
	}
	fmt.Printf("watchdog: hardware reset armed (%s) — boot guard: petting unconditionally until the agent port answers\n", desc)

	// De probe: een nieuwe verbinding naar de eigen agent-poort. Het adres
	// per poging vers gelezen — vóór de lease is er geen adres en dus geen
	// levensteken, en dat is de eerlijke uitkomst.
	probe := func(timeout time.Duration) bool {
		ip := board.Current().Net().IP
		if ip == "" {
			return false
		}
		c, err := net.DialTimeout("tcp", ip+":8080", timeout)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}

	// Fase 1: blind aaien tot het eerste levensteken, en het wachten luid
	// zeggen — stilte was hier de verkeerde default (11-08: twee hele boots
	// een niet-gewapende watchdog en niemand kon het weten). Elke ~5 minuten
	// een regel, plus één vroege na ~3 rondes.
	loudEvery := int(5 * time.Minute / nodeWDT.PetEvery)
	if loudEvery < 1 {
		loudEvery = 1
	}
	for n := 0; !probe(3 * time.Second); n++ {
		if n == 3 || n%loudEvery == loudEvery-1 {
			fmt.Printf("watchdog: no liveness sign from own agent port yet (%d attempts) — boot guard only: a full freeze resets, deafness does not yet\n", n+1)
		}
		nodeWDT.Pet()
		time.Sleep(nodeWDT.PetEvery)
	}
	fmt.Println("watchdog: liveness proven — pets now require a fresh connection to the agent port HOPOS_CANARY_LIVE")

	// Fase 2: aaien op bewijs. Eén gemiste probe is een luide melding, een
	// aanhoudende doofheid wordt een hardware-reset zodra de teller afloopt.
	misses := 0
	for {
		time.Sleep(nodeWDT.PetEvery)
		if probe(3 * time.Second) {
			nodeWDT.Pet()
			misses = 0
			continue
		}
		misses++
		fmt.Printf("watchdog: liveness probe failed (%d in a row) — withholding the pet; hardware reset follows unless the node recovers HOPOS_CANARY_MISS\n", misses)
	}
}
