// Package fdt is een minimale, alloc-vrije lezer van het Flattened Device
// Tree-formaat (DTB) dat elke arm64-firmware bij boot in x0 meegeeft — de
// portable bron voor "hoeveel RAM heeft dit board" (MemTotal) en "waar staat
// de firmware-framebuffer" (Framebuffer). HopOS carveert zijn slots niet op
// compile-time-constanten maar op wat hier gevonden wordt (QEMU virt, Pi 5/4,
// straks de O6N spreken allemaal FDT).
//
// Bewust géén volledige parser: we lezen alleen /memory (reg-groottes) en
// /chosen/framebuffer (simple-framebuffer-binding, wat Linux' simplefb ook
// gebruikt). Alles big-endian (FDT-spec, Devicetree v0.4); de blob is
// onvertrouwde firmware-input, dus elke offset wordt tegen de totale grootte
// begrensd — een kromme DTB levert (…,false), geen panic.
package fdt

import (
	"math/bits"

	"hop-os/metal/dev"
)

const (
	magic      = 0xd00dfeed
	tokBegin   = 1 // FDT_BEGIN_NODE + null-getermineerde naam
	tokEnd     = 2 // FDT_END_NODE
	tokProp    = 3 // FDT_PROP + {len, nameoff} + data
	tokNop     = 4
	tokEndTree = 9

	maxBlob = 2 << 20 // een DTB > 2MB is onzin: begrenst al ons rekenwerk
)

// be32/be64 lezen een big-endian woord van een fysiek adres (device- of
// normal-memory; dev doet gealigneerde toegang).
func be32(addr uintptr) uint32 {
	return bits.ReverseBytes32(dev.Read32(addr))
}

func be64(addr uintptr) uint64 {
	return uint64(be32(addr))<<32 | uint64(be32(addr+4))
}

// MemTotal parset de DTB op base en geeft de som van alle /memory-reg-
// groottes (bytes). ok=false bij een ongeldige of onbegrepen blob, zodat de
// aanroeper op een veilige default kan terugvallen.
//
// Aanname: #address-cells = #size-cells = 2 (64-bit) — de arm64-standaard,
// bevestigd voor QEMU virt en de Raspberry Pi. Wijkt een board hiervan af,
// dan valt de detectie terug op de default (en dat is zichtbaar in de log).
func MemTotal(base uintptr) (uint64, bool) {
	if base == 0 || be32(base) != magic {
		return 0, false
	}
	structOff := be32(base + 8)
	stringsOff := be32(base + 12)
	totalSize := be32(base + 4)
	if totalSize > maxBlob || structOff >= totalSize || stringsOff >= totalSize {
		return 0, false
	}

	p := base + uintptr(structOff)
	end := base + uintptr(totalSize)

	// depth: root = 1, /memory = 2 (direct kind van de root). De root's
	// #address-cells/#size-cells bepalen de vorm van /memory's reg-tupels; we
	// lezen ze i.p.v. 2/2 aan te nemen (historische Pi-DT's wijken af). Default
	// per Devicetree-spec (address=2, size=1) als de root ze niet noemt. Alleen
	// 1 of 2 cellen (32/64-bit) ondersteund — daarbuiten veilig (0,false).
	depth := 0
	inMemNode := false
	addrCells := uint32(2)
	sizeCells := uint32(1)
	var total uint64
	found := false

	for p+4 <= end {
		tok := be32(p)
		p += 4
		switch tok {
		case tokBegin:
			depth++
			// Node-naam: null-getermineerd, gepad tot 4. "memory" of
			// "memory@<addr>" op depth 2 is het node dat we zoeken.
			name := p
			for p < end && dev.Read8(p) != 0 {
				p++
			}
			inMemNode = depth == 2 && isMemory(name, p)
			p = align4(p + 1) // voorbij de nul, dan padden
		case tokEnd:
			inMemNode = false
			depth--
		case tokProp:
			if p+8 > end {
				return 0, false
			}
			plen := be32(p)
			nameOff := be32(p + 4)
			p += 8
			data := p
			p = align4(p + uintptr(plen))
			if p > end {
				return 0, false
			}
			// nameOff is onvertrouwde firmware-input: begrens 'm binnen de blob
			// vóór er ook maar één byte gelezen wordt — anders wijst
			// base+stringsOff+nameOff ~4GB verderop → OOB-read → data-abort.
			nameAddr := uint64(stringsOff) + uint64(nameOff)
			if nameAddr >= uint64(totalSize) {
				continue // kromme nameOff: sla deze prop over
			}
			np := base + uintptr(nameAddr)
			// Root-cellen (depth 1) leggen /memory's reg-vorm vast.
			if depth == 1 && plen == 4 {
				if propIs(np, end, "#address-cells") {
					addrCells = be32(data)
				} else if propIs(np, end, "#size-cells") {
					sizeCells = be32(data)
				}
			}
			// In /memory: reg = [ (addr,size) ... ] met root's cell-counts;
			// tel de size-cellen op.
			if inMemNode && propIs(np, end, "reg") {
				if addrCells == 0 || addrCells > 2 || sizeCells == 0 || sizeCells > 2 {
					return 0, false // niet-ondersteunde cell-vorm
				}
				stride := uintptr(addrCells+sizeCells) * 4
				szOff := uintptr(addrCells) * 4
				for off := uintptr(0); off+stride <= uintptr(plen); off += stride {
					if sizeCells == 1 {
						total += uint64(be32(data + off + szOff))
					} else {
						total += be64(data + off + szOff)
					}
					found = true
				}
			}
		case tokNop:
		case tokEndTree:
			if found {
				return total, true
			}
			return 0, false
		default:
			return 0, false
		}
	}
	if found {
		return total, true
	}
	return 0, false
}

func align4(a uintptr) uintptr { return (a + 3) &^ 3 }

// isMemory meldt of de node-naam in [start,end) "memory" of "memory@..." is.
func isMemory(start, end uintptr) bool {
	const want = "memory"
	if end-start < uintptr(len(want)) {
		return false
	}
	for i := 0; i < len(want); i++ {
		if dev.Read8(start+uintptr(i)) != want[i] {
			return false
		}
	}
	// Exact "memory" of gevolgd door '@' (unit-address).
	next := start + uintptr(len(want))
	return next == end || dev.Read8(next) == '@'
}

// propIs vergelijkt een null-getermineerde string in de strings-block met s,
// begrensd tot end: een string die tot buiten de blob zou reiken is geen
// match (de end-check short-circuit vóór elke dev.Read8 → geen OOB-read).
func propIs(addr, end uintptr, s string) bool {
	for i := 0; i < len(s); i++ {
		if addr+uintptr(i) >= end || dev.Read8(addr+uintptr(i)) != s[i] {
			return false
		}
	}
	return addr+uintptr(len(s)) < end && dev.Read8(addr+uintptr(len(s))) == 0
}

// FB is de firmware-simple-framebuffer uit /chosen (simple-framebuffer-
// binding): het beeldscherm dat de bootloader al aanzette. Op de Pi 5 is
// dit hét framebuffer-pad — de EEPROM-firmware heeft geen start.elf-
// runtime meer die er via de mailbox één kan alloceren.
type FB struct {
	Base          uint64
	Size          uint64
	Width, Height uint32
	Stride        uint32
	BPP           int // 32 (a8r8g8b8/x8r8g8b8) of 16 (r5g6b5)
}

// Framebuffer zoekt /chosen/framebuffer@... en geeft zijn geometrie. Zelfde
// veiligheidsregime als MemTotal: elke offset begrensd, kromme blob levert
// (FB{}, false) — nooit een panic. reg wordt gelezen met de root-cellen
// (chosen definieert er in de praktijk geen eigen).
func Framebuffer(base uintptr) (FB, bool) {
	if base == 0 || be32(base) != magic {
		return FB{}, false
	}
	structOff := be32(base + 8)
	stringsOff := be32(base + 12)
	totalSize := be32(base + 4)
	if totalSize > maxBlob || structOff >= totalSize || stringsOff >= totalSize {
		return FB{}, false
	}

	p := base + uintptr(structOff)
	end := base + uintptr(totalSize)

	depth := 0
	inChosen := false // depth 2: "chosen"
	inFB := false     // depth 3: "framebuffer@..." onder chosen
	addrCells := uint32(2)
	sizeCells := uint32(1)
	var fb FB
	fb.BPP = 32 // default; alleen r5g6b5 maakt er 16 van

	for p+4 <= end {
		tok := be32(p)
		p += 4
		switch tok {
		case tokBegin:
			depth++
			name := p
			for p < end && dev.Read8(p) != 0 {
				p++
			}
			switch depth {
			case 2:
				inChosen = nameIs(name, p, "chosen", false)
			case 3:
				inFB = inChosen && nameIs(name, p, "framebuffer", true)
			}
			p = align4(p + 1)
		case tokEnd:
			if inFB && depth == 3 {
				// Node compleet: geldig als de kern-velden er waren.
				if fb.Base != 0 && fb.Width != 0 && fb.Height != 0 && fb.Stride != 0 {
					return fb, true
				}
				inFB = false
			}
			if depth == 2 {
				inChosen = false
			}
			depth--
		case tokProp:
			if p+8 > end {
				return FB{}, false
			}
			plen := be32(p)
			nameOff := be32(p + 4)
			p += 8
			data := p
			p = align4(p + uintptr(plen))
			if p > end {
				return FB{}, false
			}
			nameAddr := uint64(stringsOff) + uint64(nameOff)
			if nameAddr >= uint64(totalSize) {
				continue
			}
			np := base + uintptr(nameAddr)
			if depth == 1 && plen == 4 {
				if propIs(np, end, "#address-cells") {
					addrCells = be32(data)
				} else if propIs(np, end, "#size-cells") {
					sizeCells = be32(data)
				}
			}
			if !inFB {
				continue
			}
			switch {
			case propIs(np, end, "reg"):
				if addrCells == 0 || addrCells > 2 || sizeCells == 0 || sizeCells > 2 ||
					uintptr(plen) < uintptr(addrCells+sizeCells)*4 {
					continue
				}
				if addrCells == 1 {
					fb.Base = uint64(be32(data))
				} else {
					fb.Base = be64(data)
				}
				szOff := uintptr(addrCells) * 4
				if sizeCells == 1 {
					fb.Size = uint64(be32(data + szOff))
				} else {
					fb.Size = be64(data + szOff)
				}
			case propIs(np, end, "width") && plen == 4:
				fb.Width = be32(data)
			case propIs(np, end, "height") && plen == 4:
				fb.Height = be32(data)
			case propIs(np, end, "stride") && plen == 4:
				fb.Stride = be32(data)
			case propIs(np, end, "format"):
				if plen >= 6 && dev.Read8(data) == 'r' && dev.Read8(data+1) == '5' {
					fb.BPP = 16 // r5g6b5
				}
			}
		case tokNop:
		case tokEndTree:
			return FB{}, false
		default:
			return FB{}, false
		}
	}
	return FB{}, false
}

// nameIs meldt of de node-naam in [start,end) exact s is, of (met unit=true)
// s gevolgd door '@' (unit-address).
func nameIs(start, end uintptr, s string, unit bool) bool {
	if end-start < uintptr(len(s)) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if dev.Read8(start+uintptr(i)) != s[i] {
			return false
		}
	}
	next := start + uintptr(len(s))
	if next == end {
		return true
	}
	c := dev.Read8(next)
	return c == 0 || (unit && c == '@')
}
