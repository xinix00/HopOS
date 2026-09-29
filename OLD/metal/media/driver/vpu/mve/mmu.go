package mve

import (
	"errors"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// De MVE heeft een EIGEN MMU, en dat is voor HopOS het belangrijkste feit van
// dit hele blok: de VPU ziet 32-bit virtuele adressen die wij vertalen, niet
// het fysieke geheugen van de node. Wie de page tables schrijft bepaalt dus
// waar de decoder mag lezen en schrijven — en dat is precies waarom deze
// driver in HOP hoort en niet in een app. Een sessie kan alleen bij zijn eigen
// firmware, zijn eigen ringen en de buffers die HOP er expliciet in hangt.
//
// De tabel is twee niveaus diep over 4KB-pagina's, met 1024 entries per
// tabelpagina:
//
//	 31        22 21        12 11         0
//	+------------+------------+------------+
//	|  L1 index  |  L2 index  | pageoffset |
//	+------------+------------+------------+
//
// Een PTE is één 32-bit woord:
//
//	 31  30 29                      2 1  0
//	+------+-------------------------+----+
//	| attr |        PA 39:12         | ap |
//	+------+-------------------------+----+
const (
	pageShift = 12
	pageSize  = 1 << pageShift
	ptesPer   = pageSize / 4 // 1024
	idxShift  = 10
	idxMask   = ptesPer - 1

	pteAttrShift = 30
	ptePAShift   = 2
	ptePAMask    = (1 << 28) - 1
)

// Page-attributen (attr) en toegangsrechten (ap) van een PTE.
const (
	attrPrivate  = 0
	attrSharedRW = 3

	accessNone = 0
	accessRO   = 1
	accessExec = 2
	accessRW   = 3
)

// De vaste adresindeling van de firmware (mve_protocol_def.h). Elke sessie
// krijgt zijn eigen tabel, dus deze adressen zijn per sessie hetzelfde: de
// firmware is gelinkt op instance 0 en verwacht zijn ringen op vaste plekken.
const (
	vaFirmwareBegin = 0x00000000 // text + bss van de firmware
	vaFirmwareEnd   = 0x00100000

	vaProtectedBeg = 0x20000000 // bitstream en werkgeheugen van de firmware

	// De grens tussen de twee bufferregio's is met host-interface v3
	// opgeschoven (mve_protocol_def.h, fw_v2 vs fw_v3). Dat is geen cosmetisch
	// verschil: de firmware kijkt aan welke kant van die grens een bufferadres
	// ligt. Een framebuffer die in de protected-regio staat pakt hij niet op,
	// en hij zegt er niets over — de uitvoerring blijft gewoon onaangeroerd.
	vaSplitV2    = 0x50000000
	vaFrameEndV2 = 0x80000000
	vaSplitV3    = 0x70000000
	vaFrameEndV3 = 0xF0000000

	vaMsgInQ   = 0x10079000 // host → firmware (berichten)
	vaMsgOutQ  = 0x1007A000 // firmware → host
	vaBufInQ   = 0x1007B000 // host → firmware (invoerbuffers)
	vaBufInRQ  = 0x1007C000 // firmware → host (invoer terug)
	vaBufOutQ  = 0x1007D000 // host → firmware (lege uitvoerbuffers)
	vaBufOutRQ = 0x1007E000 // firmware → host (gevulde uitvoer)
	vaRPC      = 0x1007F000 // firmware vraagt de host om geheugen
)

// regions is de tweedeling van de adresruimte zoals één firmwareversie hem
// ziet: bitstream-buffers horen in protected, pixelbuffers in framebuf.
type regions struct {
	protBeg, protEnd   uint32
	frameBeg, frameEnd uint32
}

// regionsFor geeft de indeling die bij een host-interface-versie hoort. De
// Linlon V8 van de O6N levert major 3.
func regionsFor(major uint8) regions {
	if major >= 3 {
		return regions{vaProtectedBeg, vaSplitV3, vaSplitV3, vaFrameEndV3}
	}
	return regions{vaProtectedBeg, vaSplitV2, vaSplitV2, vaFrameEndV2}
}

var errMMURange = errors.New("mve: virtual address outside the mapped regions")

// mmu is de page table van één sessie. l1 is het fysieke adres van de
// L1-pagina; l2 houdt de L2-pagina's bij zodat Destroy ze kan teruggeven.
type mmu struct {
	arena *Arena
	l1    uintptr
	l2    map[uint32]uintptr // L1-index → fysiek adres van de L2-pagina
	owned []span             // wat wij alloceerden en dus ook opruimen
}

// span is een stuk arena dat bij deze sessie hoort.
type span struct {
	pa    uintptr
	pages uint32
}

// newMMU maakt een lege page table.
func newMMU(a *Arena) (*mmu, error) {
	l1, err := a.Alloc(1, pageShift)
	if err != nil {
		return nil, err
	}
	return &mmu{arena: a, l1: l1, l2: map[uint32]uintptr{}, owned: []span{{l1, 1}}}, nil
}

// table geeft het fysieke adres van de L1-pagina: dat is wat in het
// MMU_CTRL-register van een LSID gaat.
func (m *mmu) table() uintptr { return m.l1 }

// alloc reserveert n pagina's in de arena, mapt ze op va en geeft het fysieke
// basisadres terug. Aaneengesloten fysiek, want dan kan de host er met één
// dev.Copy in schrijven en hoeft niemand een scatterlijst bij te houden.
func (m *mmu) alloc(va uint32, n uint32, access int) (uintptr, error) {
	pa, err := m.arena.Alloc(n, pageShift)
	if err != nil {
		return 0, err
	}
	if err := m.mapRange(va, pa, n, access); err != nil {
		m.arena.Free(pa, n)
		return 0, err
	}
	m.owned = append(m.owned, span{pa, n})
	return pa, nil
}

// mapRange hangt n fysieke pagina's vanaf pa op de virtuele adressen vanaf va.
func (m *mmu) mapRange(va uint32, pa uintptr, n uint32, access int) error {
	for i := uint32(0); i < n; i++ {
		if err := m.mapPage(va+i*pageSize, pa+uintptr(i)*pageSize, access); err != nil {
			return err
		}
	}
	return nil
}

// mapPage zet één vertaling. De L2-pagina wordt aangemaakt als hij nog niet
// bestaat; de L1-entry die ernaar wijst is read-only zoals de firmware
// verwacht (de VPU leest tabellen, schrijft ze nooit).
func (m *mmu) mapPage(va uint32, pa uintptr, access int) error {
	if uint64(pa)&(pageSize-1) != 0 {
		return errMMURange
	}
	l1i := (va >> (pageShift + idxShift)) & idxMask
	l2i := (va >> pageShift) & idxMask

	l2, ok := m.l2[l1i]
	if !ok {
		p, err := m.arena.Alloc(1, pageShift)
		if err != nil {
			return err
		}
		m.l2[l1i] = p
		m.owned = append(m.owned, span{p, 1})
		l2 = p
		dev.Write32(m.l1+uintptr(l1i*4), pte(attrPrivate, p, accessRO))
	}
	dev.Write32(l2+uintptr(l2i*4), pte(attrPrivate, pa, access))
	return nil
}

// unmapRange haalt n pagina's vanaf va uit de tabel. De pagina's zelf blijven
// van hun eigenaar: buffers van een app horen niet bij onze arena.
func (m *mmu) unmapRange(va uint32, n uint32) {
	for i := uint32(0); i < n; i++ {
		a := va + i*pageSize
		l1i := (a >> (pageShift + idxShift)) & idxMask
		l2, ok := m.l2[l1i]
		if !ok {
			continue
		}
		dev.Write32(l2+uintptr(((a>>pageShift)&idxMask)*4), 0)
	}
}

// lookup vertaalt een virtueel adres terug naar fysiek, zoals de VPU het ziet:
// het PTE-formaat draagt PA 39:12, dus veertig bits. Op ijzer is dat geen
// grens (het DRAM van een node ligt ruim daaronder), op een ontwikkelmachine
// wel — daar liggen heap-adressen hoger, en dan komt hier de afgekapte vorm
// terug. Alleen voor tests en diagnose: het normale pad kent zijn eigen
// adressen uit alloc.
func (m *mmu) lookup(va uint32) (uintptr, bool) {
	l1i := (va >> (pageShift + idxShift)) & idxMask
	l2, ok := m.l2[l1i]
	if !ok {
		return 0, false
	}
	p := dev.Read32(l2 + uintptr(((va>>pageShift)&idxMask)*4))
	if p == 0 {
		return 0, false
	}
	return uintptr(uint64(p>>ptePAShift&ptePAMask)<<pageShift) + uintptr(va&(pageSize-1)), true
}

// destroy geeft alles terug wat deze tabel in de arena hield.
func (m *mmu) destroy() {
	for _, s := range m.owned {
		m.arena.Free(s.pa, s.pages)
	}
	m.owned = nil
	m.l2 = nil
	m.l1 = 0
}

// pte bouwt één page-table-entry.
func pte(attr int, pa uintptr, access int) uint32 {
	return uint32(attr)<<pteAttrShift |
		uint32((uint64(pa)>>pageShift)&ptePAMask)<<ptePAShift |
		uint32(access)
}
