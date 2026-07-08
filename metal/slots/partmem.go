package slots

// Partitie-allocator: elke slot krijgt precies de MemoryLimit die HOP voor
// die job vroeg — de een 128MB, de ander 640MB — uit één pool, i.p.v. een
// vaste gelijke slab per core. Dat is "software in de vorm van de machine":
// HOP zegt hoeveel een app alloceert, HopOS deelt exact dat uit en
// overspawnt nooit (de pool is de harde grens).
//
// De pool is het slot-venster [SlotsBase, CtrlBase) — fysiek RAM dat de
// stage-2-kooi per slot op het canonieke IPA-adres van de app legt (de
// self-relocating map ontkoppelt IPA van PA, dus variabele fysieke partities
// passen er zó in). First-fit met coalescing bij vrijgave houdt fragmentatie
// klein; 2MB-uitlijning omdat de stage-2-partitieblokken 2MB zijn.

import (
	"fmt"
	"sync"

	"hop-os/metal/layout"
)

const part2M = 2 << 20

type region struct{ base, size uint64 }

var (
	partMu   sync.Mutex
	partFree = []region{{layout.SlotsBase, layout.CtrlBase - layout.SlotsBase}}
	partOf   [layout.MaxSlots + 1]region // per slot: de actieve reservering (size 0 = geen)
)

func align2M(n uint64) uint64 { return (n + part2M - 1) &^ (part2M - 1) }

// partAlloc reserveert (2MB-opgerond) size voor slot i uit de pool en geeft
// het fysieke basisadres. Een eerdere reservering van i wordt eerst
// vrijgegeven (defensief bij een re-Start). Fout als de pool geen
// aaneengesloten gat van deze maat meer heeft.
func partAlloc(i int, size uint64) (uint64, error) {
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
func PoolBytes() uint64 { return layout.CtrlBase - layout.SlotsBase }

// maxLimitFor begrenst een partitie: hij moet binnen één 1GB-blok vanaf
// linkBase blijven (de stage-2-kooi mapt de partitie met één L2-tabel) én
// onder CtrlBase (waar het IPA-beeld van de app z'n control-page verwacht).
// Een grotere app vergt een multi-GB stage-2 (later) of een groter venster
// (control-regio's omhoog = asm-werk, per board).
func maxLimitFor(linkBase uint64) uint64 {
	gbEnd := (linkBase &^ (1<<30 - 1)) + (1 << 30)
	if gbEnd > layout.CtrlBase {
		gbEnd = layout.CtrlBase
	}
	return gbEnd - linkBase
}
