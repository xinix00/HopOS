package slots

// DE WEKKER: het HOP-einde van "een app-core slaapt op EL2 en HOP wekt hem"
// (layout.IdleYield, cpu/el2/switch.s, board.Cores.Kick). De tegenhanger
// van idle in het contract is wekken, en op silicium waar een app-core
// zichzelf niet kan wekken — geen werkende WFE, geen FIQ die een WFI wekt
// (de M4, gemeten 02-09) — is HOP de enige die het kan. HOP's eigen core
// wordt toch al elke milliseconde wakker op zijn timer; deze lus rijdt mee.
//
// Per slot met een geyielde bewoner (ctx-staat Saved) twee vragen:
//
//   - is zijn wektijd (CtxWake, door de switcher uit de yield bewaard)
//     verstreken?
//   - ligt er RX: is zijn doorbell gewapend (CtrlRXDoor) en groeide de ring?
//     Dezelfde peek als de rotatie zelf doet zodra hij wakker is.
//
// Ja op één van beide = Kick op de core. De switcher wordt wakker, ackt de
// IPI, en de rotatie hervat de bewoner. Een kick te veel is een geackte FIQ
// op EL2 en verder niets. Dit is m1n1's park-recept op ditzelfde silicium
// (wfi + fast IPI), met de wekker in HOP in plaats van in een spin-table.

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/net/hopswitch"
)

// rxArmed is het gewapend-teken van de doorbell (cpu/idle/rxdoor.go, bit 63).
const rxArmed = uint64(1) << 63

// StartWaker start de wekker — alleen op een board dat kan kicken, en ná de
// vectoren: de kick is (op Apple) een HVC naar HOP's eigen EL2-handler.
func StartWaker() {
	if cores().Kick == nil {
		return
	}
	vectorsOnce.Do(cageInit)
	hopswitch.SetRXWake(wakeRX)
	go waker()
	fmt.Println("idle: waker on — HOP kicks app cores that sleep on WFI HOPOS_WAKER_UP")
}

// De tellers van de wekker (hopos.idlestat leest ze): rondes, slapende
// bewoners gezien, en kicks. Zonder deze drie is "de app slaapt en wordt
// nooit gewekt" niet te onderscheiden van "de app spint" — beide zijn 100%.
var wakerRounds, wakerArmed, wakerKicks, directRXKicks atomic.Uint64

// WakerStats geeft (rondes, slapend gezien, kicks) — cumulatief.
func WakerStats() (rounds, armed, kicks uint64) {
	return wakerRounds.Load(), wakerArmed.Load(), wakerKicks.Load()
}

// DirectRXKicks telt de doelgerichte leeg→niet-leeg-kicks van de switch.
func DirectRXKicks() uint64 { return directRXKicks.Load() }

func wakeRX(i int) {
	core := coreOf(i)
	if slotShares(i) || !coreRunning(core) || ctxState(i) != layout.CtxSaved || !rxDue(i) {
		return
	}
	if phys := physCore(core); phys >= 0 {
		cores().Kick(phys)
		directRXKicks.Add(1)
	}
}

func waker() {
	kick := cores().Kick
	for {
		time.Sleep(time.Millisecond)
		wakerRounds.Add(1)
		now := dev.Counter()
		for i := 1; i <= NumSlots(); i++ {
			core := coreOf(i)
			if slotShares(i) || !coreRunning(core) {
				continue // gedeelde cores wekt de rotatie; een geparkeerde core slaapt niet
			}
			if ctxState(i) != layout.CtxSaved {
				continue // draait, boot, of dood: niets te wekken
			}
			wakerArmed.Add(1)
			if !wakeDue(i, now) {
				continue
			}
			if phys := physCore(core); phys >= 0 {
				kick(phys)
				wakerKicks.Add(1)
			}
		}
	}
}

// wakeDue: is de geyielde bewoner van slot i due — op tijd (CtxWake, 0 =
// meteen), of op RX?
func wakeDue(i int, now uint64) bool {
	t := ctxRead(i, layout.CtxWake)
	nopeek := t&layout.CtxWakeNoPeek != 0
	t &^= layout.CtxWakeNoPeek
	if t == 0 || now >= t {
		return true
	}
	if nopeek {
		return false // een wachter zonder P: alleen zijn wektijd of een kick (HVC #4)
	}
	return rxDue(i)
}

func rxDue(i int) bool {
	// De control-page via het ctx-blok, niet via de partitie van slot i: een
	// secundaire core van een SMP-app heeft geen eigen partitie maar wel een
	// ctx-blok, en daar staat de gedeelde page (dispatchSMP).
	cp := ctxRead(i, layout.CtxCtrlPA)
	if cp == 0 {
		return false
	}
	dev.Pull(uintptr(cp)+layout.CtrlRXDoor, 8)
	door := dev.Read64(uintptr(cp) + layout.CtrlRXDoor)
	if door&rxArmed == 0 {
		return false // geen netstack die de ring leest: RX is geen reden
	}
	headPA := ctxRead(i, layout.CtxRingHeadPA)
	return headPA != 0 && dev.Read64(uintptr(headPA)) != door&^rxArmed
}

// ctxRead leest een veld uit het ctx-blok van slot i (spiegel van ctxWrite).
func ctxRead(i int, off uintptr) uint64 {
	p := ctxPA(i) + off
	dev.Pull(p, 8)
	return dev.Read64(p)
}

// EL2Sleeps telt op hoe vaak de switchers gingen slapen, over alle slots
// (layout.CtxSleeps) — cumulatief; hopos.idlestat maakt er een tempo van.
func EL2Sleeps() uint64 {
	var n uint64
	for i := 1; i <= layout.MaxSlots; i++ {
		n += ctxRead(i, layout.CtxSleeps)
	}
	return n
}

// CoreDump is de per-core-regel van de idle-meetlat (cmd/hopos idleStat):
// per draaiende core zijn ctx-staat, EL2-slaapteller, wektijd en de
// control-page uit het ctx-blok — genoeg om te zien wélke core niet slaapt
// en of de wekker hem überhaupt als bewoner ziet.
func CoreDump() string {
	out := ""
	now := dev.Counter()
	for i := 1; i <= NumSlots(); i++ {
		core := coreOf(i)
		_, res := residents(core)
		if !coreRunning(core) {
			if st := ctxState(i); st != layout.CtxEmpty || res != 0 {
				out += fmt.Sprintf(" [core %d: parked mbox=%#x res=%d state=%d]", core, dev.Read64(layout.ParkMboxPA(core)), res, st)
			}
			continue
		}
		w := ctxRead(i, layout.CtxWake)
		flag := ""
		if w&layout.CtxWakeNoPeek != 0 {
			flag = " wait"
		}
		out += fmt.Sprintf(" [core %d: res=%d state=%d sleeps=%d wakes=%d wake=%+d%s kick=%#x unit=%d cp=%#x]", core, res,
			ctxState(i), ctxRead(i, layout.CtxSleeps), ctxRead(i, layout.CtxWakes), int64(w&^layout.CtxWakeNoPeek)-int64(now), flag,
			ctxRead(i, layout.CtxKickTarget), ctxRead(i, layout.CtxUnitSlot), ctxRead(i, layout.CtxCtrlPA))
	}
	return out
}
