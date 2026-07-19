// Package slotmgr adapteert HopOS' slot-primitieven (metal/kern/slots) naar het
// SlotManager-contract dat HOP definieert (hop/pkg/hopos) en waar HOP's
// HopRunner op draait. De compile-time assertie onderaan bewijst dat de
// bare-metal kant het contract exact vervult — drift wordt zo een buildfout,
// niet een runtime-verrassing op het board.
//
// Alleen voor GOOS=tamago (het importeert metal/kern/slots → MMIO/PSCI).

//go:build tamago

package slotmgr

import (
	"time"

	"hop/pkg/hopos"

	"hop-os/metal/kern/slots"
)

// Manager implementeert hopos.SlotManager tegen metal/kern/slots.
//
// Slot-vertaling: HOP telt zijn slots 1-based en oblivious; als de node cores
// voor zijn eigen runtime reserveert (slots.SetHopCores), liggen de app-cores
// niet op 1..N maar op (1+HopReserved)..N. Deze adapter is dé (en enige) plek
// die HOP-slot → interne slot vertaalt (intern = HOP-slot + HopReserved), zodat
// slots.* zelf onveranderd op slot=core=layout kan blijven. Bij hopReserved=0
// (default) is phys() de identiteit — geen gedragswijziging.
type Manager struct{}

func New() *Manager {
	usageOnce.Do(startUsage) // de per-slot CPU-meting (usage.go) loopt zolang de node leeft
	return &Manager{}
}

// phys vertaalt een HOP-slot naar de interne slot/core-index.
func phys(slot int) int { return slot + slots.HopReserved() }

func (Manager) NumSlots() int             { return slots.AppSlotCount() }
func (Manager) CoreClass(slot int) string { return slots.CoreClass(phys(slot)) }

// StartLoader plaatst kooi + apploader. De sharegroup-plaatsing zit hier, in de
// adapter: HOP geeft de sharegroup-naam (uit de job-tag) en de poolgrootte
// (uit CPUShares) door — geen env-hack, het is een placement-directive net als
// core-class. PlaceCage kiest de fysieke core (dedicated bij lege sharegroup,
// anders de minst-belaste pool-core); de kooi erft die core via StartLoaderOn →
// StartShared, zodat StartStaged en Stop 'm hergebruiken. Faalt de start, dan
// geeft ReleaseCage de core meteen terug.
func (Manager) StartLoader(slot int, memLimit uint64, sharegroup string, poolCores int, env map[string]string) error {
	cage := phys(slot)
	core, err := slots.PlaceCage(cage, sharegroup, poolCores)
	if err != nil {
		return err
	}
	if err := slots.StartLoaderOn(core, cage, memLimit, env); err != nil {
		slots.ReleaseCage(cage)
		return err
	}
	return nil
}

func (Manager) StartStaged(slot int, memLimit uint64, cores int, env map[string]string, mounts map[string]string, ports map[string]int) error {
	return slots.StartStaged(phys(slot), memLimit, cores, env, mounts, ports)
}

func (Manager) Stop(slot int, timeout time.Duration) error {
	if err := slots.Stop(phys(slot), timeout); err != nil {
		// NIET releasen bij een Stop-fout ("not dead after revocation"): de kooi
		// kan nog een zombie-bewoner in de rotatielijst hebben (na een revoke
		// sterft die pas bij zijn eerstvolgende hervatting — en een compute-buur
		// die nooit yieldt houdt dat venster open). De pool-core opnieuw uitdelen
		// terwijl daar nog leven zit, is een isolatierisico. Fail closed: houd de
		// core gereserveerd; reconcile (of een volgende geslaagde Stop) ruimt op.
		return err
	}
	slots.ReleaseCage(phys(slot)) // pas na een schone Stop: core/pool-boekhouding vrij
	return nil
}

func (Manager) Status(slot int) hopos.SlotStatus {
	s := slots.Get(phys(slot))
	return hopos.SlotStatus{
		CoreOn:    s.CoreOn,
		App:       s.App,
		ExitCode:  s.ExitCode,
		Heartbeat: s.Heartbeat,
		RAMSize:   s.RAMSize,
		MemSys:    s.MemSys,
		CPUPct:    cpuPct(phys(slot)),
		FaultVec:  s.FaultVec,
		FaultESR:  s.FaultESR,
		FaultFAR:  s.FaultFAR,
	}
}

func (Manager) Logs(slot int) <-chan string { return slots.Logs(phys(slot)) }

// Contractbewijs: Manager MOET hopos.SlotManager zijn.
var _ hopos.SlotManager = (*Manager)(nil)
