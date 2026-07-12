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
	"fmt"
	"sync"

	"hop-os/metal/layout"
)

const part2M = 2 << 20

type region struct{ base, size uint64 }

var (
	partMu   sync.Mutex
	partOnce sync.Once
	partFree []region                    // vrije stukken, lazy uit het board-plan
	partOf   [layout.MaxSlots + 1]region // per slot: de actieve reservering (size 0 = geen)
)

// poolInit laadt de pool van het board-plan — lazy (eerste allocatie), want
// de init-volgorde tussen dit pakket en het board-pakket is niet gegarandeerd.
func poolInit() {
	for _, r := range layout.Pool() {
		partFree = append(partFree, region{r.Base, r.Size})
	}
}

func align2M(n uint64) uint64 { return (n + part2M - 1) &^ (part2M - 1) }

// partAlloc reserveert (2MB-opgerond) size voor slot i uit de pool en geeft
// het fysieke basisadres. Een eerdere reservering van i wordt eerst
// vrijgegeven (defensief bij een re-Start). Fout als de pool geen
// aaneengesloten gat van deze maat meer heeft.
func partAlloc(i int, size uint64) (uint64, error) {
	partOnce.Do(poolInit)
	size = align2M(size)
	partMu.Lock()
	defer partMu.Unlock()
	releaseLocked(i)
	for idx, r := range partFree {
		if r.size < size {
			continue
		}
		base := r.base
		if r.size == size {
			partFree = append(partFree[:idx], partFree[idx+1:]...)
		} else {
			partFree[idx] = region{r.base + size, r.size - size}
		}
		partOf[i] = region{base, size}
		return base, nil
	}
	return 0, fmt.Errorf("partitie %d MB past niet in de pool (vol of gefragmenteerd)", size>>20)
}

// partRelease geeft de reservering van slot i terug aan de pool (coalescing).
// No-op als slot i niets gealloceerd had (al vrij).
func partRelease(i int) {
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
func maxLimitFor(linkBase uint64) uint64 {
	gbEnd := (linkBase &^ (1<<30 - 1)) + (1 << 30)
	if gbEnd > layout.CtrlBase {
		gbEnd = layout.CtrlBase
	}
	return gbEnd - linkBase
}
