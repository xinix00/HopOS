package slots

// SMP (fase 5): een app met cores > 1 draait op één primair slot plus de
// cores-1 cores erna (primair+1 .. primair+cores-1), samen op één gedeelde
// heap. De geheugenisolatie zit in de stage-2-kooi (hardware), en de
// capaciteits-accounting (welke cores vrij zijn) doet HOP's HopRunner. Start
// weigert al te beginnen op een core die niet uit is.
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
// (slotCores), gezet uit Start's al-gevalideerde `cores`-argument. dispatchSMP
// en coreCount lezen HIER, nooit ctrlRead(CtrlCores). Start blíjft CtrlCores op
// de page schrijven (de app-OS-laag en de dvfs-governor lezen 'm) — alleen de
// readback wordt nooit vertrouwd.

// slotCores is HOP's vertrouwde bron van waarheid voor het aantal cores per
// slot: gezet door Start (uit het al-gevalideerde `cores`-argument), gewist door
// releaseSlot. 0 = geen actieve reservering. Enkel de HOP-kern (core 0) muteert
// en leest deze array — net als partOf — dus Go-synchronisatie is niet nodig.
// slotCores is een slice (lazy op layout.MaxSlots+1 gedimensioneerd in
// poolInit, ná board.SetMaxSlots) i.p.v. een vaste array — MaxSlots is nu
// runtime (het board volgt zijn ontdekte cores).
var slotCores []int

// coreCount geeft het vertrouwde core-aantal van slot i (minstens 1). Nooit uit
// de app-schrijfbare control-page — zie de pakketnoot hierboven.
func coreCount(i int) int {
	if i < 1 || i >= len(slotCores) {
		return 1
	}
	c := slotCores[i]
	if c < 1 {
		c = 1
	}
	return c
}
