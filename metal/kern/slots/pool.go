package slots

// Core-pool-allocator voor sharegroups (fase 6, coöperatieve core-deling).
// HOP tagt een job met een sharegroup-naam (job-tag "sharegroup") en een
// poolgrootte in HÉLE cores (uit CPUShares); slotmgr geeft die door aan
// PlaceCage. Deze allocator wijst elke sharegroup een vaste set app-cores toe
// en balanceert de kooien eroverheen.
// Zonder sharegroup krijgt een kooi een eigen dedicated core (het gedrag van
// vóór core-deling). Kooi ≠ core: er passen dus méér kooien dan cores, tot de
// RAM-pool (partmem) of de kooi-cap (SlotCap) op is — geen kunstmatig
// core-plafond.
//
// Puur boekhouding, geen MMIO → host-testbaar. slotmgr roept PlaceCage aan bij
// fase 1 van een start (de loader) en ReleaseCage bij Stop; het gekozen
// corenummer gaat via slots.StartShared de kooi in (hostCore), zodat fase 2
// (StartStaged) en Stop dezelfde core hergebruiken.
//
// App-cores zijn [HopReserved()+1, NumAppCores()]: core 0 (+ de eerste
// HopReserved) draait de HOP-runtime, de rest draagt apps.

import (
	"fmt"
	"sync"

	"hop-os/metal/abi/layout"
)

var (
	poolMu    sync.Mutex
	groupPool = map[string][]int{} // sharegroup-naam → zijn app-cores
	coreGroup = map[int]string{}   // core → sharegroup ("" = vrij of dedicated)
	coreApps  = map[int]int{}      // core → aantal levende kooien erop
	cageCore  = map[int]int{}      // kooi → toegewezen core (voor ReleaseCage)
	cageGroup = map[int]string{}   // kooi → sharegroup ("" = dedicated)
)

// coreFree: een app-core zonder pool-claim en zonder levende kooi.
func coreFree(c int) bool { return coreGroup[c] == "" && coreApps[c] == 0 }

// freeCores verzamelt de vrije app-cores (oplopend, deterministisch).
func freeCores() []int {
	var f []int
	for c := HopReserved() + 1; c <= layout.NumAppCores(); c++ {
		if coreFree(c) {
			f = append(f, c)
		}
	}
	return f
}

// leastLoaded geeft de core uit cores met de minste levende kooien (bij gelijk
// spel de laagste index — deterministisch, en het spreidt netjes rond).
func leastLoaded(cores []int) int {
	best, bestN := cores[0], coreApps[cores[0]]
	for _, c := range cores[1:] {
		if coreApps[c] < bestN {
			best, bestN = c, coreApps[c]
		}
	}
	return best
}

// PlaceCage kiest de fysieke core voor kooi (1-based interne index) met de
// gegeven sharegroup en poolgrootte (hele cores). group=="" → een eigen vrije
// core (dedicated). Anders: de minst-belaste core van de pool, die zo nodig
// wordt aangemaakt met poolCores vrije cores. Fout als er geen (genoeg) vrije
// core is — dan is de node vol op cores (RAM is de andere muur, die HOP
// bewaakt). Idempotent per kooi: een tweede PlaceCage voor dezelfde kooi geeft
// dezelfde core terug (fase 2 hoeft niet opnieuw te kiezen).
func PlaceCage(cage int, group string, poolCores int) (int, error) {
	poolMu.Lock()
	defer poolMu.Unlock()

	if c, ok := cageCore[cage]; ok {
		return c, nil // al geplaatst (twee-fase-start) — zelfde core
	}
	if poolCores < 1 {
		poolCores = 1
	}

	if group == "" {
		free := freeCores()
		if len(free) == 0 {
			return 0, fmt.Errorf("geen vrije app-core voor kooi %d (node vol op cores)", cage)
		}
		c := free[0]
		coreApps[c]++
		cageCore[cage] = c
		cageGroup[cage] = ""
		return c, nil
	}

	cores, ok := groupPool[group]
	if !ok {
		free := freeCores()
		if len(free) < poolCores {
			return 0, fmt.Errorf("sharegroup %q vraagt %d cores, %d vrij", group, poolCores, len(free))
		}
		cores = append([]int(nil), free[:poolCores]...)
		groupPool[group] = cores
		for _, c := range cores {
			coreGroup[c] = group
		}
	}
	c := leastLoaded(cores)
	coreApps[c]++
	cageCore[cage] = c
	cageGroup[cage] = group
	return c, nil
}

// ReleaseCage geeft de core van een gestopte kooi terug. Een dedicated core
// wordt meteen vrij; een pool-core blijft van de sharegroup tot zijn láátste
// kooi weg is (dan komt de hele pool vrij). No-op voor een onbekende kooi.
func ReleaseCage(cage int) {
	poolMu.Lock()
	defer poolMu.Unlock()

	c, ok := cageCore[cage]
	if !ok {
		return
	}
	grp := cageGroup[cage]
	delete(cageCore, cage)
	delete(cageGroup, cage)
	if coreApps[c] > 0 {
		coreApps[c]--
	}
	if grp == "" {
		return // dedicated: core is nu vrij (coreApps==0, coreGroup=="")
	}
	// Pool: leeg? Dan alle cores teruggeven.
	empty := true
	for _, pc := range groupPool[grp] {
		if coreApps[pc] > 0 {
			empty = false
			break
		}
	}
	if empty {
		for _, pc := range groupPool[grp] {
			delete(coreGroup, pc)
		}
		delete(groupPool, grp)
	}
}

// resetPools wist alle allocator-staat (alleen voor host-tests tussen cases).
func resetPools() {
	poolMu.Lock()
	defer poolMu.Unlock()
	groupPool = map[string][]int{}
	coreGroup = map[int]string{}
	coreApps = map[int]int{}
	cageCore = map[int]int{}
	cageGroup = map[int]string{}
}
