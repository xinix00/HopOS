// Package fdt is een minimale, alloc-vrije lezer van het Flattened Device
// Tree-formaat (DTB) dat elke arm64-firmware bij boot in x0 meegeeft — de
// portable bron voor "hoeveel RAM heeft dit board". HopOS carveert zijn
// slots niet meer op compile-time-constanten maar op wat hier gevonden wordt
// (QEMU virt, Pi 5/4, straks de O6N spreken allemaal FDT).
//
// Bewust géén volledige parser: we lezen alleen het /memory-node en tellen
// zijn `reg`-groottes op. Alles big-endian (FDT-spec, Devicetree v0.4);
// de blob is onvertrouwde firmware-input, dus elke offset wordt tegen de
// totale grootte begrensd — een kromme DTB levert (0,false), geen panic.
package fdt

import "hop-os/metal/dev"

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
	b := dev.Read32(addr)
	return b>>24 | b>>8&0xff00 | b<<8&0xff0000 | b<<24
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
	inMemNode := false
	var total uint64
	found := false

	for p+4 <= end {
		tok := be32(p)
		p += 4
		switch tok {
		case tokBegin:
			// Node-naam: null-getermineerd, gepad tot 4. "memory" of
			// "memory@<addr>" is het node dat we zoeken.
			name := p
			for p < end && dev.Read8(p) != 0 {
				p++
			}
			inMemNode = isMemory(name, p)
			p = align4(p + 1) // voorbij de nul, dan padden
		case tokEnd:
			inMemNode = false
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
			// In /memory: de "reg"-property is [ (addr64,size64) ... ];
			// tel de size-cellen op.
			if inMemNode && propIs(base+uintptr(stringsOff)+uintptr(nameOff), "reg") {
				for off := uintptr(0); off+16 <= uintptr(plen); off += 16 {
					total += be64(data + off + 8)
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

// propIs vergelijkt een null-getermineerde string in de strings-block met s.
func propIs(addr uintptr, s string) bool {
	for i := 0; i < len(s); i++ {
		if dev.Read8(addr+uintptr(i)) != s[i] {
			return false
		}
	}
	return dev.Read8(addr+uintptr(len(s))) == 0
}
