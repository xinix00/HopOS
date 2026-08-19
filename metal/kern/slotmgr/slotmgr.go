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
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/xinix00/hop/pkg/hopos"

	"github.com/xinix00/HopOS/metal/kern/slots"
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

// NumCores is de EERLIJKE app-core-capaciteit die HOP ziet: de PSCI-getelde
// app-cores min de door de node-runtime gereserveerde cores (HopReserved).
// agentboot rapporteert dit als CPUCores, dus een 4-core Pi (core 0 = HOP)
// biedt er 3 aan. Het aantal KOOIEN dat de node kan dragen (AppSlotCount, tot
// SlotCap — sharegroups stapelen méér kooien dan cores) zit BEWUST niet in het
// contract: HOP kent alleen cores, probeert te plaatsen, en de node zegt ja of
// nee. De echte muren (vrije cores in pool.go, RAM in partmem) bewaakt de node.
func (Manager) NumCores() int {
	if n := slots.NumSlots() - slots.HopReserved(); n > 0 {
		return n
	}
	return 0
}

func (Manager) CoreClass(slot int) string { return slots.CoreClass(phys(slot)) }

// PoolLargest vult hopos.PoolReporter: de grootste partitie die er NU nog in
// past, zodat HOP's toelating een job kan weigeren die nergens meer past in
// plaats van hem toe te laten, te reserveren, en de plaatsing te laten falen.
//
// Waarom dit naast NumCores staat en niet erin: cores zijn een vast getal en
// dit is een momentopname. HOP hoeft er niets van te weten om te werken (de
// interface is optioneel) — het scheelt alleen de vijf-seconden-lus waarin de
// node zichzelf intermitterend vol noemt (gemeten 19-08, LicheeRV).
func (Manager) PoolLargest() uint64 { return slots.PoolLargest() }

// StartStream is HET startpad: kooi plaatsen (PlaceCage dekt dedicated én
// sharegroup — een placement-directive net als core-class, geen env-hack) en
// dan de image streamend de partitie in (slots.StartStreamOn).
// Capaciteitstoestanden dragen hopos.ErrNoCapacity zodat HOP ze als pending
// behandelt en niet herstart-stormt — een restart-lus op een onplaatsbare job
// is een storm: elke poging downloadt de image opnieuw en faalt opnieuw. Op
// élk faalpad is de kooi hier al teruggegeven (ReleaseCage) — de aanroeper
// ruimt alleen zijn eigen boekhouding op.
func (Manager) StartStream(slot int, image io.Reader, size int64, spec hopos.StartSpec) error {
	cage := phys(slot)
	core, err := slots.PlaceCage(cage, spec.Sharegroup, spec.PoolCores)
	if err != nil {
		if errors.Is(err, slots.ErrPoolSize) {
			// Geen capaciteitsfout: twee jobspecs in dezelfde sharegroup zijn
			// het oneens over de poolgrootte. Wachten lost dat nooit op, dus
			// dit mag geen "pending" worden — de spec moet veranderen, en dan
			// hoort de operator de reden te zien.
			return err
		}
		return fmt.Errorf("%w: %v", hopos.ErrNoCapacity, err)
	}
	if err := slots.StartStreamOn(core, cage, image, size, spec.MemLimit, spec.Cores,
		spec.Env, spec.Mounts, spec.Ports, spec.Job); err != nil {
		slots.ReleaseCage(cage)
		if errors.Is(err, slots.ErrNoPartition) {
			// Geen partitie-ruimte is een capaciteitstoestand die vanzelf
			// overgaat (een buurman verhuist of stopt): hand-back en pending.
			return fmt.Errorf("%w: %v", hopos.ErrNoCapacity, err)
		}
		return err
	}
	return nil
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
		Cage:      s.Cage,
	}
}

func (Manager) Logs(slot int) <-chan string { return slots.Logs(phys(slot)) }

// Contractbewijs: Manager MOET hopos.SlotManager zijn.
var _ hopos.SlotManager = (*Manager)(nil)
