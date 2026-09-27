package slots

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/dev"
)

// SMP apps own one cage and a contiguous physical core span. The primary CPU
// uses the cage context; each secondary uses a separate per-core context.
// Physical ownership is maintained by the node core allocator.
//
// SECURITY (isolatie-invariant): het aantal cores van een app MAG NIET uit de
// control-page (CtrlCores) worden teruggelezen voor vertrouwensbeslissingen.
// Die page ligt pageRW in de stage-2-kooi van de app (stage2.go), dus een
// kwaadwillende app kan CtrlCores herschrijven. Zou HOP hem terugvertrouwen,
// dan kon de app:
//   - CtrlCores ophogen en zo via CtrlSMPReq buurcores (van een ander slot) in
//     zijn eigen kooi laten dispatchen (dispatchSMP-bereikcheck);
//   - CtrlCores verlagen zodat Stop's stillOn-scan levende secundaire cores mist
//     en releaseSlot een partitie vrijgeeft waarvan cores nog draaien.
// Daarom houdt HOP een eigen, vertrouwde per-slot core-telling in HOP-geheugen
// (smpCores), gezet uit Start's al-gevalideerde `cores`-argument. dispatchSMP
// en coreCount lezen HIER, nooit ctrlRead(CtrlCores). Start blíjft CtrlCores op
// de page schrijven (de app-OS-laag en de dvfs-governor lezen 'm) — alleen de
// readback wordt nooit vertrouwd.

// smpCores is HOP's vertrouwde bron van waarheid voor het aantal cores per
// slot: gezet door Start (uit het al-gevalideerde `cores`-argument), gewist door
// releaseSlot. 0 = geen actieve reservering. Enkel de HOP-kern (core 0) muteert
// en leest deze array — net als partOf — dus Go-synchronisatie is niet nodig.
// smpCores is een slice (lazy op layout.MaxSlots+1 gedimensioneerd in
// poolInit, ná board.SetMaxSlots) i.p.v. een vaste array — MaxSlots is nu
// runtime (het board volgt zijn ontdekte cores).
var smpCores []int

// coreCount geeft het vertrouwde core-aantal van slot i (minstens 1). Nooit uit
// de app-schrijfbare control-page — zie de pakketnoot hierboven.
func coreCount(i int) int {
	if i < 1 || i >= len(smpCores) {
		return 1
	}
	c := smpCores[i]
	if c < 1 {
		c = 1
	}
	return c
}

// smpContext returns a CPU's context identity within this cage.
func smpContext(slot, core int) int {
	if core == coreOf(slot) {
		return slot
	}
	return layout.SMPContextID(core)
}

// SMPPending laat per levend slot zien of er een ONBEANTWOORD core-verzoek
// staat (de console-vraag `cores`). Dat getal is het verschil tussen de twee
// verklaringen als een SMP-app bevriest, en van buiten is het anders niet te
// zien: de app-runtime spint in goos.Task op CtrlSMPReq tot HOP hem nul maakt
// (cpu/smp), dus één gemist verzoek verbrandt een core op 100% zonder dat de
// app nog één logregel schrijft. Staat er een nummer, dan vroeg de app en
// zweeg HOP; staat er 0, dan heeft de app nooit gevraagd en ligt het aan de
// app. Precies die vraag bleef 21-09 op de O6N open bij een app met vijf
// cores: drie secundairen kwamen op, de vierde kreeg geen enkele regel.
func SMPPending() string {
	out := ""
	for i := 1; i <= layout.MaxSlots; i++ {
		if !ctxLive(ctxState(i)) {
			continue
		}
		out += fmt.Sprintf(" [slot %d: core %d, %d core(s), smpreq=%d]",
			i, coreOf(i), coreCount(i), ctrlRead(i, layout.CtrlSMPReq))
	}
	return out
}

// prepareSMPContexts initializes the complete trusted sibling chain before
// dispatch. Secondary CPUs share the app's memory but never its CPU context.
func prepareSMPContexts(slot, count int) {
	first := coreOf(slot)
	for core := first; core < first+count; core++ {
		id := smpContext(slot, core)
		if core != first {
			dev.Clear(ctxPA(id), layout.CtxLen)
			// Geen wekdoel tot deze core zelf geyield heeft. Moet expliciet,
			// want het gewiste blok staat op 0 en dat is de affiniteit van
			// fysieke core 0 — zie layout.CtxKickNone. Een schakel voor een
			// core die de app-runtime nooit opvraagt blijft hier staan, en
			// ving zonder dit de wekken van de primaire op.
			ctxWrite(id, layout.CtxKickTarget, layout.CtxKickNone)
			ctxWrite(id, layout.CtxCtrlPA, ctxRead(slot, layout.CtxCtrlPA))
			ctxWrite(id, layout.CtxRingHeadPA, ctxRead(slot, layout.CtxRingHeadPA))
			ctxWrite(id, layout.CtxUnitSlot, uint64(slot))
		}
		next := slot
		if core+1 < first+count {
			next = smpContext(slot, core+1)
		}
		ctxWrite(id, layout.CtxNextPA, uint64(ctxPA(next)))
	}
}
