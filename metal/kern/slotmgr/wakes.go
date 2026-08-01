// wakes.go: het cijfer dat cpu% pas bruikbaar maakt.
//
// Aanleiding (31-07). Twee lege apps op één hart lazen allebei 36% busy en dat
// leek een meetfout: "hij telt het schakelen als werk". Dat is het niet — de
// meting klopt (zie de kop van usage.go), de apps zijn écht bezig. Maar met
// alleen een percentage kun je niet zien WAARMEE, en dus ook niet of het erg is:
// 36% aan één rekenklus is een app die werk doet, 36% aan duizenden keren per
// seconde wakker worden om niets te vinden is verspilde stroom.
//
// Daarom telt de app-kant zijn idle-rondes (metal/cpu/idle/wakes.go → CtrlWakes)
// en rekent deze lezer het om naar de twee getallen die het verschil laten zien:
// wekken per seconde, en hoeveel eigen tijd één wek kost. Dat laatste is de
// diagnose in één getal — een wek die tientallen microseconden kost op een hart
// dat bij élke wissel zijn TLB leegt, is de prijs van de wissel en niet van het
// werk.
//
// Alleen op de HopOS-console en niet in de SlotStatus-RPC: dit is een
// bring-up-meting, en het contract met HOP (hop/pkg/hopos) uitbreiden voor een
// getal dat morgen misschien nul is, is de verkeerde volgorde.

//go:build tamago

package slotmgr

import (
	"log"
	"time"
)

// wakeReport is de periode tussen twee regels — een veelvoud van usageSample,
// want per 5s zou dit een logstorm zijn en de trend zit in minuten, niet in
// seconden.
const wakeReport = 6 // × usageSample = 30s

type wakeState struct {
	last  uint64 // vorige CtrlWakes-stand
	seen  bool
	round int
}

var wakeSeen [len(usagePct)]wakeState

// reportWakes logt hoe vaak slot i wakker werd en wat één wek hem kostte.
// busy = de tikken die dit slot in het venster NIET idle was; tickHz maakt er
// tijd van. Zwijgt tot hij twee standen heeft en zwijgt over een slot dat niet
// wakker wordt — een stille app hoeft geen regel per halve minuut.
func reportWakes(i int, wakes, busy, tickHz uint64) {
	if i < 0 || i >= len(wakeSeen) {
		return
	}
	w := &wakeSeen[i]
	if !w.seen || wakes < w.last {
		w.seen, w.last, w.round = true, wakes, 0
		return
	}
	d := wakes - w.last
	w.last = wakes
	if w.round++; w.round < wakeReport {
		return
	}
	w.round = 0
	if d == 0 {
		return
	}
	// d en busy beslaan allebei precies één usageSample — de teller loopt elke
	// ronde mee, alleen het LOGGEN is getemperd. Dus geen wakeReport in deze som.
	perSec := d * uint64(time.Second) / uint64(usageSample)
	perWake := time.Duration(busy*uint64(time.Second)/tickHz) / time.Duration(d)
	log.Printf("slot %d: %d wakeups/s, %v of own time per wakeup", i, perSec, perWake.Round(time.Microsecond))
}
