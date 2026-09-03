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
// desnoods in meerdere losse DRAM-regio's — passen er zó in). Best-fit (de
// kleinste passende regio, 31-07) met coalescing bij vrijgave houdt fragmentatie
// klein; 2MB-uitlijning omdat de stage-2-partitieblokken 2MB zijn.
//
// De keten eromheen — wat HOP zelf houdt, hoe een partitie teruggegeven wordt,
// welke plafonds er zijn en welke vallen er met datum in zitten — staat in
// docs/geheugen-ontwerp.md.

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/xinix00/HopOS/metal/v2/abi/frameq"
	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/net/hopswitch"
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

const (
	defaultNetworkBuffer = 50 << 20
	controlPerSlot       = uint64(layout.SlotControlStride)
	sharedPerSlot        = controlPerSlot + 2*frameq.PageSize
	minFramePool         = 64 << 10
)

type region struct{ base, size uint64 }

var (
	partMu   sync.Mutex
	partOnce sync.Once
	partFree []region // vrije stukken, lazy uit het board-plan
	partOf   []region // per slot: de actieve reservering (size 0 = geen); lazy

	// Eén fysieke systeempot voor alle control- en netwerkbuffers. De app ziet
	// alleen zijn vaste IPA-vensters; basis en geometrie blijven HOP-privé. Het
	// bereik wordt vóór de eerste plaatsing uit partFree gesneden, zodat geen
	// app-partitie ooit dezelfde pagina's kan krijgen.
	bufferArena region
	// op layout.MaxSlots+1 gedimensioneerd (het board zet MaxSlots vóór gebruik)
)

// BufferArenaState is de minimale fysieke staat die een kern-flip meeneemt.
// Control-, queue- en pooladressen volgen uit Base; geen per-slot tabel.
type BufferArenaState struct {
	Base, Size uint64
}

// networkGeometry snijdt alleen de vaste metadata uit de voorkant. Payload
// wordt nadrukkelijk NIET over MaxSlots verdeeld: alles achter de kleine
// control- en descriptorrecords is één gezamenlijke chunkpool.
func networkGeometry(total uint64) (reserved, metadata, payload uint64, err error) {
	if layout.MaxSlots < 1 {
		return 0, 0, 0, fmt.Errorf("network buffer: geen slots")
	}
	reserved = align2M(total)
	metadata = sharedPerSlot * uint64(layout.MaxSlots)
	if reserved < metadata+minFramePool {
		need := metadata + minFramePool
		return 0, 0, 0, fmt.Errorf("network buffer %d MiB te klein voor %d descriptorqueues; minimaal %d MiB",
			total>>20, layout.MaxSlots, (need+(1<<20)-1)>>20)
	}
	return reserved, metadata, reserved - metadata, nil
}

// ConfigureNetworkBuffer reserveert de HOP-brede systeempot. Alleen control
// plus twee descriptorpagina's per mogelijk slot ligt vast; alle resterende
// bytes vormen de dynamische framepool voor de Sessions die werkelijk druk zijn.
func ConfigureNetworkBuffer(total uint64) error {
	if total == 0 {
		total = defaultNetworkBuffer
	}
	reserved, metadata, payload, err := networkGeometry(total)
	if err != nil {
		return err
	}
	partOnce.Do(poolInit)
	partMu.Lock()
	defer partMu.Unlock()
	if bufferArena.size != 0 {
		if bufferArena.size == reserved {
			return nil
		}
		return fmt.Errorf("network buffer staat al op %d MiB", bufferArena.size>>20)
	}

	best := -1
	for idx := len(partFree) - 1; idx >= 0; idx-- {
		r := partFree[idx]
		if r.size < reserved {
			continue
		}
		if best < 0 || r.size < partFree[best].size {
			best = idx
		}
	}
	if best < 0 {
		return fmt.Errorf("%w: network buffer van %d MiB past niet aaneengesloten in de pool",
			ErrNoPartition, reserved>>20)
	}
	r := partFree[best]
	base := (r.base + r.size - reserved) &^ (part2M - 1)
	if base < r.base {
		return fmt.Errorf("%w: network buffer heeft geen uitgelijnde plek", ErrNoPartition)
	}
	takeRange(base, base+reserved)
	bufferArena = region{base: base, size: reserved}
	if err := hopswitch.ConfigureFramePool(uintptr(base+metadata), payload); err != nil {
		insertFree(bufferArena)
		bufferArena = region{}
		return err
	}
	return nil
}

// ConfigureNetworkBufferSpec is de bootconfig-ingang. Kale getallen zijn
// bytes; K/M/G en KB/MB/GB/KiB/MiB/GiB zijn binaire machten. Alleen gehele
// getallen: een geheugenplan hoort exact en kopieerbaar te zijn.
func ConfigureNetworkBufferSpec(spec string) error {
	s := strings.TrimSpace(spec)
	if s == "" {
		return ConfigureNetworkBuffer(defaultNetworkBuffer)
	}
	lower := strings.ToLower(s)
	mul := uint64(1)
	for _, suf := range []struct {
		name string
		mul  uint64
	}{
		{"gib", 1 << 30}, {"gb", 1 << 30}, {"g", 1 << 30},
		{"mib", 1 << 20}, {"mb", 1 << 20}, {"m", 1 << 20},
		{"kib", 1 << 10}, {"kb", 1 << 10}, {"k", 1 << 10},
		{"b", 1},
	} {
		if strings.HasSuffix(lower, suf.name) {
			lower = strings.TrimSpace(lower[:len(lower)-len(suf.name)])
			mul = suf.mul
			break
		}
	}
	n, err := strconv.ParseUint(lower, 10, 64)
	if err != nil || n == 0 || n > ^uint64(0)/mul {
		return fmt.Errorf("network buffer %q is ongeldig (gebruik bijvoorbeeld 50M)", spec)
	}
	return ConfigureNetworkBuffer(n * mul)
}

func ensureNetworkBuffer() error {
	partOnce.Do(poolInit)
	partMu.Lock()
	configured := bufferArena.size != 0
	partMu.Unlock()
	if configured {
		return nil
	}
	return ConfigureNetworkBuffer(defaultNetworkBuffer)
}

// BufferArena geeft de flip-laag de arena die levende apps al gebruiken.
func BufferArena() (BufferArenaState, error) {
	if err := ensureNetworkBuffer(); err != nil {
		return BufferArenaState{}, err
	}
	partMu.Lock()
	defer partMu.Unlock()
	return BufferArenaState{Base: bufferArena.base, Size: bufferArena.size}, nil
}

// AdoptBufferArena neemt bij een kern-flip exact dezelfde fysieke pot over.
func AdoptBufferArena(st BufferArenaState) error {
	reserved, metadata, payload, err := networkGeometry(st.Size)
	if err != nil || st.Base == 0 || st.Base%part2M != 0 || reserved != st.Size {
		return fmt.Errorf("network buffer handoff is ongeldig: %#x+%#x", st.Base, st.Size)
	}
	partOnce.Do(poolInit)
	partMu.Lock()
	defer partMu.Unlock()
	if bufferArena.size != 0 {
		if bufferArena.base == st.Base && bufferArena.size == st.Size {
			return nil
		}
		return fmt.Errorf("network buffer was al anders gereserveerd")
	}
	if !freeSpan(st.Base, st.Base+st.Size) {
		return fmt.Errorf("network buffer %#x+%d MiB ligt niet vrij in de pool van deze kern", st.Base, st.Size>>20)
	}
	takeRange(st.Base, st.Base+st.Size)
	bufferArena = region{base: st.Base, size: st.Size}
	if err := hopswitch.ConfigureFramePool(uintptr(st.Base+metadata), payload); err != nil {
		insertFree(bufferArena)
		bufferArena = region{}
		return err
	}
	return nil
}

// slotBuffers geeft het fysieke control-blok en de twee descriptorpagina's.
// Payloadcapaciteit ontbreekt expres: die hoort bij de gezamenlijke pool.
func slotBuffers(i int) (ctrl, tx, rx uintptr, err error) {
	if i < 1 || i > layout.MaxSlots {
		return 0, 0, 0, fmt.Errorf("slot buffers: slot %d buiten bereik", i)
	}
	if err = ensureNetworkBuffer(); err != nil {
		return 0, 0, 0, err
	}
	partMu.Lock()
	defer partMu.Unlock()
	off := uint64(i-1) * sharedPerSlot
	if off+sharedPerSlot > bufferArena.size {
		return 0, 0, 0, fmt.Errorf("slot buffers: slot %d valt buiten de pot", i)
	}
	ctrl = uintptr(bufferArena.base + off)
	tx = ctrl + uintptr(controlPerSlot)
	rx = tx + frameq.PageSize
	return ctrl, tx, rx, nil
}

// poolInit laadt de pool van het board-plan — lazy (eerste allocatie), want
// de init-volgorde tussen dit pakket en het board-pakket is niet gegarandeerd.
func poolInit() {
	partOf = make([]region, layout.MaxSlots+1)
	smpCores = make([]int, layout.MaxSlots+1)
	hostCore = make([]int, layout.MaxSlots+1) // slot→core (share.go)
	for _, r := range layout.Pool() {
		partFree = append(partFree, region{r.Base, r.Size})
	}
	// Het EIGEN venster uit de pool knippen (kern-flip, docs/kern-flip.md): na
	// een flip woont deze kern in een regio die het board-plan als pool
	// declareert — die mag nooit aan een app worden uitgedeeld. Op een gewone
	// boot is het venster al een plan-hole en is dit een no-op; de bron is de
	// éigen RAM-declaratie (ownRegion), dus dit dekt élke flip-positie zonder
	// board-kennis. Host: (0,0) — tests zien exact het oude gedrag.
	if s, e := ownRegion(); e > s {
		takeRange(s, e)
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
// de kooi zou simpelweg kleiner zijn dan de partitie. Eén bron van waarheid.
//
// Een eerdere reservering van dit slot wordt hergebruikt (een re-Start moet zijn
// eigen regio terug kunnen krijgen), maar alleen als de nieuwe maat past — zie
// het blok in de functie. Fout als de pool geen aaneengesloten gat van deze maat
// meer heeft.
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

	// De vorige reservering van dit slot pas teruggeven als de nieuwe past.
	//
	// Hier stond releaseLocked(i) vooraan, "defensief bij een re-Start". Dat is
	// het juist niet: faalt de zoektocht hieronder, dan keert deze functie terug
	// met een fout terwijl de partitie van slot i al vrij in de pool ligt — en
	// dan geeft de VOLGENDE plaatsing dat geheugen aan iemand anders terwijl de
	// bewoner er nog in draait. Stille corruptie, en precies op het pad dat een
	// onplaatsbare job elke vijf seconden opnieuw raakt (Derek, 19-08).
	//
	// Teruggeven MOET wel vóór de zoektocht, want een re-Start van hetzelfde
	// slot heeft juist zijn eigen regio nodig om weer te passen. Dus: eerst een
	// momentopname, dan vrijgeven en zoeken, en bij een misser alles terugzetten
	// zoals het was. De vrije lijst is een handvol regio's — die kopie is
	// goedkoper dan één verkeerd uitgedeelde partitie.
	hadFree := append([]region(nil), partFree...)
	hadOf := region{}
	if i >= 0 && i <= layout.MaxSlots {
		hadOf = partOf[i]
	}
	restore := func() {
		partFree = hadFree
		if i >= 0 && i <= layout.MaxSlots {
			partOf[i] = hadOf
		}
	}
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
		restore()
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
	insertFree(r)
}

// takeRange haalt alles wat vrij is in [base,end) uit de vrije lijst en geeft
// terug hoeveel bytes dat waren. De tegenhanger van insertFree, en net als
// daar geldt: dít is de plek waar de dubbeluitgifte-invariant woont, dus élke
// claim op de pool loopt hierlangs — de ownRegion-trim van poolInit, de
// adoptie van een bestaande partitie (partAdopt) en de kern-flip-lening
// (BorrowKernWindow). Drie eigen split-lussen was drie kansen om het subtiel
// anders te doen; de duurste bug van de flip-changeset zat precies daar
// (31-08, twee slots op één partitie).
//
// Partiële overlap is geen fout maar het normale geval: een venster kan over
// een pool-grens heen liggen, en dan hoort alleen het pool-deel geclaimd te
// worden. Wie zeker moet weten dat het HELE bereik vrij was, vraagt dat vooraf
// met freeSpan. Aanroepen onder partMu.
func takeRange(base, end uint64) uint64 {
	if end <= base {
		return 0
	}
	var took uint64
	// Een VERSE slice, geen partFree[:0]: een claim middenin een regio splitst
	// hem in twee, dus deze lus kan méér elementen schrijven dan hij leest — en
	// in-place zou dat de eerstvolgende, nog ongelezen regio overschrijven.
	// GEMETEN 01-09: het eigen kern-venster viel middenin de grote QEMU-regio,
	// waarmee de tweede pool-regio verdween en de adoptie zijn partitie "niet
	// vrij" vond. (Het filter-idioom werkt alleen als de uitvoer nooit groeit.)
	var next []region
	for _, r := range partFree {
		rEnd := r.base + r.size
		if end <= r.base || base >= rEnd { // geen overlap
			next = append(next, r)
			continue
		}
		if base > r.base {
			next = append(next, region{r.base, base - r.base})
		}
		if end < rEnd {
			next = append(next, region{end, rEnd - end})
		}
		lo, hi := base, end
		if lo < r.base {
			lo = r.base
		}
		if hi > rEnd {
			hi = rEnd
		}
		took += hi - lo
	}
	partFree = next
	return took
}

// freeSpan meldt of [base,end) in zijn geheel binnen één vrije regio ligt —
// de vraag die vóór een claim gesteld moet worden als een deel-claim geen
// geldig antwoord is (partAdopt: een blob dat een bezette partitie beschrijft
// hoort geweigerd te worden, niet half ingewilligd). Aanroepen onder partMu.
func freeSpan(base, end uint64) bool {
	for _, r := range partFree {
		if base >= r.base && end <= r.base+r.size {
			return true
		}
	}
	return false
}

// insertFree geeft een regio terug aan de vrije lijst: gesorteerd (op base)
// invoegen, dan met beide buren samensmelten. Aanroepen onder partMu.
func insertFree(r region) {
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

// BorrowKernWindow leent een venster uit de pool voor de kern-flip
// (docs/kern-flip.md): de nieuwe kern wordt hier geplaatst en draait er tot
// een vólgende flip. Zelfde keuzeregels als partAlloc (best-fit, hoog-eerst,
// 2MB-korrel), maar niet aan een slot gebonden. Geeft basis én werkelijke
// maat terug. Hooguit één lening tegelijk: een tweede aanvraag terwijl de
// eerste loopt is een programmeerfout van de flip-laag en faalt.
func BorrowKernWindow(size uint64) (base, grown uint64, err error) {
	partOnce.Do(poolInit)
	size = align2M(size)
	partMu.Lock()
	defer partMu.Unlock()
	if kernWindow.size != 0 {
		return 0, 0, fmt.Errorf("kern-flip: er staat al een geleend venster (%#x+%d MB)", kernWindow.base, kernWindow.size>>20)
	}
	best := -1
	for idx := len(partFree) - 1; idx >= 0; idx-- {
		r := partFree[idx]
		if r.size < size {
			continue
		}
		if (r.base+r.size-size)&^(part2M-1) < r.base {
			continue
		}
		if best < 0 || r.size < partFree[best].size {
			best = idx
		}
	}
	if best < 0 {
		return 0, 0, fmt.Errorf("%w: kern-venster van %d MB past niet in de pool", ErrNoPartition, size>>20)
	}
	r := partFree[best]
	base = (r.base + r.size - size) &^ (part2M - 1)
	takeRange(base, base+size)
	kernWindow = region{base, size}
	return base, size, nil
}

// ReturnKernWindow geeft de flip-lening terug (mislukte flip — een geslaagde
// keert niet terug). No-op zonder lening.
func ReturnKernWindow() {
	partMu.Lock()
	defer partMu.Unlock()
	if kernWindow.size == 0 {
		return
	}
	insertFree(kernWindow)
	kernWindow = region{}
}

// kernWindow is de actieve flip-lening (size 0 = geen).
var kernWindow region

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
	partMu.Lock()
	reserved := bufferArena.size
	partMu.Unlock()
	if reserved >= n {
		return 0
	}
	return n - reserved
}

// PoolLargest is de grootste partitie die op dít moment nog te plaatsen is: het
// grootste vrije gat, met dezelfde eisen als partAlloc (2MB-uitlijning en een
// basis die de kooi kan beschrijven).
//
// Het bestaat omdat de SOM een verkeerd antwoord geeft. HOP laat een job toe op
// PoolBytes minus het gereserveerde, en een pool van drie regio's kan 60MB vrij
// hebben terwijl er nergens 36MB áán één stuk ligt. Dan reserveert de agent
// eerst, faalt de plaatsing, en geeft hij de task terug — elke vijf seconden
// opnieuw. GEMETEN 19-08 op de LicheeRV: de gerapporteerde capaciteit flapperde
// tussen 162 en 198MB van 222, en in dat venster weigerde de node een ándere
// job van 28MB die er wél in paste.
//
// Met dit getal kan de toelating in één keer nee zeggen, zonder te reserveren.
func PoolLargest() uint64 {
	partOnce.Do(poolInit)
	partMu.Lock()
	defer partMu.Unlock()
	var best uint64
	for _, r := range partFree {
		// Zoek de grootste maat die in deze regio past: aflopend per 2MB-korrel
		// is niet nodig — het is de regiomaat, afgerond naar beneden op de
		// korrel, mits de basis daarvoor te beschrijven is.
		size := r.size &^ (part2M - 1)
		for size > 0 && (r.base+r.size-size)&^(part2M-1) < r.base {
			size -= part2M
		}
		if size > best {
			best = size
		}
	}
	return best
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
