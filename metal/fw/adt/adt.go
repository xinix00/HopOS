// Package adt is een minimale, alloc-vrije lezer van Apple's Device Tree — de
// boom die iBoot op elke Apple-SoC achterlaat, en de enige bron voor wat dit
// silicium écht bevat: welke cores er zijn en hoe je ze start, waar de
// dockchannel zit, waar de opslag-coprocessor woont, welk MAC de NIC draagt.
//
// Waarom naast fw/fdt en niet erin: het is een ánder formaat. FDT is
// big-endian met een aparte stringtabel en tokens; de ADT is little-endian,
// draagt de naam ván een node als property "name", en heeft geen tokens — een
// node is een telling van properties en kinderen, en dan die properties en
// kinderen achter elkaar. Twee formaten in één parser persen zou van beide een
// slechtere lezer maken.
//
// Bewust géén volledige boom in geheugen: de ADT is 448KB en HopOS leest er een
// handvol waarden uit. Elke functie loopt de boom opnieuw af vanaf de wortel;
// dat kost microseconden en scheelt een allocator die er op dit punt van de
// boot nog niet is. Alle offsets worden tegen de gedeclareerde grootte
// begrensd: een kromme ADT levert (…, false), geen panic.
//
// Formaat (m1n1 proxyclient/m1n1/adt.py, de leidende referentie):
//
//	node:     property_count u32, child_count u32, properties…, children…
//	property: name [32]byte, size u32 (bit 31 = vlag, niet de lengte),
//	          value [size]byte, uitgevuld tot een viervoud
package adt

import "github.com/xinix00/HopOS/metal/dev"

const (
	propNameLen = 32
	propHdrLen  = propNameLen + 4 // naam + grootte
	nodeHdrLen  = 8               // property_count + child_count
	sizeMask    = 0x7fffffff      // bit 31 is een vlag van de firmware, geen lengte

	// maxTree begrenst al ons rekenwerk. De ADT van een M4 is ~448KB; 8MB is
	// ruim en houdt een kapotte grootte uit de lussen hieronder.
	maxTree = 8 << 20
)

// Tree is een gevalideerde ADT: de enige plek waar deze firmware-input gewogen
// wordt, zodat de walkers eronder alleen binnen de boom lopen.
type Tree struct {
	base uintptr
	end  uintptr
}

// Node is een offset in de boom (vanaf base). De wortel is 0.
type Node uint32

// Open weegt de boom. false = geen bruikbare ADT (nul-adres, onzinnige grootte,
// of een wortel die niet eens zijn eigen header draagt).
func Open(base uintptr, size uint32) (Tree, bool) {
	if base == 0 || size < nodeHdrLen || size > maxTree {
		return Tree{}, false
	}
	t := Tree{base: base, end: base + uintptr(size)}
	// Sanity: de wortel moet properties hebben en binnen de boom passen.
	np, nc, ok := t.counts(0)
	if !ok || np == 0 || np > 1024 || nc > 1024 {
		return Tree{}, false
	}
	return t, true
}

// counts leest de node-header op offset off.
func (t Tree) counts(off Node) (props, children uint32, ok bool) {
	p := t.base + uintptr(off)
	if p+nodeHdrLen > t.end {
		return 0, 0, false
	}
	return dev.Read32(p), dev.Read32(p + 4), true
}

// propAt geeft de gegevens van de property op offset p, plus de offset van de
// volgende. ok=false bij een property die buiten de boom valt.
func (t Tree) propAt(p uintptr) (name [propNameLen]byte, val uintptr, size uint32, next uintptr, ok bool) {
	if p+propHdrLen > t.end {
		return name, 0, 0, 0, false
	}
	for i := uintptr(0); i < propNameLen; i++ {
		name[i] = dev.Read8(p + i)
	}
	size = dev.Read32(p+propNameLen) & sizeMask
	val = p + propHdrLen
	if size > maxTree || val+uintptr(size) > t.end {
		return name, 0, 0, 0, false
	}
	next = (val + uintptr(size) + 3) &^ 3
	return name, val, size, next, true
}

// afterProps geeft de offset waar de kinderen van deze node beginnen.
func (t Tree) afterProps(off Node) (uintptr, bool) {
	np, _, ok := t.counts(off)
	if !ok {
		return 0, false
	}
	p := t.base + uintptr(off) + nodeHdrLen
	for i := uint32(0); i < np; i++ {
		if _, _, _, next, ok := t.propAt(p); ok {
			p = next
		} else {
			return 0, false
		}
	}
	return p, true
}

// end geeft de offset net voorbij deze node (dus die van zijn volgende broer).
func (t Tree) nodeEnd(off Node) (uintptr, bool) {
	p, ok := t.afterProps(off)
	if !ok {
		return 0, false
	}
	_, nc, _ := t.counts(off)
	for i := uint32(0); i < nc; i++ {
		if p >= t.end {
			return 0, false
		}
		e, ok := t.nodeEnd(Node(p - t.base))
		if !ok {
			return 0, false
		}
		p = e
	}
	return p, true
}

// Prop geeft adres en lengte van de waarde van property `name` op deze node.
// De waarde blijft in het geheugen van de firmware staan; de aanroeper leest
// hem met dev.Read* (het is device-geheugen, dus niet zomaar een []byte).
func (t Tree) Prop(off Node, name string) (addr uintptr, size uint32, ok bool) {
	np, _, ok := t.counts(off)
	if !ok {
		return 0, 0, false
	}
	p := t.base + uintptr(off) + nodeHdrLen
	for i := uint32(0); i < np; i++ {
		n, val, sz, next, ok := t.propAt(p)
		if !ok {
			return 0, 0, false
		}
		if matches(n, name) {
			return val, sz, true
		}
		p = next
	}
	return 0, 0, false
}

// matches vergelijkt een 32-byte propertynaam (met nullen uitgevuld) met een
// Go-string. Apple gebruikt in ADT-namen zowel '-' als '_'; de firmware is daar
// niet consequent in, dus behandelen we ze als gelijk — net als m1n1's
// Python-kant doet.
func matches(n [propNameLen]byte, s string) bool {
	if len(s) > propNameLen {
		return false
	}
	for i := 0; i < propNameLen; i++ {
		c := byte(0)
		if i < len(s) {
			c = s[i]
		}
		g := n[i]
		if g == '_' {
			g = '-'
		}
		if c == '_' {
			c = '-'
		}
		if g != c {
			return false
		}
	}
	return true
}

// Name geeft de naam van een node: de property "name", zonder de afsluitende nul.
func (t Tree) Name(off Node) (string, bool) {
	addr, size, ok := t.Prop(off, "name")
	if !ok || size == 0 || size > 64 {
		return "", false
	}
	b := make([]byte, 0, size)
	for i := uint32(0); i < size; i++ {
		c := dev.Read8(addr + uintptr(i))
		if c == 0 {
			break
		}
		b = append(b, c)
	}
	return string(b), true
}

// Child zoekt een direct kind op naam.
func (t Tree) Child(off Node, name string) (Node, bool) {
	p, ok := t.afterProps(off)
	if !ok {
		return 0, false
	}
	_, nc, _ := t.counts(off)
	for i := uint32(0); i < nc; i++ {
		if p >= t.end {
			return 0, false
		}
		c := Node(p - t.base)
		if n, ok := t.Name(c); ok && n == name {
			return c, true
		}
		e, ok := t.nodeEnd(c)
		if !ok {
			return 0, false
		}
		p = e
	}
	return 0, false
}

// Children roept fn aan voor elk direct kind, tot fn false teruggeeft.
func (t Tree) Children(off Node, fn func(Node) bool) {
	p, ok := t.afterProps(off)
	if !ok {
		return
	}
	_, nc, _ := t.counts(off)
	for i := uint32(0); i < nc; i++ {
		if p >= t.end {
			return
		}
		c := Node(p - t.base)
		if !fn(c) {
			return
		}
		e, ok := t.nodeEnd(c)
		if !ok {
			return
		}
		p = e
	}
}

// Path zoekt een node via zijn pad ("/arm-io/uart0"). De wortel is "/".
func (t Tree) Path(path string) (Node, bool) {
	n := Node(0)
	for i := 0; i < len(path); {
		if path[i] == '/' {
			i++
			continue
		}
		j := i
		for j < len(path) && path[j] != '/' {
			j++
		}
		c, ok := t.Child(n, path[i:j])
		if !ok {
			return 0, false
		}
		n, i = c, j
	}
	return n, true
}

// U32/U64 lezen een getal uit een property. Ontbreekt hij of is hij te kort,
// dan komt def terug — de aanroeper hoeft geen twee-waarden-dans te doen voor
// waarden waar een verstandige terugval voor bestaat.
func (t Tree) U32(off Node, name string, def uint32) uint32 {
	addr, size, ok := t.Prop(off, name)
	if !ok || size < 4 {
		return def
	}
	return dev.Read32(addr)
}

// U64 leest ALTIJD in twee helften van 32 bits. Een property-waarde begint 36
// bytes na het begin van de property (naam 32 + grootte 4) en is dus op een
// viervoud uitgelijnd, niet op een achtvoud — en de boom staat in
// device-geheugen, waar een 64-bit lees op een scheef adres een
// alignment-fault geeft. Gekost: één boot met een EL1-exception in dev.Read64
// (29-08).
func (t Tree) U64(off Node, name string, def uint64) uint64 {
	addr, size, ok := t.Prop(off, name)
	if !ok || size < 8 {
		return def
	}
	return uint64(dev.Read32(addr)) | uint64(dev.Read32(addr+4))<<32
}

// ── Adressen ────────────────────────────────────────────────────────────────
//
// Een reg-waarde in de ADT is RELATIEF aan de bus waar de node aan hangt, en de
// breedte van zijn velden komt uit de #address-cells/#size-cells van de OUDER.
// Om er een fysiek adres van te maken loop je omhoog: elke node met een
// ranges-property vertaalt een venster van zijn eigen busadressen naar die van
// zijn ouder. Wie dat overslaat krijgt een adres dat er plausibel uitziet en
// nergens heen wijst — de ans-node heet niet voor niets ans@81600000 terwijl
// hij op 0x481600000 woont.
//
// Dit is m1n1's adt_get_reg(adt, path_offsets, ...), en daarom werkt het op een
// PAD en niet op een losse node: de vertaling heeft de hele keten ouders nodig.

// Path met de keten erbij: chain[0] is de wortel, chain[len-1] de node zelf.
func (t Tree) PathTrace(path string) (chain []Node, ok bool) {
	chain = append(chain, 0)
	n := Node(0)
	for i := 0; i < len(path); {
		if path[i] == '/' {
			i++
			continue
		}
		j := i
		for j < len(path) && path[j] != '/' {
			j++
		}
		c, ok := t.Child(n, path[i:j])
		if !ok {
			return nil, false
		}
		chain = append(chain, c)
		n, i = c, j
	}
	return chain, true
}

// cells geeft de #address-cells/#size-cells van een node (default 2/2 — wat
// elke Apple-SoC voert die wij zien).
func (t Tree) cells(off Node) (ac, sc uint32) {
	return t.U32(off, "#address-cells", 2), t.U32(off, "#size-cells", 2)
}

// readCells leest n cellen van 32 bits als één getal (little-endian, laagste
// cel eerst — de ADT is little-endian, anders dan FDT).
func readCells(addr uintptr, n uint32) uint64 {
	var v uint64
	for i := uint32(0); i < n && i < 2; i++ {
		v |= uint64(dev.Read32(addr+uintptr(i)*4)) << (32 * i)
	}
	return v
}

// RegAt geeft het i-de (adres, grootte)-paar van de node aan het einde van de
// keten, vertaald naar een fysiek adres.
func (t Tree) RegAt(chain []Node, i int) (base, size uint64, ok bool) {
	if len(chain) < 2 {
		return 0, 0, false
	}
	self := chain[len(chain)-1]
	ac, sc := t.cells(chain[len(chain)-2]) // de OUDER bepaalt de breedte
	if ac == 0 || ac > 2 || sc > 2 {
		return 0, 0, false
	}
	addr, sz, ok := t.Prop(self, "reg")
	if !ok {
		return 0, 0, false
	}
	entry := uintptr(ac+sc) * 4
	o := uintptr(i) * entry
	if uintptr(sz) < o+entry {
		return 0, 0, false
	}
	base = readCells(addr+o, ac)
	size = readCells(addr+o+uintptr(ac)*4, sc)

	// Omhoog door de keten: elke ouder met ranges vertaalt.
	for k := len(chain) - 2; k >= 1; k-- {
		b, done := t.translate(chain[k], chain[k-1], base)
		if done {
			return 0, 0, false
		}
		base = b
	}
	return base, size, true
}

// translate past de ranges van `node` toe op een busadres. Heeft de node geen
// ranges, dan houdt de vertaling daar op (het adres is dan al fysiek) — dat is
// hetzelfde `break` als in m1n1's translate().
func (t Tree) translate(node, parent Node, addr uint64) (uint64, bool) {
	raddr, rsz, ok := t.Prop(node, "ranges")
	if !ok {
		return addr, false
	}
	ac, sc := t.cells(node)
	pac, _ := t.cells(parent)
	if ac == 0 || ac > 2 || sc > 2 || pac == 0 || pac > 2 {
		return addr, true
	}
	entry := uintptr(ac+pac+sc) * 4
	if entry == 0 {
		return addr, true
	}
	for o := uintptr(0); o+entry <= uintptr(rsz); o += entry {
		bus := readCells(raddr+o, ac)
		par := readCells(raddr+o+uintptr(ac)*4, pac)
		size := readCells(raddr+o+uintptr(ac+pac)*4, sc)
		if addr >= bus && addr-bus < size {
			return addr - bus + par, false
		}
	}
	return addr, false
}

// Reg is het gemak voor het gewone geval: pad opzoeken en het i-de venster
// vertaald teruggeven.
func (t Tree) RegOf(path string, i int) (base, size uint64, ok bool) {
	chain, ok := t.PathTrace(path)
	if !ok {
		return 0, 0, false
	}
	return t.RegAt(chain, i)
}
