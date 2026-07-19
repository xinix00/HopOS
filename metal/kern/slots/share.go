package slots

// Coöperatieve core-deling (fase 6): meerdere slots — elk met eigen kooi,
// partitie en netstack — delen één fysieke core. De EL2-switch
// (cpu/el2/switch.s) wisselt op de WFE-yields van de idle-governor; HOP doet
// hier alleen de boekhouding: welk slot op welke core woont (hostCore) en de
// bewonerslijst in het per-core sched-blok (layout.Sched*) die de
// EL2-rotatie round-robin afloopt. HOP is de enige lijst-schrijver; de
// EL2-switch schrijft alleen cursor en ctx-staten — SPSC-discipline zoals
// bij de ringen.
//
// V1-regels: alleen één-core-apps delen (SMP-apps krijgen eigen cores), en
// een compute-app die nooit yieldt starft zijn buren — per ontwerp: compute
// hoort op een eigen core, en HOP's liveness ziet de gestokte heartbeats.

import (
	"fmt"
	"time"

	"hop-os/metal/abi/layout"
	"hop-os/metal/dev"
)

// hostCore koppelt een slot aan zijn fysieke core (0 = eigen core: slot i
// woont op core i, het klassieke één-op-één-model). Gezet door StartShared,
// gewist door releaseSlot; gedimensioneerd in poolInit (partmem.go).
var hostCore []int

// coreOf geeft de fysieke core waar slot i woont.
func coreOf(i int) int {
	if i >= 1 && i < len(hostCore) && hostCore[i] != 0 {
		return hostCore[i]
	}
	return i
}

// ctxPA is het switch-contextblok van slot i (in zijn stage-2-tabelblok).
func ctxPA(i int) uintptr { return layout.Stage2TablePA(i) + layout.CtxOff }

// ctxState leest het levensteken van slot i (layout.Ctx*-waarden; de
// EL2-switch schrijft Running/Saved/Dead, HOP Empty/BootPending/Running).
func ctxState(i int) uint64 { return dev.Read64(ctxPA(i) + layout.CtxState) }

// ctxLive: deze staat betekent "bezig" (boot-pending, gesaved of draaiend).
func ctxLive(st uint64) bool {
	return st >= layout.CtxBootPending && st <= layout.CtxRunning
}

// StartShared start slot i als (mede)bewoner van fysieke core. Is de core
// geparkeerd of cold, dan is dit gewoon het klassieke startschot op een
// andere core dan het slotnummer; draait hij al, dan komt het slot er via
// het ctx-blok bij zonder de buren te storen. Eén core per slot (SMP-apps
// delen niet); de image wordt geplaatst zoals bij Start.
func StartShared(core, i int, image []byte, memLimit uint64, env map[string]string, mounts map[string]string, ports map[string]int) error {
	if err := checkSlot(i); err != nil {
		return err
	}
	if core < 1 || core > layout.NumAppCores() {
		return fmt.Errorf("shared core %d out of range 1..%d", core, layout.NumAppCores())
	}
	partOnce.Do(poolInit)
	hostCore[i] = core
	err := startImage(i, image, memLimit, 1, env, mounts, ports, true)
	if err != nil {
		hostCore[i] = 0
	}
	return err
}

// residentReset maakt slot i de enige bewoner van core en zet zijn staat op
// Running — het startschot (mailbox/PSCI) volgt direct hierna. Alleen voor
// een niet-draaiende core (geparkeerd of cold): er leest geen rotatie mee.
func residentReset(core, i int) {
	b := layout.ParkMboxPA(core)
	dev.Write64(b+layout.SchedCursor, 0)
	dev.Clear(b+layout.SchedList, uint64(layout.SlotCap))
	dev.Write8(b+layout.SchedList, uint8(i))
	dev.Write64(b+layout.SchedCount, 1)
	dev.Write64(ctxPA(i)+layout.CtxState, layout.CtxRunning)
	dev.MB()
	refreshShared(core)
}

// residentAdd hangt slot i in de bewonerslijst van core: het eerste gat
// (0-byte), anders append. Entry vóór count, met barrière — de EL2-rotatie
// mag nooit een half geschreven staart zien.
func residentAdd(core, i int) {
	b := layout.ParkMboxPA(core)
	n := dev.Read64(b + layout.SchedCount)
	if n > uint64(layout.SlotCap) {
		n = uint64(layout.SlotCap) // defensief: rommel nooit als lijstlengte volgen
	}
	// Al lid? (twee-fase-start: de loader stond al in de lijst, ctx nu Dead) —
	// niet dubbel toevoegen; fase 2 flipt straks alleen de ctx-staat.
	for k := uint64(0); k < n; k++ {
		if dev.Read8(b+layout.SchedList+uintptr(k)) == uint8(i) {
			refreshShared(core)
			return
		}
	}
	for k := uint64(0); k < n; k++ {
		if dev.Read8(b+layout.SchedList+uintptr(k)) == 0 {
			dev.Write8(b+layout.SchedList+uintptr(k), uint8(i))
			dev.MB()
			refreshShared(core)
			return
		}
	}
	dev.Write8(b+layout.SchedList+uintptr(n), uint8(i))
	dev.MB()
	dev.Write64(b+layout.SchedCount, n+1)
	dev.MB()
	refreshShared(core)
}

// residentRemove haalt slot i uit de lijst van core (gat achterlaten; de
// rotatie slaat 0-bytes over, residentAdd hergebruikt ze).
func residentRemove(core, i int) {
	b := layout.ParkMboxPA(core)
	n := dev.Read64(b + layout.SchedCount)
	if n > uint64(layout.SlotCap) {
		n = uint64(layout.SlotCap)
	}
	for k := uint64(0); k < n; k++ {
		if dev.Read8(b+layout.SchedList+uintptr(k)) == uint8(i) {
			dev.Write8(b+layout.SchedList+uintptr(k), 0)
		}
	}
	dev.MB()
	refreshShared(core)
}

// refreshShared zet het CtrlShared-woord van elke lévende bewoner van core:
// 1 als er ≥2 zijn (ieders idle-governor yieldt dan via HVC zodat de buren
// draaien), anders 0 (de laatste bewoner is weer dedicated en WFE't puur voor
// power). Aangeroepen na elke lijst-mutatie — HOP is de enige schrijver van
// dit woord, de app leest het alleen.
func refreshShared(core int) {
	b := layout.ParkMboxPA(core)
	n := dev.Read64(b + layout.SchedCount)
	if n > uint64(layout.SlotCap) {
		n = uint64(layout.SlotCap)
	}
	var live []int
	for k := uint64(0); k < n; k++ {
		s := int(dev.Read8(b + layout.SchedList + uintptr(k)))
		if s != 0 && s <= layout.MaxSlots && ctxLive(ctxState(s)) {
			live = append(live, s)
		}
	}
	v := uint64(0)
	if len(live) >= 2 {
		v = 1
	}
	for _, s := range live {
		dev.Write64(layout.CtrlPagePA(s)+layout.CtrlShared, v)
	}
	dev.MB()
}

// slotShares: woont er op de core van slot i nog een ándere levende bewoner?
// Bepaalt het Stop-pad: gedeeld = op de ctx-staat wachten (de core parkeert
// niet — de buren leven door), alleen = het klassieke parkeer-pad.
func slotShares(i int) bool {
	b := layout.ParkMboxPA(coreOf(i))
	n := dev.Read64(b + layout.SchedCount)
	if n > layout.SlotCap {
		n = layout.SlotCap
	}
	for k := uint64(0); k < n; k++ {
		e := int(dev.Read8(b + layout.SchedList + uintptr(k)))
		if e != 0 && e != i && e <= layout.MaxSlots && ctxLive(ctxState(e)) {
			return true
		}
	}
	return false
}

// bootPendingDispatch zet slot i klaar als boot-pending bewoner van een
// drááiende core: {ctrl-page, trampoline} in het ctx-blok, staat →
// boot-pending, en in de lijst — de EL2-rotatie cold-boot hem bij de
// eerstvolgende yield van een buur (~ms; de rotatie zet de staat dan op
// Running vóór de trampoline-sprong, dus de poll hieronder ziet het).
//
// Eén race bestaat er echt: de rotatie las de lijst nét vóór onze append en
// parkeerde de core (de laatste buur stierf precies nu). Daarom de poll met
// dubbelrol: wordt de core geparkeerd gezien, dan alsnog het gewone
// mailbox-startschot; blijft de staat boot-pending op een drááiende core,
// dan yieldt daar niets (compute-buur) en falen we luid met teruggedraaide
// boekhouding.
func bootPendingDispatch(core, i int, tramp, ctx uint64) error {
	c := ctxPA(i)
	dev.Write64(c+layout.CtxBootCtx, ctx)
	dev.Write64(c+layout.CtxBootPC, tramp)
	dev.MB()
	dev.Write64(c+layout.CtxState, layout.CtxBootPending)
	dev.MB()
	residentAdd(core, i)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if dev.Read64(c+layout.CtxState) != layout.CtxBootPending {
			return nil // opgepikt: de rotatie boot(te) hem
		}
		if !coreRunning(core) {
			// Park-race: zelf dispatchen. Staat éérst op Running, anders zou
			// de rotatie hem straks nógmaals cold-booten (dubbelboot).
			dev.Write64(c+layout.CtxState, layout.CtxRunning)
			dev.MB()
			return dispatchCore(core, tramp, ctx)
		}
		time.Sleep(time.Millisecond)
	}
	// Niet opgepikt: boekhouding terugdraaien. Won hij de race tóch nog
	// (staat al Running), dan hoort hij juist terug in de lijst.
	residentRemove(core, i)
	dev.MB()
	if dev.Read64(c+layout.CtxState) != layout.CtxBootPending {
		residentAdd(core, i)
		return nil
	}
	dev.Write64(c+layout.CtxState, layout.CtxEmpty)
	dev.MB()
	return fmt.Errorf("slot %d: core %d never yielded to boot its new resident (compute-bound neighbour?)", i, core)
}

// waitCtxDead polt tot de EL2-switch slot i dood heeft gemeld (exit, fault
// of revoke) — het gedeelde-core-equivalent van waitStopped.
func waitCtxDead(i int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st := ctxState(i); st == layout.CtxDead || st == layout.CtxEmpty {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// CoreIdle meldt of een fysieke core geen enkele app draait (geparkeerd of
// cold) — voor regressies/diagnose die een CORE toetsen waar Get een SLOT
// toetst (op een gedeelde core zegt de core-mailbox niets over één bewoner).
func CoreIdle(core int) bool { return !coreRunning(core) }
