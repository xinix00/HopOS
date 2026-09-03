// Package xnuboot leest het boot_args-blok dat iBoot op elke Apple-SoC in x0
// meegeeft: het RAM-contract, het framebuffer en het adres van de device tree.
// m1n1 krijgt dat blok nu, wij straks — dit is de reden dat het param-blok van
// board/apple kan verdwijnen zodra we zelf het bootobject zijn.
//
// Naast fw/fdt en fw/adt omdat het net zo goed een firmware-formaat is, en
// omdat het hier host-testbaar is: board/apple zelf bouwt alleen voor tamago.
//
// Formaat: m1n1 src/xnuboot.h. De lengte van het cmdline-veld hangt aan de
// revisie (1 → 256, 2 → 608, 3 → 1024 bytes), en daarachter staan boot_flags en
// de échte RAM-grootte. Deze mini meldt revisie 3.
package xnuboot

import "github.com/xinix00/HopOS/metal/v2/dev"

// Vaste offsets in het blok (natuurlijke uitlijning, arm64).
const (
	offRevision    = 0x00 // u16
	offVersion     = 0x02 // u16
	offVirtBase    = 0x08
	offPhysBase    = 0x10
	offMemSize     = 0x18
	offTopOfKern   = 0x20
	offVideoBase   = 0x28
	offVideoStride = 0x38
	offVideoW      = 0x40
	offVideoH      = 0x48
	offVideoDepth  = 0x50
	offMachType    = 0x58 // u32
	offDevTree     = 0x60 // pointer, VIRTUEEL adres
	offDevTreeSize = 0x68 // u32
	offCmdline     = 0x70
)

// Args is wat we ervan gebruiken.
type Args struct {
	Revision, Version uint32
	VirtBase          uint64
	PhysBase          uint64 // begin van het RAM dat van ons is
	MemSize           uint64 // ... en hoeveel
	TopOfKernelData   uint64 // tot waar de firmware het zelf gevuld heeft
	MemSizeActual     uint64 // het fysiek aanwezige RAM
	FB                struct{ Base, Stride, W, H, Depth uint64 }
	ADT               uint64 // FYSIEK adres van de device tree
	ADTSize           uint32
	DevTree           uint64 // ... zoals het er staat: VIRTUEEL, ongecorrigeerd
}

// read64 leest in twee helften: boot_args ligt in geheugen dat we op dit moment
// van de boot ongecachet en zonder garanties over 64-bit-toegang benaderen —
// dezelfde reden als bij de ADT-lezer.
func read64(p uintptr) uint64 {
	return uint64(dev.Read32(p)) | uint64(dev.Read32(p+4))<<32
}

// Read leest het blok op adres p. ok=false als het er niet plausibel
// uitziet: dit is firmware-input en een verkeerde pointer moet een lege waarde
// opleveren, geen halve waarheid.
func Read(p uintptr) (Args, bool) {
	var b Args
	if p == 0 {
		return b, false
	}
	// Revisie en versie delen één woord (u16 + u16). Ze los uitlezen zou een
	// scheve 32-bit toegang zijn — op de host levert dat stil de verkeerde
	// helft, op device-geheugen een alignment-fault.
	rv := dev.Read32(p + offRevision)
	b.Revision, b.Version = rv&0xffff, rv>>16
	if b.Revision == 0 || b.Revision > 3 {
		return b, false
	}
	b.VirtBase = read64(p + offVirtBase)
	b.PhysBase = read64(p + offPhysBase)
	b.MemSize = read64(p + offMemSize)
	b.TopOfKernelData = read64(p + offTopOfKern)
	if b.PhysBase == 0 || b.MemSize == 0 || b.TopOfKernelData < b.PhysBase {
		return b, false
	}
	b.FB.Base = read64(p + offVideoBase)
	b.FB.Stride = read64(p + offVideoStride)
	b.FB.W = read64(p + offVideoW)
	b.FB.H = read64(p + offVideoH)
	b.FB.Depth = read64(p + offVideoDepth)

	// De device tree staat er als VIRTUEEL adres in; het fysieke volgt uit het
	// verschil dat de firmware zelf meegeeft. Wie dat vergeet leest de boom op
	// een adres dat alleen voor de firmware bestond.
	b.DevTree = read64(p + offDevTree)
	b.ADTSize = dev.Read32(p + offDevTreeSize)
	//
	// Rekenen doen we MODULO 2^64 en zonder volgorde-aanname. Hier stond
	// `if dt >= b.VirtBase`, en dat leek redelijk: een virtueel adres ligt
	// boven zijn basis. Maar iBoot geeft virt_base niet altijd in dezelfde
	// vorm — gemeten 30-08 op de M4, twee boots van hetzelfde image:
	//
	//	virt_base 0x5374000          devtree 0x6388000   → ADT 0x10002388000
	//	virt_base 0xffffffffff374000 devtree 0x1614000   → guard faalt, ADT 0
	//
	// Dezelfde lage bits, de tweede keer mét de hoge kernelbits erin. Het
	// verschil is in beide gevallen goed zodra je het gewoon laat wrappen;
	// alleen de vergelijking was fout. Dat kostte een boot waarin álles
	// wegviel wat aan de boom hangt (PCIe, dus ook netwerk en NVMe) — het
	// "1 van de 4 chain-boots"-raadsel uit de TODO.
	//
	// De controle die ervoor in de plaats komt zegt wél iets: het resultaat
	// moet in het DRAM liggen dat de firmware zelf beschrijft.
	dt := read64(p + offDevTree)
	if adt := dt - b.VirtBase + b.PhysBase; adt >= b.PhysBase && adt-b.PhysBase < b.MemSize {
		b.ADT = adt
	}

	// Achter het cmdline-veld staan de vlaggen en de échte RAM-grootte; de
	// lengte van dat veld hangt aan de revisie.
	cmdlen := uint64(256)
	switch b.Revision {
	case 2:
		cmdlen = 608
	case 3:
		cmdlen = 1024
	}
	b.MemSizeActual = read64(p + offCmdline + uintptr(cmdlen) + 8)
	return b, true
}
