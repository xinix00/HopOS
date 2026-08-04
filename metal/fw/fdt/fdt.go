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

	"github.com/xinix00/HopOS/metal/dev"
)

const (
	magic      = 0xd00dfeed
	tokBegin   = 1 // FDT_BEGIN_NODE + null-getermineerde naam
	tokEnd     = 2 // FDT_END_NODE
	tokProp    = 3 // FDT_PROP + {len, nameoff} + data
	tokNop     = 4
	tokEndTree = 9

	maxBlob = 2 << 20 // een DTB > 2MB is onzin: begrenst al ons rekenwerk
	hdrLen  = 40      // vaste headergrootte (Devicetree v0.4, tot en met size_dt_struct)
)

// hdr is de GEVALIDEERDE header: de enige plek waar deze firmware-input gewogen
// wordt, zodat elke walker eronder alleen nog binnen gedeclareerde blokken
// loopt. Dat was hier vier keer los uitgeschreven — en elke kopie toetste
// alleen dat de offsets binnen totalSize vielen, waarna er tóch op totalSize
// werd afgelopen. Eén helper, dus één definitie van "binnen de blob".
type hdr struct {
	base       uintptr
	total      uint64  // gedeclareerde blobgrootte
	structs    uintptr // structure-block: [structs, structEnd)
	structEnd  uintptr
	strings    uintptr // strings-block (property-namen): [strings, stringsEnd)
	stringsEnd uintptr
	rsv        uintptr // /memreserve/-array: [rsv, rsvEnd)
	rsvEnd     uintptr
}

// header leest en weegt de DTB-header op base. Getoetst wordt: de pointer, het
// magic, een blobgrootte tussen de header en maxBlob, en dat de drie
// blok-offsets binnen die grootte vallen. Een header die deze test overleeft
// draagt geen enkele offset meer die buiten de blob wijst.
//
// De blokGROOTTES (size_dt_struct/size_dt_strings, header-offsets 0x24/0x20)
// worden gebruikt wanneer de firmware ze plausibel vult — dan lopen de walkers
// tot het einde van hún blok in plaats van tot het einde van de blob. Vult hij
// ze niet (0, of buiten de blob), dan valt de grens terug op de blobgrootte:
// dat is precies wat er vóór deze helper stond, dus nooit slechter dan
// voorheen, en de fijnere grens is winst waar hij te halen valt.
func header(base uintptr) (hdr, bool) {
	if base == 0 || be32(base) != magic {
		return hdr{}, false
	}
	h := hdr{base: base, total: uint64(be32(base + 4))}
	if h.total < hdrLen || h.total > maxBlob {
		return hdr{}, false
	}
	structOff := uint64(be32(base + 8))
	stringsOff := uint64(be32(base + 12))
	rsvOff := uint64(be32(base + 16))
	if structOff >= h.total || stringsOff >= h.total || rsvOff >= h.total {
		return hdr{}, false
	}
	blockEnd := func(off, size uint64) uintptr {
		if size > 0 && off+size <= h.total { // overflow kan niet: beide uint32
			return base + uintptr(off+size)
		}
		return base + uintptr(h.total)
	}
	h.structs = base + uintptr(structOff)
	h.structEnd = blockEnd(structOff, uint64(be32(base+36))) // size_dt_struct
	h.strings = base + uintptr(stringsOff)
	h.stringsEnd = blockEnd(stringsOff, uint64(be32(base+32))) // size_dt_strings
	h.rsv = base + uintptr(rsvOff)
	h.rsvEnd = base + uintptr(h.total)
	return h, true
}

// propName geeft het adres van een property-naam in het strings-block, of
// ok=false als de nameoff-cel buiten dat blok wijst (kromme blob).
func (h hdr) propName(nameOff uint32) (uintptr, bool) {
	p := h.strings + uintptr(nameOff)
	if p < h.strings || p >= h.stringsEnd {
		return 0, false
	}
	return p, true
}

// Valid meldt of op base een geldige DTB-blob staat: een niet-nul-pointer met
// het FDT-magic (0xd00dfeed, big-endian) op offset 0 en een header die zichzelf
// kan dragen. Dé onderscheidende test voor "kreeg dit board een device-tree
// mee?" — op UEFI/ACPI-firmware (de O6N) is er geen DTB en wijst de scratch-
// pointer naar rommel, waar elke reader terecht (…,false) op teruggeeft; met
// Valid kan de aanroeper dat expliciet vaststellen en er LUID op reageren
// (waarschuwen + veilige terugval) i.p.v. stil te degraderen.
//
// Bewust dezelfde toets als de readers doen: een blob die Valid overleeft maar
// waar MemTotal dan alsnog op afketst (of erger: 8 gedeclareerde bytes waar de
// readers +8/+12/+16 uit lezen) is een val voor de aanroeper.
func Valid(base uintptr) bool {
	_, ok := header(base)
	return ok
}

// be32/be64 lezen een big-endian woord van een fysiek adres (device- of
// normal-memory; dev doet gealigneerde toegang).
func be32(addr uintptr) uint32 {
	return bits.ReverseBytes32(dev.Read32(addr))
}

func be64(addr uintptr) uint64 {
	return uint64(be32(addr))<<32 | uint64(be32(addr+4))
}

// Region is een fysieke geheugenrange (bytes). Losgekoppeld van layout.Region
// zodat fdt een generieke DTB-lezer blijft; de aanroeper converteert.
type Region struct{ Addr, Size uint64 }

// MemTotal geeft de som van alle /memory-banken (bytes). ok=false bij een
// ongeldige blob, zodat de aanroeper op een veilige default kan terugvallen.
func MemTotal(base uintptr) (uint64, bool) {
	regs, ok := MemRegions(base)
	if !ok {
		return 0, false
	}
	var total uint64
	for _, r := range regs {
		total += r.Size
	}
	return total, true
}

// MemRegions parset de DTB op base en geeft álle /memory-banken (elk een
// (adres,grootte)-tupel). ok=false bij een ongeldige of onbegrepen blob.
//
// Cell-vorm uit de root's #address-cells/#size-cells (default 2/1 per spec);
// alleen 1 of 2 cellen (32/64-bit) ondersteund. Onvertrouwde firmware-input:
// elke offset wordt begrensd — een kromme DTB levert (nil,false), geen panic.
func MemRegions(base uintptr) ([]Region, bool) {
	h, ok := header(base)
	if !ok {
		return nil, false
	}

	p := h.structs
	end := h.structEnd

	depth := 0
	inMemNode := false
	addrCells := uint32(2)
	sizeCells := uint32(1)
	var regs []Region

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
			inMemNode = depth == 2 && nameIs(name, p, "memory", true)
			p = align4(p + 1)
		case tokEnd:
			inMemNode = false
			depth--
		case tokProp:
			if p+8 > end {
				return nil, false
			}
			plen := be32(p)
			nameOff := be32(p + 4)
			p += 8
			data := p
			p = align4(p + uintptr(plen))
			if p > end {
				return nil, false
			}
			np, ok := h.propName(nameOff)
			if !ok {
				continue
			}
			if depth == 1 && plen == 4 {
				if propIs(np, h.stringsEnd, "#address-cells") {
					addrCells = be32(data)
				} else if propIs(np, h.stringsEnd, "#size-cells") {
					sizeCells = be32(data)
				}
			}
			// In /memory: reg = [ (addr,size) ... ] met root's cell-counts.
			if inMemNode && propIs(np, h.stringsEnd, "reg") {
				if addrCells == 0 || addrCells > 2 || sizeCells == 0 || sizeCells > 2 {
					return nil, false
				}
				stride := uintptr(addrCells+sizeCells) * 4
				szOff := uintptr(addrCells) * 4
				for off := uintptr(0); off+stride <= uintptr(plen); off += stride {
					var a, s uint64
					if addrCells == 1 {
						a = uint64(be32(data + off))
					} else {
						a = be64(data + off)
					}
					if sizeCells == 1 {
						s = uint64(be32(data + off + szOff))
					} else {
						s = be64(data + off + szOff)
					}
					regs = append(regs, Region{Addr: a, Size: s})
				}
			}
		case tokNop:
		case tokEndTree:
			return regs, len(regs) > 0
		default:
			return nil, false
		}
	}
	return regs, len(regs) > 0
}

// Bootargs geeft /chosen/bootargs — de boot-parameterregel (op de Pi:
// cmdline.txt, door de firmware in de DTB gezet). Hét kanaal voor
// node-configuratie zonder rebuild (Derek, 2026-07-11): hopos.*-sleutels
// erin, de rest (Linux-restanten) wordt genegeerd door de parser.
func Bootargs(base uintptr) (string, bool) {
	return nodeString(base, "chosen", "bootargs")
}

// InitrdRegion geeft [start, end) van het door de firmware geladen initramfs
// (/chosen/linux,initrd-start en -end; 4- of 8-byte big-endian cellen). HopOS
// gebruikt dat mechanisme bewust als config-kanaal: `initramfs hopos.cfg
// <addr>` in config.txt laadt een tekstbestand van élke maat — het 1024-byte
// bootargs-plafond (gemeten 19-07: elke cmdline verloor zijn staart) geldt
// daar niet. ok=false zonder initramfs of bij een kromme regio.
func InitrdRegion(base uintptr) (start, end uintptr, ok bool) {
	cell := func(name string) (uint64, bool) {
		b, ok := nodeBytes(base, "chosen", name)
		if !ok || (len(b) != 4 && len(b) != 8) {
			return 0, false
		}
		var v uint64
		for _, c := range b {
			v = v<<8 | uint64(c)
		}
		return v, true
	}
	s, ok1 := cell("linux,initrd-start")
	e, ok2 := cell("linux,initrd-end")
	if !ok1 || !ok2 || e <= s || e-s > maxBlob {
		return 0, 0, false
	}
	return uintptr(s), uintptr(e), true
}

// RootString leest een string-property van de root-node (bv. "serial-number",
// door de Pi-firmware gezet) — de stabiele bron voor een board-identiteit
// zoals de MAC. ok=false bij een kromme blob of ontbrekende property.
func RootString(base uintptr, name string) (string, bool) {
	return nodeString(base, "", name)
}

// nodeString leest een string-property: node="" = de root zelf, anders het
// directe kind met die naam (bv. "chosen"). ok=false bij een kromme blob of
// ontbrekende property.
func nodeString(base uintptr, node, name string) (string, bool) {
	b, ok := nodeBytes(base, node, name)
	if !ok {
		return "", false
	}
	// Null-getermineerde string; de terminator niet meenemen.
	for i, c := range b {
		if c == 0 {
			return string(b[:i]), true
		}
	}
	return string(b), true
}

// nodeBytes leest de ruwe bytes van een property (voor cellen zoals
// linux,initrd-start; nodeString knipt op de NUL).
func nodeBytes(base uintptr, node, name string) ([]byte, bool) {
	h, ok := header(base)
	if !ok {
		return nil, false
	}

	p := h.structs
	end := h.structEnd
	depth := 0
	inNode := node == "" // root-props zitten op depth 1

	for p+4 <= end {
		tok := be32(p)
		p += 4
		switch tok {
		case tokBegin:
			depth++
			nameStart := p
			for p < end && dev.Read8(p) != 0 {
				p++
			}
			if node != "" {
				inNode = depth == 2 && nameIs(nameStart, p, node, false)
			}
			p = align4(p + 1)
		case tokEnd:
			if node != "" && depth == 2 {
				inNode = false
			}
			depth--
		case tokProp:
			if p+8 > end {
				return nil, false
			}
			plen := be32(p)
			nameOff := be32(p + 4)
			p += 8
			data := p
			p = align4(p + uintptr(plen))
			if p > end {
				return nil, false
			}
			wantDepth := 1
			if node != "" {
				wantDepth = 2
			}
			np, nok := h.propName(nameOff)
			if inNode && depth == wantDepth && nok && propIs(np, h.stringsEnd, name) {
				b := make([]byte, 0, plen)
				for i := uintptr(0); i < uintptr(plen); i++ {
					b = append(b, dev.Read8(data+i))
				}
				return b, true
			}
		case tokNop:
		case tokEndTree:
			return nil, false
		default:
			return nil, false
		}
	}
	return nil, false
}

// MemReserve leest het /memreserve/-blok uit de DTB-header (off_mem_rsvmap,
// base+16): een array {u64 adres, u64 grootte} afgesloten door {0,0}. Dit zijn
// regio's die de firmware/TF-A voor zichzelf houdt — nooit uitdelen als pool.
// Leeg (of nil bij een kromme blob) is geldig: veel boards reserveren niets.
func MemReserve(base uintptr) []Region {
	h, ok := header(base)
	if !ok {
		return nil
	}
	var regs []Region
	p, end := h.rsv, h.rsvEnd
	for p+16 <= end {
		a := be64(p)
		s := be64(p + 8)
		p += 16
		if a == 0 && s == 0 {
			break // terminator
		}
		regs = append(regs, Region{Addr: a, Size: s})
		if len(regs) > 64 { // sanity: een gezonde DTB heeft er een handvol
			break
		}
	}
	return regs
}

// BlobSize geeft de totale DTB-grootte (bytes) uit de header; 0 bij een
// ongeldige blob. Voor de aanroeper die de DTB-regio zelf uit de pool wil
// snijden (de firmware legde 'm ergens in RAM neer).
func BlobSize(base uintptr) uint64 {
	h, ok := header(base)
	if !ok {
		return 0
	}
	return h.total
}

func align4(a uintptr) uintptr { return (a + 3) &^ 3 }

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
	Width, Height uint32
	Stride        uint32
	BPP           int // 32 (a8r8g8b8/x8r8g8b8) of 16 (r5g6b5)
}

// Framebuffer zoekt /chosen/framebuffer@... en geeft zijn geometrie. Zelfde
// veiligheidsregime als MemTotal: elke offset begrensd, kromme blob levert
// (FB{}, false) — nooit een panic. reg wordt gelezen met de root-cellen
// (chosen definieert er in de praktijk geen eigen).
func Framebuffer(base uintptr) (FB, bool) {
	h, ok := header(base)
	if !ok {
		return FB{}, false
	}

	p := h.structs
	end := h.structEnd

	depth := 0
	inChosen := false // depth 2: "chosen"
	inFB := false     // depth 3: "framebuffer@..." onder chosen
	addrCells := uint32(2)
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
			np, nok := h.propName(nameOff)
			if !nok {
				continue
			}
			sEnd := h.stringsEnd
			// De cellen van het DICHTSTBIJZIJNDE niveau winnen: de Pi-firmware
			// schrijft /chosen mét een eigen #address-cells=1 en de framebuffer-
			// reg als <u32 base><u32 size> — wie hier alleen de root-cellen (2)
			// leest, plakt base en size aan elkaar tot een adres in de
			// stratosfeer, en de eerste pixel-veeg is dan een bus-fault=reset.
			// GEMETEN 04-08 (FBDBG, Pi 5): base=0x3f800000003f4800 — dít was de
			// "32-bpp-freeze" van 19-07, drie weken vermomd als silicium.
			if plen == 4 && (depth == 1 || (depth == 2 && inChosen)) &&
				propIs(np, sEnd, "#address-cells") {
				addrCells = be32(data)
			}
			if !inFB {
				continue
			}
			switch {
			case propIs(np, sEnd, "reg"):
				if addrCells == 0 || addrCells > 2 || uintptr(plen) < uintptr(addrCells)*4 {
					continue
				}
				if addrCells == 1 {
					fb.Base = uint64(be32(data))
				} else {
					fb.Base = be64(data)
				}
			case propIs(np, sEnd, "width") && plen == 4:
				fb.Width = be32(data)
			case propIs(np, sEnd, "height") && plen == 4:
				fb.Height = be32(data)
			case propIs(np, sEnd, "stride") && plen == 4:
				fb.Stride = be32(data)
			case propIs(np, sEnd, "format"):
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
