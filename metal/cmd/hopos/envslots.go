package main

import (
	"github.com/xinix00/hop/pkg/hopos"
	"io"
)

// envSlots vult de slot-env aan bij elke start: `always` gaat er altijd in
// (mits de jobspec de sleutel niet zelf zet — de spec wint), `optin` alleen
// als de jobspec de sleutel leeg declareert ("HOPOS_APPS":"" → HopOS vult
// hem). Zo krijgt elke app HOPOS_HOST gratis, maar betaalt alleen wie erom
// vraagt de env-ruimte van de app-catalogus. Puur een schil om de slotmgr —
// de rest van het SlotManager-contract gaat er onaangeroerd doorheen.
type envSlots struct {
	hopos.SlotManager
	always map[string]string
	optin  map[string]string
}

// PoolLargest reist niet mee via de ingebedde interface — hopos.PoolReporter
// staat bewust NAAST SlotManager — dus geeft de schil hem expliciet door. Zonder
// dit ziet HOP's toelating de optionele interface niet en valt hij terug op de
// som, wat precies het gedrag is dat we wilden weghalen.
func (e envSlots) PoolLargest() uint64 {
	if pr, ok := e.SlotManager.(hopos.PoolReporter); ok {
		return pr.PoolLargest()
	}
	return 0
}

func (e envSlots) StartStream(slot int, image io.Reader, size int64, spec hopos.StartSpec) error {
	spec.Env = e.merge(spec.Env)
	return e.SlotManager.StartStream(slot, image, size, spec)
}

func (e envSlots) merge(env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+len(e.always))
	for k, v := range env {
		out[k] = v
	}
	for k, v := range e.always {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	for k, v := range e.optin {
		if cur, ok := out[k]; ok && cur == "" {
			out[k] = v
		}
	}
	return out
}

// The embedded SlotManager omits optional placement hints. Preserve the
// physical free-run query so a shared pool does not hide an available SMP run.
func (e envSlots) CanPlaceDedicated(slot, cores int) bool {
	if placement, ok := e.SlotManager.(hopos.DedicatedPlacement); ok {
		return placement.CanPlaceDedicated(slot, cores)
	}
	return true // no hint: StartStream remains the placement authority
}

var _ hopos.DedicatedPlacement = envSlots{}
