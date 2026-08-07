package slots

// Partitie-allocator: elke slot krijgt precies de MemoryLimit die HOP voor
// die job vroeg — de een 128MB, de ander 640MB — uit één pool, i.p.v. een
// vaste gelijke slab per core. Dat is "software in de vorm van de machine":
// HOP zegt hoeveel een app alloceert, HopOS deelt exact dat uit en
// overspawnt nooit (de pool is de harde grens).
//
// De pool is het vrije DRAM van het bóárd (layout.Plan.Pool — op de Pi de
// volledige 8GB minus HOP en firmware-carveouts, geen artificiële limiet):
// fysiek RAM dat de stage-2-kooi per slot op het canonieke IPA-adres van de
// app legt (de map ontkoppelt IPA van PA, dus variabele fysieke partities —
// desnoods in meerdere losse DRAM-regio's — passen er zó in). First-fit met
// coalescing bij vrijgave houdt fragmentatie klein; 2MB-uitlijning omdat de
// stage-2-partitieblokken 2MB zijn.

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/xinix00/HopOS/metal/abi/layout"
)

// ErrNoPartition markeert "de partitie past nu niet in de pool" — een
// CAPACITEITSTOESTAND, geen defect: hij verdwijnt zodra een buurpartitie
// vrijkomt of verhuist. De adapter (kern/slotmgr) vertaalt dit naar
// hopos.ErrNoCapacity zodat HOP de taak teruggeeft aan de leader (hand-back)
// in plaats van hem in een herstartlus te jagen. Zonder dit sentinel was een
// gefragmenteerde pool een storm: elke poging downloadde de image opnieuw,
// faalde opnieuw, en na vijf keer hield de taak zijn reservering vast "voor
// de operator" — gemeten 01-08, cloudflared 124MB naast een verhuisde welcome.
var ErrNoPartition = errors.New("no partition space")

const part2M = 2 << 20

type region struct{ base, size uint64 }

var (
	partMu   sync.Mutex
	partOnce sync.Once
	partFree []region // vrije stukken, lazy uit het board-plan
	partOf   []region // per slot: de actieve reservering (size 0 = geen); lazy
	// op layout.MaxSlots+1 gedimensioneerd (het board zet MaxSlots vóór gebruik)
)

// poolInit laadt de pool van het board-plan — lazy (eerste allocatie), want
// de init-volgorde tussen dit pakket en het board-pakket is niet gegarandeerd.
func poolInit() {
	partOf = make([]region, layout.MaxSlots+1)
	smpCores = make([]int, layout.MaxSlots+1)
	hostCore = make([]int, layout.MaxSlots+1) // slot→core (share.go)
	for _, r := range layout.Pool() {
		partFree = append(partFree, region{r.Base, r.Size})
	}
	// Op ADRES sorteren — en alleen deze kopie, niet Plan.Pool: element 0 daarvan
	// bepaalt het linkadres van élk app-image (cageLinkBase), dus die orde is
	// functioneel en mag niet verschuiven.
	//
	// Waarom sorteren moet: releaseLocked voegt een vrijgegeven stuk gesorteerd in
	// en smelt het met zijn twee BUREN. Op een ongesorteerde lijst wijst "de buur"
	// naar een willekeurige regio, dus smelt hij niet — en dan heelt fragmentatie
	// nooit meer. Het LicheeRV-plan zet de hoge regio bewust vooraan, dus dit is
	// daar geen theorie.
	sort.Slice(partFree, func(a, b int) bool { return partFree[a].base < partFree[b].base })
}

func align2M(n uint64) uint64 { return (n + part2M - 1) &^ (part2M - 1) }

// partAlloc reserveert size voor slot i uit de pool en geeft basis én de
// WERKELIJKE maat terug — opgerond naar de 2MB-blokkorrel van de map. Meer
// eist geen enkele kooi meer sinds TOR (de cageGrain/cageBaseAlign-naad die
// hier zat was op beide architecturen een no-op geworden en is 04-08
// gesloopt; een toekomstige kooi met een grovere korrel brengt hem terug).
//
// Die tweede returnwaarde is er omdat het anders fout gaat, en dat is gemeten:
// de allocator rondde de maat op en bewaarde die, maar de aanroeper hield zijn
// eigen getal en gaf dát aan cagePrepare. Twee maten voor één partitie. Met een
// memory_limit van 24MB kreeg de kooi 0x1800000 te zien terwijl de partitie
// 32MB was (31-07: "maat 0x1800000 is geen macht van twee ≥ 8" — toen NAPOT nog
// een macht van twee eiste). Sinds TOR is het goedaardig geworden en dus stiller:
// de kooi zou simpelweg kleiner zijn dan de partitie. Eén bron van waarheid. Een eerdere reservering van i wordt eerst vrijgegeven
// (defensief bij een re-Start). Fout als de pool geen aaneengesloten gat van deze
// maat meer heeft.
//
// HOOG-EERST: de top van de hoogste passende regio (partFree is base-gesorteerd,
// dus achteraan beginnen). Het lage DRAM is op servers schaars en kostbaar — het
// draagt de venster-kandidaten en het onder-4GB-bereik voor toekomstige DMA —
// terwijl de bulk (Altra: ~300GB boven de 512GB-grens, via MapHigh bereikbaar)
// alleen partities draagt. Laag-eerst zou het lage blok volproppen en de bulk
// nooit raken.
func partAlloc(i int, size uint64) (base, grown uint64, err error) {
	partOnce.Do(poolInit)
	size = align2M(size)
	partMu.Lock()
	defer partMu.Unlock()
	releaseLocked(i)

	// Best-fit: de KLEINSTE regio die deze partitie nog kan dragen. Hoog-eerst
	// (wat hier stond) is goed voor de bulk-boven-de-4GB-vraag, maar het kiest
	// niet tussen regio's die allebei passen — en dan snijdt een kleine job zijn
	// partitie uit de énige regio die nog een grote kan dragen.
	//
	// GEMETEN 31-07: een 64MB-retry pakte 0x8be00000 uit de 127MB-regio terwijl
	// de losse 64MB-regio vrij lag, en daarna paste 124MB nergens meer ("does not
	// fit the pool" bij élke volgende poging). Geen lek, geen volle pool — een
	// keuze. Best-fit laat een grote regio groot zolang een kleinere volstaat.
	//
	// Binnen de gekozen regio blijft het hoog-eerst (zie hieronder): dát deel van
	// de oude vorm was wél om een reden zo.
	best := -1
	for idx := len(partFree) - 1; idx >= 0; idx-- {
		r := partFree[idx]
		if r.size < size {
			continue
		}
		// Draagt hij een bruikbare basis? Zo niet, dan is hij voor déze maat geen
		// kandidaat — anders zou best-fit een regio kiezen die straks afketst.
		if (r.base+r.size-size)&^(part2M-1) < r.base {
			continue
		}
		if best < 0 || r.size < partFree[best].size {
			best = idx
		}
	}
	if best < 0 {
		return 0, 0, fmt.Errorf("%w: partition %d MB does not fit the pool (full, fragmented, or no base the cage can describe)", ErrNoPartition, size>>20)
	}
	r := partFree[best]
	// Binnen de regio de HOOGSTE bruikbare basis. Dat is niet willekeurig: op een
	// board waar de lage regio HOP's eigen structuren draagt en de bulk erboven
	// ligt, houdt hoog-eerst het lage stuk vrij. De uitlijning is 2MB — sinds
	// TOR kan de kooi elk bereik uitdrukken; onder NAPOT was dat de maat zelf,
	// en koos de allocator anders adressen die de whitelist niet kón beschrijven
	// (gemeten 31-07: "basis 0x8bf00000 niet gealigneerd op maat 0x4000000").
	base = (r.base + r.size - size) &^ (part2M - 1)
	// Voor- en achterstuk teruggeven; het middenstuk is van dit slot.
	rest := partFree[:best:best]
	if base > r.base {
		rest = append(rest, region{r.base, base - r.base})
	}
	if end := base + size; end < r.base+r.size {
		rest = append(rest, region{end, r.base + r.size - end})
	}
	partFree = append(rest, partFree[best+1:]...)
	partOf[i] = region{base, size}
	return base, size, nil
}

// partRelease geeft de reservering van slot i terug aan de pool (coalescing).
// No-op als slot i niets gealloceerd had (al vrij). partOnce.Do óók hier:
// een Stop vóór de allereerste Start (defensieve cleanup/reconcile) bereikt
// releaseLocked anders met partOf==nil → nil-deref-panic; en releaseSlot
// schrijft ná deze aanroep smpCores[i], dat poolInit tegelijk alloceert.
func partRelease(i int) {
	partOnce.Do(poolInit)
	partMu.Lock()
	defer partMu.Unlock()
	releaseLocked(i)
}

func releaseLocked(i int) {
	if i < 0 || i > layout.MaxSlots {
		return
	}
	r := partOf[i]
	if r.size == 0 {
		return
	}
	partOf[i] = region{}

	// Gesorteerd (op base) invoegen, dan met beide buren samensmelten.
	pos := 0
	for pos < len(partFree) && partFree[pos].base < r.base {
		pos++
	}
	partFree = append(partFree, region{})
	copy(partFree[pos+1:], partFree[pos:])
	partFree[pos] = r
	if pos+1 < len(partFree) && partFree[pos].base+partFree[pos].size == partFree[pos+1].base {
		partFree[pos].size += partFree[pos+1].size
		partFree = append(partFree[:pos+1], partFree[pos+2:]...)
	}
	if pos > 0 && partFree[pos-1].base+partFree[pos-1].size == partFree[pos].base {
		partFree[pos-1].size += partFree[pos].size
		partFree = append(partFree[:pos], partFree[pos+1:]...)
	}
}

// partitionOf geeft de actieve reservering van slot i terug (base, size). ok=
// false als slot i niets gealloceerd heeft.
func partitionOf(i int) (base, size uint64, ok bool) {
	partOnce.Do(poolInit)
	partMu.Lock()
	defer partMu.Unlock()
	if i < 0 || i >= len(partOf) || partOf[i].size == 0 {
		return 0, 0, false
	}
	return partOf[i].base, partOf[i].size, true
}

// PoolBytes is de totale grootte van de partitie-pool — de plaatsings-ceiling
// die HOP krijgt. HOP overspawnt daar (per-job MemoryLimit) nooit overheen.
func PoolBytes() uint64 {
	var n uint64
	for _, r := range layout.Pool() {
		n += r.Size
	}
	return n
}

// maxLimitFor begrenst een partitie: hij moet binnen één 1GB-blok vanaf
// linkBase blijven (de stage-2-kooi mapt de partitie met één L2-tabel) én
// onder CtrlBase (waar het IPA-beeld van de app z'n control-page verwacht).
// Voor het canonieke linkBase 0x50000000 komt dat uit op 768MB (0x30000000):
// [0x40000000,0x80000000) is het GB-blok, minus de 0x10000000 tussen linkBase
// en dat blok. Dit is een bewuste, gedeelde slot-cap — geen bug.
//
// De lift wanneer de eerste app > 768MB verschijnt: het venster verruimen —
// de control-regio's (CtrlBase e.v.) omhoog schuiven zodat een groter GB-blok
// past, óf een multi-GB stage-2-map (meer dan één L2-tabel per partitie). Beide
// zijn asm-/layout-werk per board; tot dan is 768MB de harde per-slot-ceiling.
//
// linkBase is altijd het canonieke slot-1-adres: beide aanroepers pinnen 'm op
// layout.SlotBase(1) (images zijn canoniek gelinkt — de stage-2 ís de
// relocatie). De aftrekking hieronder kan dus niet underflowen; wie hier ooit
// een variabele linkBase langs wil sturen, moet dát eerst afvangen.
func maxLimitFor(linkBase uint64) uint64 {
	gbEnd := (linkBase &^ (1<<30 - 1)) + (1 << 30)
	if gbEnd > layout.CtrlBase {
		gbEnd = layout.CtrlBase
	}
	return gbEnd - linkBase
}
