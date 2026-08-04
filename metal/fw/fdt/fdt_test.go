// Host-tests voor de DTB-lezer. De blob wordt hier in gewoon heap-geheugen
// gebouwd en via zijn adres gelezen — metal/dev doet op de host normale
// memory-access (zelfde patroon als abi/ring en net/hopswitch), dus dit toetst
// exact de offset-rekenkunde die op ijzer de firmware-input verwerkt.
package fdt

import (
	"testing"
	"unsafe"
)

// builder bouwt een DTB met een geldige v17-header, inclusief de
// blokgroottes (size_dt_struct/size_dt_strings) die echte firmware ook vult.
type builder struct {
	structs []byte
	strs    []byte
	rsv     []byte
}

func app32(dst []byte, v uint32) []byte {
	return append(dst, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

func app64(dst []byte, v uint64) []byte {
	return app32(app32(dst, uint32(v>>32)), uint32(v))
}

func pad4(dst []byte) []byte {
	for len(dst)%4 != 0 {
		dst = append(dst, 0)
	}
	return dst
}

// strOff legt een property-naam in het strings-block (hergebruik bij herhaling,
// zoals dtc doet) en geeft zijn offset.
func (b *builder) strOff(name string) uint32 {
	for i := 0; i+len(name) < len(b.strs); i++ {
		if string(b.strs[i:i+len(name)]) == name && b.strs[i+len(name)] == 0 {
			return uint32(i)
		}
	}
	off := uint32(len(b.strs))
	b.strs = append(append(b.strs, name...), 0)
	return off
}

func (b *builder) begin(name string) *builder {
	b.structs = app32(b.structs, tokBegin)
	b.structs = pad4(append(append(b.structs, name...), 0))
	return b
}

func (b *builder) end() *builder {
	b.structs = app32(b.structs, tokEnd)
	return b
}

func (b *builder) prop(name string, data []byte) *builder {
	b.structs = app32(b.structs, tokProp)
	b.structs = app32(b.structs, uint32(len(data)))
	b.structs = app32(b.structs, b.strOff(name))
	b.structs = pad4(append(b.structs, data...))
	return b
}

func (b *builder) propU32(name string, v uint32) *builder {
	return b.prop(name, app32(nil, v))
}

func (b *builder) reserve(addr, size uint64) *builder {
	b.rsv = app64(app64(b.rsv, addr), size)
	return b
}

func (b *builder) blob() []byte {
	structs := app32(b.structs, tokEndTree)
	rsv := append(append([]byte{}, b.rsv...), make([]byte, 16)...) // {0,0}-terminator
	rsvOff := hdrLen
	structOff := rsvOff + len(rsv)
	stringsOff := structOff + len(structs)
	total := stringsOff + len(b.strs)

	out := make([]byte, 0, total)
	out = app32(out, magic)
	out = app32(out, uint32(total))
	out = app32(out, uint32(structOff))
	out = app32(out, uint32(stringsOff))
	out = app32(out, uint32(rsvOff))
	out = app32(out, 17) // version
	out = app32(out, 16) // last_comp_version
	out = app32(out, 0)  // boot_cpuid_phys
	out = app32(out, uint32(len(b.strs)))
	out = app32(out, uint32(len(structs)))
	out = append(out, rsv...)
	out = append(out, structs...)
	out = append(out, b.strs...)
	return out
}

// live houdt elke testblob in leven zolang zijn adres nog als uintptr rondgaat.
var live [][]byte

func at(blob []byte) uintptr {
	live = append(live, blob)
	return uintptr(unsafe.Pointer(&blob[0]))
}

// pi bouwt een DTB in de vorm die de Pi-firmware afgeeft: twee memory-banken,
// /chosen met bootargs + initrd, een framebuffer-knoop en een memreserve.
func pi() *builder {
	b := &builder{}
	b.begin("").
		propU32("#address-cells", 2).
		propU32("#size-cells", 2).
		prop("serial-number", []byte("100000001a2b3c4d\x00"))
	b.begin("memory@0").
		prop("reg", app64(app64(nil, 0x40000000), 0x40000000)).
		end()
	b.begin("memory@80000000").
		prop("reg", app64(app64(nil, 0x80000000), 0x100000000)).
		end()
	b.begin("chosen").
		prop("bootargs", []byte("console=serial0,115200 hopos.node=hop-1 hopos.wd=off\x00")).
		prop("linux,initrd-start", app32(nil, 0x2000000)).
		prop("linux,initrd-end", app32(nil, 0x2000100))
	b.begin("framebuffer@3e000000").
		prop("reg", app64(app64(nil, 0x3e000000), 0x800000)).
		propU32("width", 1920).
		propU32("height", 1080).
		propU32("stride", 7680).
		prop("format", []byte("a8r8g8b8\x00")).
		end()
	b.end() // chosen
	b.end() // root
	b.reserve(0x3f000000, 0x100000)
	return b
}

func TestMemRegions(t *testing.T) {
	base := at(pi().blob())
	regs, ok := MemRegions(base)
	if !ok || len(regs) != 2 {
		t.Fatalf("MemRegions = %v, %v — wil twee banken", regs, ok)
	}
	if regs[0] != (Region{0x40000000, 0x40000000}) || regs[1] != (Region{0x80000000, 0x100000000}) {
		t.Errorf("banken = %v", regs)
	}
	total, ok := MemTotal(base)
	if !ok || total != 0x140000000 {
		t.Errorf("MemTotal = %#x, %v — wil 0x140000000", total, ok)
	}
}

func TestChosenEnRoot(t *testing.T) {
	base := at(pi().blob())
	args, ok := Bootargs(base)
	if !ok || args != "console=serial0,115200 hopos.node=hop-1 hopos.wd=off" {
		t.Errorf("Bootargs = %q, %v", args, ok)
	}
	s, ok := RootString(base, "serial-number")
	if !ok || s != "100000001a2b3c4d" {
		t.Errorf("serial-number = %q, %v", s, ok)
	}
	if _, ok := RootString(base, "bootargs"); ok {
		t.Error("bootargs is een /chosen-property, niet van de root")
	}
	start, end, ok := InitrdRegion(base)
	if !ok || start != 0x2000000 || end != 0x2000100 {
		t.Errorf("InitrdRegion = %#x..%#x, %v", start, end, ok)
	}
}

func TestFramebufferEnMemReserve(t *testing.T) {
	base := at(pi().blob())
	fb, ok := Framebuffer(base)
	if !ok {
		t.Fatal("Framebuffer niet gevonden")
	}
	if fb.Base != 0x3e000000 || fb.Width != 1920 || fb.Height != 1080 || fb.Stride != 7680 || fb.BPP != 32 {
		t.Errorf("FB = %+v", fb)
	}
	rs := MemReserve(base)
	if len(rs) != 1 || rs[0] != (Region{0x3f000000, 0x100000}) {
		t.Errorf("MemReserve = %v", rs)
	}
}

// DE REGRESSIE (04-08, drie weken vermomd als "32-bpp-freeze"): de échte
// Pi-firmware geeft /chosen een eigen #address-cells=1/#size-cells=1 en
// schrijft de framebuffer-reg als <u32 base><u32 size>. De parser las met de
// root-cellen (2) en plakte base en size aaneen tot 0x3f800000003f4800 —
// waarna de eerste pixel-veeg een bus-fault-reset was (gemeten, FBDBG). De
// pi()-testblob hierboven bouwde de node met root-cellen en bevestigde dus de
// verkeerde aanname; deze blob bouwt hem zoals het ijzer hem geeft.
func TestFramebufferMetChosenCellen(t *testing.T) {
	b := &builder{}
	b.begin("").
		propU32("#address-cells", 2).
		propU32("#size-cells", 2)
	b.begin("chosen").
		propU32("#address-cells", 1).
		propU32("#size-cells", 1)
	b.begin("framebuffer@3f800000").
		prop("reg", app32(app32(nil, 0x3f800000), 0x3f4800)).
		propU32("width", 1920).
		propU32("height", 1080).
		propU32("stride", 3840).
		prop("format", []byte("r5g6b5\x00")).
		end()
	b.end() // chosen
	b.end() // root
	fb, ok := Framebuffer(at(b.blob()))
	if !ok {
		t.Fatal("Framebuffer niet gevonden")
	}
	if fb.Base != 0x3f800000 || fb.Stride != 3840 || fb.BPP != 16 {
		t.Errorf("FB = %+v — base/stride/bpp moeten uit de CHOSEN-cellen komen", fb)
	}
}

// DE REGRESSIE: Valid accepteerde een header die zichzelf niet kan dragen (een
// gedeclareerde grootte vanaf 8 bytes), terwijl elke reader daarna +8/+12/+16
// leest. Nu is Valid dezelfde toets als de readers doen.
func TestValidWeigertEenTeKleineHeader(t *testing.T) {
	short := app32(app32(nil, magic), 8) // magic + totalsize 8, verder niets
	short = append(short, make([]byte, 4)...)
	if Valid(at(short)) {
		t.Error("Valid accepteerde een blob van 8 gedeclareerde bytes")
	}
	if BlobSize(at(short)) != 0 {
		t.Error("BlobSize gaf een maat voor een onbruikbare header")
	}

	if Valid(0) {
		t.Error("Valid(0) hoort false te zijn")
	}
	garbage := app32(app32(nil, 0xdeadbeef), 1024)
	garbage = append(garbage, make([]byte, 64)...)
	if Valid(at(garbage)) {
		t.Error("Valid accepteerde een blob zonder FDT-magic")
	}

	// En de echte blob moet er natuurlijk wél door.
	blob := pi().blob()
	base := at(blob)
	if !Valid(base) {
		t.Error("Valid weigerde een geldige DTB")
	}
	if got := BlobSize(base); got != uint64(len(blob)) {
		t.Errorf("BlobSize = %d, wil %d", got, len(blob))
	}
}

// Een header met offsets buiten de gedeclareerde blob mag geen enkele reader
// aan het lezen zetten — ook niet met een op zichzelf plausibele totalsize.
func TestKrommeOffsetsWordenGeweigerd(t *testing.T) {
	blob := pi().blob()
	for _, tc := range []struct {
		name string
		off  int // header-offset van het te verzieken veld
	}{
		{"off_dt_struct", 8},
		{"off_dt_strings", 12},
		{"off_mem_rsvmap", 16},
	} {
		broken := append([]byte{}, blob...)
		copy(broken[tc.off:], app32(nil, uint32(len(blob)+0x1000)))
		base := at(broken)
		if Valid(base) {
			t.Errorf("%s buiten de blob werd geaccepteerd", tc.name)
		}
		if _, ok := MemRegions(base); ok {
			t.Errorf("%s buiten de blob: MemRegions las alsnog", tc.name)
		}
		if _, ok := Bootargs(base); ok {
			t.Errorf("%s buiten de blob: Bootargs las alsnog", tc.name)
		}
		if _, ok := Framebuffer(base); ok {
			t.Errorf("%s buiten de blob: Framebuffer las alsnog", tc.name)
		}
		if rs := MemReserve(base); rs != nil {
			t.Errorf("%s buiten de blob: MemReserve las alsnog (%v)", tc.name, rs)
		}
	}
}

// Een nameoff die buiten het strings-block wijst is de klassieke corruptie: de
// property wordt overgeslagen, er wordt niet buiten het blok gelezen, en de
// rest van de boom blijft leesbaar.
func TestNameoffBuitenStringsBlok(t *testing.T) {
	b := &builder{}
	b.begin("").propU32("#address-cells", 2).propU32("#size-cells", 2)
	b.begin("memory@0").prop("reg", app64(app64(nil, 0x40000000), 0x1000000)).end()
	b.end()
	blob := b.blob()

	// De nameoff van de laatste property (reg) ver buiten het strings-block
	// zetten: zoek het laatste FDT_PROP-token en verzien zijn nameoff-cel.
	structOff := int(be32At(blob, 8))
	structSize := int(be32At(blob, 36))
	last := -1
	for p := structOff; p+12 <= structOff+structSize; p += 4 {
		if be32At(blob, p) == tokProp {
			last = p
		}
	}
	if last < 0 {
		t.Fatal("geen FDT_PROP gevonden in de testblob")
	}
	copy(blob[last+8:], app32(nil, 0xffff))

	if _, ok := MemRegions(at(blob)); ok {
		t.Error("reg met een kromme nameoff werd alsnog als /memory-reg gelezen")
	}
}

func be32At(b []byte, off int) uint32 {
	return uint32(b[off])<<24 | uint32(b[off+1])<<16 | uint32(b[off+2])<<8 | uint32(b[off+3])
}
