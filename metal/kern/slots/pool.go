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
//
// Deze nummers zijn LOGISCH en aaneengesloten — dat is een afspraak van kern/slots
// en geen uitspraak over het silicium. Op ARM valt hij samen met de PSCI-nummering;
// op RISC-V levert een board een lijst hart-ID's die niet aaneengesloten hoeft te
// zijn, en vertaalt de kooi-naad (hartOf in cage_riscv64.go). Buiten die naad komt
// een fysiek hart-ID hier niet voor.

import (
	"errors"
	"fmt"
	"sync"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
)

// ErrPoolSize markeert een sharegroup-spec die niet bij de bestaande groep past.
// Bewust apart van de capaciteitsfouten van dit pakket, want het is een ándere
// soort fout: op capaciteit kan een aanroeper wachten (een slot komt vrij), op
// twee jobspecs die het oneens zijn nooit — dáár moet een spec veranderen. De
// aanroeper (slotmgr) mag dit dus niet als "pending" behandelen.
var ErrPoolSize = errors.New("sharegroup-poolgrootte wijkt af")

var (
	poolMu    sync.Mutex
	groupPool = map[string][]int{} // sharegroup-naam → zijn app-cores
	coreGroup = map[int]string{}   // core → sharegroup ("" = vrij of dedicated)
	coreApps  = map[int]int{}      // core → aantal levende kooien erop
	cageCore  = map[int]int{}      // kooi → toegewezen (primaire) core
	cageSpan  = map[int]int{}      // kooi → aantal cores dat hij bezet houdt
	cageGroup = map[int]string{}   // kooi → sharegroup ("" = dedicated)
)

// isAppCore: ligt c in het app-bereik [HopReserved()+1, NumAppCores()]?
func isAppCore(c int) bool { return c > HopReserved() && c <= layout.NumAppCores() }

// runFree: staan er `cores` opeenvolgende, vrije app-cores vanaf primary?
func runFree(primary, cores int, class string) bool {
	for c := primary; c < primary+cores; c++ {
		if !isAppCore(c) || !coreFree(c) || (class != "" && CoreClass(c) != class) {
			return false
		}
	}
	return true
}

// reserve boekt de hele core-run van een kooi. ALLE cores van een SMP-app
// gaan in coreApps, niet alleen de primaire: anders ziet de volgende
// plaatsing de secundaire cores als vrij en zet er stil een tweede app
// bovenop. Dat was geen theorie — gemeten 05-09 op de M4: een 1-core app die
// op de tweede core van een SMP-buur landde deed 3144 µs per system call in
// plaats van 23, en 0,8 in plaats van 81 MB/s inkomend. Geen fout, geen
// logregel, alleen een app die 137x trager is.
func reserve(cage, primary, cores int, group string) int {
	for c := primary; c < primary+cores; c++ {
		coreApps[c]++
	}
	cageCore[cage], cageSpan[cage], cageGroup[cage] = primary, cores, group
	return primary
}

// coreFree: een app-core zonder pool-claim en zonder levende kooi.
func coreFree(c int) bool { return coreGroup[c] == "" && coreApps[c] == 0 }

// freeCores verzamelt de vrije app-cores (oplopend, deterministisch).
func freeCores(class string) []int {
	var f []int
	for c := HopReserved() + 1; c <= layout.NumAppCores(); c++ {
		if coreFree(c) && (class == "" || CoreClass(c) == class) {
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

// PlaceCage kiest de fysieke core(s) voor kooi (1-based interne index) met de
// sharegroup en poolgrootte uit de jobspec, plus het eigen core-aantal van de
// app (cores: 1 = gewone app, >1 = SMP). Idempotent per kooi: een tweede
// PlaceCage voor dezelfde kooi geeft dezelfde core terug (twee-fase-start).
//
// De allocator boekt élke core die de kooi bezet houdt — bij SMP dus ook de
// secundairen — zodat een volgende plaatsing er nooit stil bovenop landt.
func PlaceCage(cage int, group string, poolCores, cores int, class string) (int, error) {
	poolMu.Lock()
	defer poolMu.Unlock()

	if c, ok := cageCore[cage]; ok {
		return c, nil // al geplaatst (twee-fase-start) — zelfde core
	}
	if poolCores < 1 {
		poolCores = 1
	}
	if cores < 1 {
		cores = 1
	}

	if group == "" {
		return placeDedicated(cage, cores, class)
	}
	if cores > 1 {
		// Een gedeelde kooi draait per definitie op één core (de pool ís het
		// deel-mechanisme); twee tegelijk zou betekenen dat de app cores van
		// de groep opeist die andere leden ook gebruiken.
		return 0, fmt.Errorf("%w: sharegroup %q en %d app-cores gaan niet samen — een kooi in een sharegroup draait op één core",
			ErrPoolSize, group, cores)
	}

	pool, ok := groupPool[group]
	if !ok {
		free := freeCores(class)
		if len(free) < poolCores {
			return 0, fmt.Errorf("sharegroup %q vraagt %d cores, %d vrij", group, poolCores, len(free))
		}
		pool = append([]int(nil), free[:poolCores]...)
		groupPool[group] = pool
		for _, c := range pool {
			coreGroup[c] = group
		}
	} else if len(pool) != poolCores {
		// De poolgrootte van een bestaande groep is NIET "first wins". Het was dat
		// stil: een groep die met twee cores begon negeerde de vier die de volgende
		// job vroeg, en die job kreeg dus de helft van zijn afgesproken hart-budget
		// zonder dat iemand het zag. De grootte is een eigenschap van de GROEP (het
		// zijn dezelfde cores), dus twee jobs die er iets anders over zeggen zijn
		// niet allebei waar — één van de twee specs is fout, en dat hoort de
		// aanroeper te horen in plaats van te raden waarom zijn app te weinig
		// hart heeft.
		return 0, fmt.Errorf("%w: sharegroup %q heeft een pool van %d core(s), maar kooi %d vraagt %d — één sharegroup, één poolgrootte",
			ErrPoolSize, group, len(pool), cage, poolCores)
	}
	// Existing pools retain their physical cores, including after adoption.
	// A joining member may constrain them, but may never silently move them.
	for _, c := range pool {
		if class != "" && CoreClass(c) != class {
			return 0, fmt.Errorf("%w: sharegroup %q has core %d of class %q, requested %q", ErrPoolSize, group, c, CoreClass(c), class)
		}
	}
	return reserve(cage, leastLoaded(pool), 1, group), nil
}

// placeDedicated kiest de core(s) van een kooi zonder sharegroup. Kooi == core
// is hier de REGEL en niet het toeval: een SMP-app draait per definitie op
// kooi..kooi+cores-1 (smp.go) en de kern weigert elke andere primaire core
// (validateSMPPlacement). Wie hier "de laagste vrije core" pakt, laat die twee
// uit elkaar lopen — dan lukt een SMP-start afhankelijk van de vólgorde waarin
// jobs geplaatst zijn (gemeten 05-09: cloudflared eerst weghalen en Spin met
// twee cores plaatsen faalde met "kooi 2 woont op core 1", andersom niet).
//
// Een gewone app houdt de terugval op de laagste vrije core: kooinummers lopen
// door boven het aantal cores, en een 1-core app hoeft niet op zijn eigen
// nummer te draaien om te werken. Hij mag alleen nooit stil op een bezette
// core belanden — zie reserve.
func placeDedicated(cage, cores int, class string) (int, error) {
	if runFree(cage, cores, class) {
		return reserve(cage, cage, cores, ""), nil
	}
	if cores > 1 {
		return 0, fmt.Errorf("SMP-kooi %d vraagt de cores %d..%d (eigen core plus de cores erna) en die zijn niet allemaal vrij",
			cage, cage, cage+cores-1)
	}
	free := freeCores(class)
	if len(free) == 0 {
		return 0, fmt.Errorf("geen vrije app-core voor kooi %d (node vol op cores; delen vraagt een sharegroup)", cage)
	}
	return reserve(cage, free[0], 1, ""), nil
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
	span := cageSpan[cage]
	if span < 1 {
		span = 1
	}
	delete(cageCore, cage)
	delete(cageSpan, cage)
	delete(cageGroup, cage)
	for x := c; x < c+span; x++ { // de hele run terug, ook de SMP-secundairen
		if coreApps[x] > 0 {
			coreApps[x]--
		}
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

// snapshotGroup copies the complete pool, including cores without a resident.
func snapshotGroup(cage int) (string, []int) {
	poolMu.Lock()
	defer poolMu.Unlock()
	group := cageGroup[cage]
	return group, append([]int(nil), groupPool[group]...)
}

// adoptCage restores a record already checked by ValidateAdoption.
func adoptCage(st SlotState) {
	poolMu.Lock()
	defer poolMu.Unlock()
	if st.ShareGroup != "" && groupPool[st.ShareGroup] == nil {
		groupPool[st.ShareGroup] = append([]int(nil), st.GroupCores...)
		for _, c := range st.GroupCores {
			coreGroup[c] = st.ShareGroup
		}
	}
	reserve(st.Slot, st.Core, st.Cores, st.ShareGroup)
}

// resetPools wist alle allocator-staat (alleen voor host-tests tussen cases).
func resetPools() {
	poolMu.Lock()
	defer poolMu.Unlock()
	groupPool = map[string][]int{}
	coreGroup = map[int]string{}
	coreApps = map[int]int{}
	cageCore = map[int]int{}
	cageSpan = map[int]int{}
	cageGroup = map[int]string{}
}

// CanPlaceDedicated is a read-only hint; PlaceCage performs the reservation.
func CanPlaceDedicated(primary, cores int) bool {
	poolMu.Lock()
	defer poolMu.Unlock()
	return cores > 0 && runFree(primary, cores, "")
}
