// De kern-flip-regressie (docs/kern-flip.md, image/qemu-run.sh flip): kern A
// start een app, haalt de flip-bundel van de host (10.0.2.2 = de Mac in
// QEMU-slirp) en springt erin; de geflipte kern B — dezelfde build, herbaseerd
// naar een uit de pool geleend venster — ADOPTEERT die app en draait daarna de
// VOLLEDIGE demo af.
//
// Dát is de acceptatie, in twee delen:
//
//   - de app moet de wissel niet gemerkt hebben: zijn heartbeat loopt door
//     over de sprong heen (dezelfde teller, geen herstart) en zijn logs komen
//     na de flip gewoon weer binnen bij de nieuwe servicer;
//   - de nieuwe kern moet daarna nog alles kunnen: de hele demo-suite, met de
//     geadopteerde app nog levend in slot 1.
//
// De fetch is hier een kale GET: dit is de meetbank, en de bundel komt van de
// host ernaast. Het echte pad op een board is kernflip.FlipFromURL, die de
// sha256 uit de platform-config eist vóór hij ook maar iets plaatst.

//go:build qemuvirt

package main

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"time"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/kern/kernflip"
	"github.com/xinix00/HopOS/metal/kern/slots"
	"github.com/xinix00/HopOS/metal/net/hopswitch"
)

// flipMode/flipURL komen als -X binnen (image/qemu-run.sh flip); leeg = deze
// regressie bestaat niet in deze build.
var (
	flipMode string
	flipURL  string
)

// flipSlot is de bewoner die de flip moet overleven; flipHand draagt de
// handoff van kern A naar B (gezet door flipAdopt, gelezen door flipVerify).
const flipSlot = 1

var flipHand kernflip.Handoff

// flipLogs telt de logregels van de bewoner: zijn ronde-teller uit de
// outkeep-sessie. Loopt hij ná de flip door, dan komen de antwoorden nog
// steeds terug en is de NAT-mapping écht overgedragen.
var flipLogs int

// flipAdopt draait ZEER VROEG in main (vóór slots/stage2 iets initialiseren):
// het consumeert het handoff-blob en zet de adoptie-stand, zodat InitVectors
// de plan-regio van de levende bewoners met rust laat.
func flipAdopt() {
	// Deze bank flipt, dus de node moet flip-baar zijn: de switch-code die een
	// app-core uitvoert verhuist naar de plan-regio. Op een echte node komt
	// deze stand uit de platform-config (hopos.flip.enable); hier is de
	// build-vlag de config. Vóór álles, want de beslissing moet vaststaan vóór
	// de eerste kooi-init.
	slots.SetFlipCapable(true)
	if h, ok := kernflip.Adopted(); ok {
		flipHand = h
		start, end := runtime.MemRegion()
		fmt.Printf("HOPOS_FLIP_BOOT — flipped kernel alive at %#x..%#x (borrowed window %#x+%dMB, previous kernel window %#x+%dMB, %d resident(s) handed over)\n",
			start, end, h.Window, h.Total>>20, h.OldBase, h.OldSize>>20, len(h.Slots))
	}
}

// flipGens is hoe vaak deze regressie flipt. TWEE, en dat is geen luxe: pas
// bij de tweede flip is het venster dat teruggegeven wordt écht geleende
// pool-grond (het eerste kern-venster is op elk board een plan-hole). Daarmee
// bewijst de bank het hele leen-model — lenen, springen, teruggeven — én dat
// dezelfde bewoner twee kernwissels onder zich door overleeft.
const flipGens = 2

// flipDemo draait ná hopswitch.Up(): in kern A start hij de app en springt,
// in elke volgende kern adopteert hij wat er nog leeft en flipt zo nodig door.
func flipDemo() {
	if flipHand.Window != 0 {
		flipVerify()
	} else {
		// Kern A: een app met drie dingen die de flip moeten overleven — een
		// gepubliceerde poort (die komt uit het slot-record terug), een gemount
		// volume (MOUNTCHECK schrijft en leest er elke ronde in; is het
		// mount-punt na de wissel weg, dan faalt die write) en een
		// LANGLEVENDE uitgaande sessie (NETDEMO=outkeep: één UDP-socket die
		// elke twee seconden een DNS-query doet). Die tweede is het echte
		// NAT-bewijs: raakt de conntrack-mapping bij de wissel kwijt, dan vindt
		// het antwoord de weg terug niet meer en valt de app om met
		// "NAT-mapping weg?" — precies wat een cloudflared-tunnel zou doen.
		fmt.Println("flip: starting a resident with a live outbound session that has to survive the flip...")
		mustStart("flip-resident", flipSlot, 64<<20, 1,
			map[string]string{"ROLE": "flip-survivor", "NETDEMO": "outkeep", "MOUNTCHECK": "/data/flip.txt"},
			map[string]string{"/data": "/data"}, map[string]int{"http": 8080}, &flipLogs)
		mustReady("flip-resident", flipSlot, 5*time.Second)
		// Even laten praten: de conntrack-mapping moet bestaan vóór we springen.
		time.Sleep(2500 * time.Millisecond)
		if n := len(hopswitch.SnapshotNAT().Flows); n == 0 {
			fail("flip-nat", fmt.Errorf("geen enkele NAT-flow vóór de flip — de regressie zou de overdracht niet bewijzen"))
		}
		fmt.Printf("flip: resident in slot %d is ready (heartbeat %d), %d NAT flow(s) live\n",
			flipSlot, slots.Get(flipSlot).Heartbeat, len(hopswitch.SnapshotNAT().Flows))
	}

	if kernflip.Generation() >= flipGens {
		// Klaar met flippen: de bewoner heeft ze allemaal overleefd en mag weg,
		// zodat de rest van de demo zijn slots vrij heeft.
		mustStop("flip-resident-stop", flipSlot, 3*time.Second)
		// De kooi ook teruggeven. In het agent-pad doet slotmgr dat bij zijn
		// Stop; deze demo gebruikt slots.Start/Stop rechtstreeks, dus hier met
		// de hand. Zonder dit blijft de core van de geadopteerde bewoner als
		// bezet geboekt — precies wat adoptCage bedoelt, maar dan voor altijd.
		slots.ReleaseCage(flipSlot)
		fmt.Printf("HOPOS_FLIP_OK — resident survived %d kernel flips; borrow-and-return proven\n", flipGens)
		return
	}

	fmt.Printf("flip: fetching %s (generation %d → %d)\n", flipURL, kernflip.Generation(), kernflip.Generation()+1)
	resp, err := http.Get(flipURL)
	if err != nil {
		fail("flip-fetch", err)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	resp.Body.Close()
	if err != nil {
		fail("flip-fetch", err)
	}
	fmt.Printf("flip: bundle is %d bytes — flipping with a live resident\n", len(b))
	if err := kernflip.Flip(b); err != nil {
		fail("flip", err)
	}
}

// flipVerify is de kern-B-kant: de bewoners overnemen en bewijzen dat er niets
// van ze gemist is.
func flipVerify() {
	live := slots.AdoptSlots(flipHand.Slots)
	flows := hopswitch.RestoreNAT(flipHand.NAT)
	// De geadopteerde bewoner heeft een VERSE servicer (AdoptSlots startte hem),
	// dus ook een vers logkanaal — precies zoals na elke Start. De drain hoort
	// er dus opnieuw op: die van kern A ging met kern A mee. In het agent-pad
	// gebeurt dat vanzelf, want HOP's LogBroadcaster vraagt zijn kanaal per
	// slot op wanneer hij het nodig heeft.
	go drainLogs(flipSlot, &flipLogs)
	if flows != len(flipHand.NAT.Flows) {
		fail("flip-adopt", fmt.Errorf("%d van %d NAT-flows overleefden de flip", flows, len(flipHand.NAT.Flows)))
	}
	if live != len(flipHand.Slots) {
		fail("flip-adopt", fmt.Errorf("%d van %d bewoners overleefden de flip", live, len(flipHand.Slots)))
	}
	if live == 0 {
		fail("flip-adopt", fmt.Errorf("de flip droeg geen bewoners over — de regressie bewijst dan niets"))
	}
	// Het bewijs dat hij niet herstart is: de app-status is nog READY (een
	// verse app zou door BOOTING moeten) en de heartbeat LOOPT door — met een
	// stand die hoger ligt dan de nul waar een herstarte app op begint.
	s := slots.Get(flipSlot)
	if s.App != layout.StatusReady || s.Heartbeat == 0 {
		fail("flip-adopt", fmt.Errorf("slot %d: status=%d heartbeat=%d na de flip", flipSlot, s.App, s.Heartbeat))
	}
	hb := s.Heartbeat
	time.Sleep(600 * time.Millisecond)
	if s = slots.Get(flipSlot); s.Heartbeat <= hb {
		fail("flip-adopt", fmt.Errorf("slot %d: heartbeat staat stil na de flip (%d → %d)", flipSlot, hb, s.Heartbeat))
	}
	fmt.Printf("HOPOS_FLIP_ADOPT_OK — resident slot %d never stopped (generation %d): status=READY, heartbeat %d→%d across the kernel flip\n",
		flipSlot, flipHand.Gen, hb, s.Heartbeat)

	// En het NAT-bewijs: de app moet ná de wissel nog steeds antwoorden krijgen
	// op de socket die hij vóór de wissel opende. Zijn ronde-teller loopt door
	// in de logs; raakte de mapping kwijt, dan exit hij met "NAT-mapping weg?"
	// en ziet de status hieronder dat meteen.
	before := flipLogs
	time.Sleep(3 * time.Second)
	s = slots.Get(flipSlot)
	if s.App != layout.StatusReady {
		fail("flip-nat", fmt.Errorf("slot %d: status=%d exit=%d na de flip — de uitgaande sessie overleefde de wissel niet",
			flipSlot, s.App, s.ExitCode))
	}
	if flipLogs <= before {
		fail("flip-nat", fmt.Errorf("slot %d: geen nieuwe ronde gelogd na de flip (%d) — het antwoord vindt de weg terug niet meer",
			flipSlot, flipLogs))
	}
	fmt.Printf("HOPOS_FLIP_NAT_OK — %d conntrack flow(s) carried over: the resident's outbound session kept getting answers (%d → %d rounds)\n",
		flows, before, flipLogs)

	// Het geleende model sluit zijn cirkel zónder dat iemand iets teruggeeft:
	// deze kern claimde bij zijn poolInit alleen zijn éigen venster, dus het
	// venster van de vorige kern is gewoon weer vrije pool (zie de notitie in
	// kern/slots/adopt.go — expliciet teruggeven was een dubbeluitgifte). Wat
	// hier gemeten wordt is dus of de pool ná de flip nog de volle capaciteit
	// draagt; het échte bewijs is de swarm verderop, die 24 × 48 MB plaatst.
	fmt.Printf("HOPOS_FLIP_POOL_OK — previous kernel window (%#x+%dMB) is pool again: largest placeable %d MB\n",
		flipHand.OldBase, flipHand.OldSize>>20, slots.PoolLargest()>>20)
}
