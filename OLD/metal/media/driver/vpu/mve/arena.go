package mve

import (
	"errors"
	"sync"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// Arena is het fysieke geheugen dat de VPU mag zien: page tables, firmware,
// de communicatiegebieden en alles wat de firmware onderweg zelf opvraagt
// (referentieframes — bij 4K HEVC honderden MB's). Eén regio, door het board
// aangewezen, buiten élke RAM-declaratie van de Go-runtime, precies zoals de
// NIC- en NVMe-regio's.
//
// De allocator is een bitmap over 4KB-pagina's met first-fit. Dat is bewust
// het domste dat werkt: de aanvragen zijn groot en zeldzaam (een sessie opent,
// vraagt zijn frames, en houdt ze tot hij sluit), dus een slimmere allocator
// zou alleen maar meer te bewijzen hebben. Wat hij wél moet kunnen is
// uitlijning: de firmware vraagt om 2^n-grenzen voor zijn frame-buffers.
//
// Alle sessies delen één arena, en een sessie alloceert zowel uit Open als uit
// zijn eigen Poll (het geheugenverzoek van de firmware). Die lopen onder
// verschillende sessielocks, dus de arena heeft een eigen lock.
type Arena struct {
	mu    sync.Mutex
	base  uintptr
	pages uint32
	used  []uint64 // bitmap, 1 = in gebruik
	free  uint32   // vrije pagina's (voor Describe/telemetrie)
}

// maxLog2Align is de grootste uitlijning die ergens op slaat: de VPU ziet
// 32-bit adressen, dus een grens van 2^32 of meer bestaat voor hem niet. Het
// getal komt van de firmware, en daarboven loopt de stapgrootte van Alloc
// over (nul: een eindeloze lus).
const maxLog2Align = 31

// Foutgevallen die een aanroeper apart wil zien.
var (
	errArenaFull  = errors.New("mve: no contiguous pages left in the VPU arena")
	errArenaRange = errors.New("mve: address outside the VPU arena")
)

// NewArena neemt een fysieke regio in beheer. Base en size worden naar
// beneden/boven op de paginakorrel gebracht; een regio kleiner dan één pagina
// is een programmeerfout van het board en geeft een lege arena.
func NewArena(base uintptr, size uint64) *Arena {
	start := (uint64(base) + pageSize - 1) &^ (pageSize - 1)
	end := (uint64(base) + size) &^ (pageSize - 1)
	if end <= start {
		return &Arena{}
	}
	n := uint32((end - start) / pageSize)
	return &Arena{
		base:  uintptr(start),
		pages: n,
		used:  make([]uint64, (n+63)/64),
		free:  n,
	}
}

// Pages geeft de capaciteit en wat daarvan vrij is.
func (a *Arena) Pages() (total, free uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pages, a.free
}

// Alloc reserveert n aaneengesloten pagina's op een 2^log2align-grens en geeft
// hun fysieke basisadres. De pagina's zijn gewist: de firmware verwacht nullen
// in zijn bss, en een halve vorige sessie in een referentieframe is precies
// het soort bug dat zich als willekeurige artefacten voordoet.
func (a *Arena) Alloc(n uint32, log2align uint8) (uintptr, error) {
	if n == 0 || log2align > maxLog2Align {
		return 0, errArenaRange
	}
	step := uint64(1)
	if log2align > pageShift {
		step = 1 << (log2align - pageShift)
	}
	a.mu.Lock()
	for i := uint64(0); i+uint64(n) <= uint64(a.pages); i += step {
		if a.spanFree(uint32(i), n) {
			a.mark(uint32(i), n, true)
			a.free -= n
			a.mu.Unlock()
			// Wissen buiten de lock: de pagina's zijn al van ons, en bij een
			// referentieframe gaat het om megabytes.
			pa := a.base + uintptr(i*pageSize)
			dev.Clear(pa, uint64(n)*pageSize)
			return pa, nil
		}
	}
	a.mu.Unlock()
	return 0, errArenaFull
}

// Free geeft n pagina's vanaf pa terug. Een adres buiten de arena is een
// programmeerfout en wordt genegeerd, niet gepanikeerd: dit pad loopt ook bij
// het opruimen van een gecrashte sessie. Om dezelfde reden telt alleen wat
// werkelijk in gebruik was: een dubbele free mag de teller niet opblazen,
// anders belooft Pages() ruimte die er niet is.
func (a *Arena) Free(pa uintptr, n uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	i, ok := a.index(pa)
	if !ok || uint64(i)+uint64(n) > uint64(a.pages) {
		return
	}
	a.free += a.mark(i, n, false)
}

// index zet een fysiek adres om in een paginanummer binnen de arena.
func (a *Arena) index(pa uintptr) (uint32, bool) {
	if pa < a.base {
		return 0, false
	}
	off := uint64(pa - a.base)
	if off%pageSize != 0 || off/pageSize >= uint64(a.pages) {
		return 0, false
	}
	return uint32(off / pageSize), true
}

// spanFree meldt of [i, i+n) helemaal vrij is.
func (a *Arena) spanFree(i, n uint32) bool {
	for j := i; j < i+n; j++ {
		if a.used[j/64]&(1<<(j%64)) != 0 {
			return false
		}
	}
	return true
}

// mark zet of wist de bits van [i, i+n) en geeft hoeveel er werkelijk
// omgingen.
func (a *Arena) mark(i, n uint32, set bool) uint32 {
	var changed uint32
	for j := i; j < i+n; j++ {
		bit := uint64(1) << (j % 64)
		if (a.used[j/64]&bit != 0) != set {
			a.used[j/64] ^= bit
			changed++
		}
	}
	return changed
}
