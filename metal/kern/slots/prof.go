package slots

import (
	"fmt"
	"sort"
	"sync/atomic"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
)

// Sampling-profiler voor een app-slot (hopos.profile=<slot>): de wekker
// stuurt elke tick een IPI naar elke draaiende core van de eenheid; de EL2-
// fiq-handler legt de onderbroken EL1-PC in CtxLastPC (switch.s), en de tick
// erna telt die hier. Elke 2 s de top op de console, daarna leeg. De PC's
// symboliseren met `go tool addr2line <app.elf>`. Kosten voor de app: één
// EL2-rondje per ms (~1 µs). Alleen de wekker raakt prof aan.
var ProfileSlot atomic.Int32

var (
	prof       = map[uint64]uint32{}
	profKicked [64]bool
	profTicks  int
	profIdle   int
)

func profileTick(i int) {
	for c := i; c < i+coreCount(i); c++ {
		if c < len(profKicked) && profKicked[c] {
			if pc := ctxRead(c, layout.CtxLastPC); pc != 0 {
				prof[pc]++
			}
			profKicked[c] = false
		}
		if ctxState(c) != layout.CtxRunning {
			profIdle++
			continue
		}
		if phys := physCore(coreOf(c)); phys >= 0 {
			cores().Kick(phys)
			if c < len(profKicked) {
				profKicked[c] = true
			}
		}
	}
	if profTicks++; profTicks < 2000 {
		return
	}
	type kv struct {
		pc uint64
		n  uint32
	}
	var all []kv
	var total uint32
	for pc, n := range prof {
		all = append(all, kv{pc, n})
		total += n
	}
	sort.Slice(all, func(a, b int) bool { return all[a].n > all[b].n })
	out := fmt.Sprintf("prof: slot %d, %d samples, %d idle ticks:", i, total, profIdle)
	for k, e := range all {
		if k >= 12 {
			break
		}
		out += fmt.Sprintf(" %#x=%d", e.pc, e.n)
	}
	fmt.Println(out)
	prof = map[uint64]uint32{}
	profTicks, profIdle = 0, 0
}
