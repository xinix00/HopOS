// Host-tests voor de ADT-lezer. De boom wordt hier in gewoon heap-geheugen
// gebouwd en via zijn adres gelezen — metal/dev doet op de host normale
// memory-access (zelfde patroon als fw/fdt), dus dit toetst exact de
// offset-rekenkunde die op ijzer de firmware-input verwerkt.
package adt

import (
	"testing"
	"unsafe"
)

// node bouwt een ADT-node: properties (naam → waarde) en kinderen.
type node struct {
	name     string
	props    [][2]interface{} // {naam string, waarde []byte}
	children []*node
}

func u32(v uint32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
func u64(v uint64) []byte { return append(u32(uint32(v)), u32(uint32(v>>32))...) }

func (n *node) encode() []byte {
	props := [][2]interface{}{{"name", []byte(n.name + "\x00")}}
	props = append(props, n.props...)

	out := append(u32(uint32(len(props))), u32(uint32(len(n.children)))...)
	for _, p := range props {
		name := p[0].(string)
		val, _ := p[1].([]byte)
		var nb [propNameLen]byte
		copy(nb[:], name)
		out = append(out, nb[:]...)
		out = append(out, u32(uint32(len(val)))...)
		out = append(out, val...)
		for len(out)%4 != 0 {
			out = append(out, 0)
		}
	}
	for _, c := range n.children {
		out = append(out, c.encode()...)
	}
	return out
}

// tree bouwt een ADT en opent hem op zijn heap-adres.
func tree(t *testing.T, root *node) Tree {
	t.Helper()
	b := root.encode()
	tr, ok := Open(uintptr(unsafe.Pointer(&b[0])), uint32(len(b)))
	if !ok {
		t.Fatal("Open weigerde een geldige boom")
	}
	t.Cleanup(func() { _ = b }) // b levend houden zolang de test loopt
	return tr
}

func sample() *node {
	return &node{
		name: "device-tree",
		props: [][2]interface{}{
			{"compatible", []byte("j773gap\x00")},
		},
		children: []*node{
			{
				name:  "cpus",
				props: [][2]interface{}{{"#address-cells", u32(2)}},
				children: []*node{
					{name: "cpu0", props: [][2]interface{}{
						{"reg", u32(0)}, {"cpu-impl-reg", u64(0x210050000)},
					}},
					{name: "cpu6", props: [][2]interface{}{
						{"reg", u32(0x100)}, {"cpu-impl-reg", u64(0x211050000)},
					}},
				},
			},
			{
				name: "arm-io",
				children: []*node{
					{name: "uart0", props: [][2]interface{}{
						{"reg", append(u64(0x3ad200000), u64(0x4000)...)},
					}},
					{name: "ans", props: [][2]interface{}{
						{"nvme-secure-bar", []byte{}},
					}},
				},
			},
		},
	}
}

func TestPathAndProps(t *testing.T) {
	tr := tree(t, sample())

	if n, ok := tr.Path("/"); !ok || n != 0 {
		t.Fatalf("wortel: %v %v", n, ok)
	}
	if name, ok := tr.Name(0); !ok || name != "device-tree" {
		t.Fatalf("wortelnaam %q %v", name, ok)
	}

	u, ok := tr.Path("/arm-io/uart0")
	if !ok {
		t.Fatal("/arm-io/uart0 niet gevonden")
	}
	base, size, ok := tr.RegOf("/arm-io/uart0", 0)
	if !ok || base != 0x3ad200000 || size != 0x4000 {
		t.Fatalf("uart0 reg = %#x+%#x (%v)", base, size, ok)
	}
	if _, _, ok := tr.RegOf("/arm-io/uart0", 1); ok {
		t.Fatal("reg[1] bestaat niet en werd tóch geleverd")
	}
	_ = u

	// Een node zónder de gezochte property, en een pad dat niet bestaat.
	if _, _, ok := tr.Prop(u, "nvme-secure-bar"); ok {
		t.Fatal("uart0 draagt geen nvme-secure-bar")
	}
	if _, ok := tr.Path("/arm-io/does-not-exist"); ok {
		t.Fatal("onbestaand pad werd gevonden")
	}
}

func TestChildrenAndNameSeparators(t *testing.T) {
	tr := tree(t, sample())
	cpus, ok := tr.Path("/cpus")
	if !ok {
		t.Fatal("/cpus niet gevonden")
	}

	var seen []string
	tr.Children(cpus, func(n Node) bool {
		name, _ := tr.Name(n)
		seen = append(seen, name)
		return true
	})
	if len(seen) != 2 || seen[0] != "cpu0" || seen[1] != "cpu6" {
		t.Fatalf("kinderen van /cpus: %v", seen)
	}

	// '-' en '_' zijn in ADT-namen uitwisselbaar; de firmware is daar niet
	// consequent in.
	if v := tr.U32(cpus, "#address_cells", 0); v != 2 {
		t.Fatalf("#address_cells = %d, verwacht 2 (streepje/underscore gelijk)", v)
	}

	c6, ok := tr.Child(cpus, "cpu6")
	if !ok {
		t.Fatal("cpu6 niet gevonden")
	}
	if v := tr.U32(c6, "reg", 0xffff); v != 0x100 {
		t.Fatalf("cpu6 reg = %#x", v)
	}
	if v := tr.U64(c6, "cpu-impl-reg", 0); v != 0x211050000 {
		t.Fatalf("cpu6 impl = %#x", v)
	}
}

// Een lege property (size 0) is geldig en betekent "deze vlag staat aan" —
// nvme-secure-bar is er zo een, en daar hangt op de M4 de hele NVMe-registerkaart
// aan. Hij moet gevonden worden, niet weggevallen zijn.
func TestEmptyPropertyIsAFlag(t *testing.T) {
	tr := tree(t, sample())
	ans, ok := tr.Path("/arm-io/ans")
	if !ok {
		t.Fatal("/arm-io/ans niet gevonden")
	}
	if _, size, ok := tr.Prop(ans, "nvme-secure-bar"); !ok || size != 0 {
		t.Fatalf("nvme-secure-bar: size %d ok=%v", size, ok)
	}
}

// Onvertrouwde input: een gedeclareerde grootte die niet klopt mag geen paniek
// geven en geen geheugen buiten de boom lezen.
func TestBrokenTreeIsRefused(t *testing.T) {
	b := sample().encode()
	if _, ok := Open(uintptr(unsafe.Pointer(&b[0])), 4); ok {
		t.Fatal("een boom van 4 bytes werd geaccepteerd")
	}
	if _, ok := Open(0, uint32(len(b))); ok {
		t.Fatal("nul-adres werd geaccepteerd")
	}
	if _, ok := Open(uintptr(unsafe.Pointer(&b[0])), maxTree+1); ok {
		t.Fatal("onzinnige grootte werd geaccepteerd")
	}

	// Een boom die halverwege afgekapt is: Path moet netjes falen.
	tr, ok := Open(uintptr(unsafe.Pointer(&b[0])), uint32(len(b)/2))
	if ok {
		if _, found := tr.Path("/arm-io/uart0"); found {
			t.Fatal("pad gevonden in een afgekapte boom")
		}
	}
}

// De ranges-vertaling met de echte getallen van de M4-mini: de opslag-
// coprocessor heet ans@81600000 en woont op 0x481600000. Wie de vertaling
// overslaat krijgt een adres dat er plausibel uitziet en nergens heen wijst —
// precies de fout die je pas op ijzer merkt.
func TestRangesTranslation(t *testing.T) {
	armIO := &node{
		name: "arm-io",
		props: [][2]interface{}{
			{"#address-cells", u32(2)},
			{"#size-cells", u32(2)},
			// bus 0x0 → ouder 0x400000000, venster 8GB
			{"ranges", append(append(u64(0), u64(0x400000000)...), u64(0x200000000)...)},
		},
		children: []*node{
			{name: "ans", props: [][2]interface{}{
				{"reg", append(u64(0x81600000), u64(0x88000)...)},
			}},
			{name: "sart-ans", props: [][2]interface{}{
				{"reg", append(u64(0x85c50000), u64(0xc000)...)},
			}},
		},
	}
	tr := tree(t, &node{
		name: "device-tree",
		props: [][2]interface{}{
			{"#address-cells", u32(2)}, {"#size-cells", u32(2)},
		},
		children: []*node{armIO},
	})

	for _, c := range []struct {
		path string
		want uint64
	}{
		{"/arm-io/ans", 0x481600000},
		{"/arm-io/sart-ans", 0x485c50000},
	} {
		got, _, ok := tr.RegOf(c.path, 0)
		if !ok || got != c.want {
			t.Fatalf("%s → %#x (%v), verwacht %#x", c.path, got, ok, c.want)
		}
	}
}

// Een node buiten élk ranges-venster hoort onvertaald terug te komen, niet
// stilletjes op een verkeerd adres te landen.
func TestAddressOutsideRangesStaysPut(t *testing.T) {
	tr := tree(t, &node{
		name:  "device-tree",
		props: [][2]interface{}{{"#address-cells", u32(2)}, {"#size-cells", u32(2)}},
		children: []*node{{
			name: "bus",
			props: [][2]interface{}{
				{"#address-cells", u32(2)}, {"#size-cells", u32(2)},
				{"ranges", append(append(u64(0x1000), u64(0x900000000)...), u64(0x1000)...)},
			},
			children: []*node{{name: "dev", props: [][2]interface{}{
				{"reg", append(u64(0xdead0000), u64(0x100)...)},
			}}},
		}},
	})
	got, _, ok := tr.RegOf("/bus/dev", 0)
	if !ok || got != 0xdead0000 {
		t.Fatalf("buiten het venster → %#x (%v), verwacht onvertaald 0xdead0000", got, ok)
	}
}

// Cellen van 32 bits (ac=1) komen op Apple-SoC's voor bij sommige bussen; de
// lezer moet ze aankunnen zonder acht bytes te lezen waar er vier staan.
func TestSingleCellAddresses(t *testing.T) {
	tr := tree(t, &node{
		name:  "device-tree",
		props: [][2]interface{}{{"#address-cells", u32(2)}, {"#size-cells", u32(2)}},
		children: []*node{{
			name: "bus",
			props: [][2]interface{}{
				{"#address-cells", u32(1)}, {"#size-cells", u32(1)},
			},
			children: []*node{{name: "dev", props: [][2]interface{}{
				{"reg", append(u32(0x1234), u32(0x40)...)},
			}}},
		}},
	})
	base, size, ok := tr.RegOf("/bus/dev", 0)
	if !ok || base != 0x1234 || size != 0x40 {
		t.Fatalf("1-cel reg → %#x+%#x (%v)", base, size, ok)
	}
}
