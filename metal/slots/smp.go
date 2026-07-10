package slots

// SMP (fase 5): een app met cores > 1 draait op één primair slot plus de
// cores-1 cores erna (primair+1 .. primair+cores-1), samen op één gedeelde
// heap. Hoeveel cores een app heeft staat op zijn control-page (CtrlCores, door
// Start gezet) — dat is de enige bron van waarheid, dus er is geen aparte
// reserverings-administratie nodig: de geheugenisolatie zit in de stage-2-kooi
// (hardware), en de capaciteits-accounting (welke cores vrij zijn) doet HOP's
// HopRunner. Start weigert al te beginnen op een core die niet uit is.

import "hop-os/metal/layout"

// appCores geeft álle cores van de app op slot i: de primaire plus, bij een
// SMP-app, zijn secundaire cores. Voor een gewone app is dat gewoon [i]. Zo kan
// het lifecycle-pad (Stop) één keer over de cores lopen, ongeacht of het er één
// of meerdere zijn.
func appCores(i int) []int {
	cores := int(ctrlRead(i, layout.CtrlCores))
	if cores < 1 {
		cores = 1
	}
	all := make([]int, 0, cores)
	for c := i; c < i+cores; c++ {
		all = append(all, c)
	}
	return all
}
