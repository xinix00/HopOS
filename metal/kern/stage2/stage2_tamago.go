//go:build tamago

package stage2

import (
	"fmt"
	"unsafe"

	"github.com/xinix00/HopOS/metal/abi/checksum"
	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/cpu/el2"
	"github.com/xinix00/HopOS/metal/dev"
)

// hvcRevoke doet HVC #0 vanuit EL1 → de revoke-vector op EL2 (TLBI ALLE1IS).
// De handler klobbert alleen x16 (de immediate-keuze); Go behandelt dat als
// caller-saved. Zie revoke_arm64.s en de handler in InitVectors.
func hvcRevoke()

// switchMagic spelt "HOPSWTC1" (little-endian): de descriptor van de
// switch-code-kopie in de plan-regio.
const switchMagic = 0x3143545753504F48

// De descriptor-indeling op layout.SwitchCodePA (64 bytes, dan de blobs):
//
//	+0   magic
//	+8   totale lengte (descriptor + blobs)
//	+16  FNV-1a-64 over de blobs (in kopieervolgorde)
//	+24  offset van el2entry    (t.o.v. de descriptor-basis)
//	+32  offset van s2tramp
//	+40  offset van smpEL2Tramp
const (
	swLen   = 8
	swHash  = 16
	swEntry = 24
	swTramp = 32
	swSMP   = 40
	swHead  = 0x40
)

// imageBlobs geeft de drie EL2-blobs als slices ÍN het kern-image, in de
// volgorde van el2.BlobSymbols: el2entry, s2tramp, smpEL2Tramp. Eén pass —
// zowel de som als de kopie hieronder lopen hierover, want twee keer
// extraheren is twee kansen om het net anders te doen.
func imageBlobs() [][]byte {
	e, eEnd, t, tEnd, s, sEnd := el2.ImageBlobs()
	if e == 0 {
		return nil
	}
	out := make([][]byte, 0, 3)
	for _, r := range [3][2]uint64{{e, eEnd}, {t, tEnd}, {s, sEnd}} {
		if r[1] <= r[0] || r[1]-r[0] > el2.MaxBlobSize {
			panic(fmt.Sprintf("stage2: switch-blob heeft onmogelijke maat %#x..%#x — eindmarker verschoven?", r[0], r[1]))
		}
		out = append(out, unsafe.Slice((*byte)(unsafe.Pointer(uintptr(r[0]))), r[1]-r[0]))
	}
	return out
}

// BlobBytes plakt ze aaneen: dít is waar SwitchCodeHash de som over rekent, en
// kern/kernflip rekent dezelfde som over de blobs uit een NIEUWE kern-bundel —
// gelijke som betekent dat een app-core die nu in deze code draait de flip
// ongestoord overleeft.
func BlobBytes() []byte {
	var out []byte
	for _, b := range imageBlobs() {
		out = append(out, b...)
	}
	return out
}

// SwitchCodeHash geeft de som die in de descriptor van de geïnstalleerde
// kopie staat (0 = geen geldige kopie in de plan-regio). De flip-laag toetst
// hem tegen de nieuwe kern vóór hij springt.
func SwitchCodeHash() uint64 {
	base := layout.SwitchCodePA()
	if dev.Read64(base) != switchMagic {
		return 0
	}
	return dev.Read64(base + swHash)
}

// adopting markeert dat deze boot een geadopteerde flip is: er DRAAIEN al
// app-cores in de plan-regio (in de switch-code-kopie, op hun sched-blokken en
// ctx-blokken). Alles wat InitVectors normaal vers neerzet, moet dan blijven
// staan — zie de skips daar. Zetten vóór EnsureVectors.
var adopting bool

// SetAdopting zet de adoptie-stand (kern/kernflip, vroeg in de boot).
func SetAdopting(v bool) { adopting = v }

// flipCapable: mag deze node zichzelf later vervangen? Zo niet, dan blijft de
// switch-code in het kern-image staan en is het gedrag byte-voor-byte dat van
// vóór de kern-flip bestond.
//
// Dit is de énige plek waar de flip een node raakt die er nooit gebruik van
// maakt: alles wat er verder bij hoort (bundel lezen, plaatsen, adopteren) is
// inert tot er echt geflipt wordt, maar de verhuizing zelf gebeurt bij ELKE
// boot en verandert waar een app-core zijn instructies vandaan haalt.
//
// Bewust een runtime-vlag en géén build-tag: twee build-smaken betekent dat de
// smaak die je test niet de smaak is die draait, en die drift heeft deze boom
// al eens vijf boot-cycli gekost (zie dev.RealCacheOps).
var flipCapable bool

// SetFlipCapable zet die stand; aanroepen vóór de eerste EnsureVectors.
func SetFlipCapable(v bool) { flipCapable = v }

// installSwitchCode kopieert de drie EL2-blobs (el2entry, s2tramp,
// smpEL2Tramp) uit het kern-image naar de plan-regio (layout.SwitchCodePA) en
// schakelt de el2-accessors om (docs/kern-flip.md): vanaf hier voert een
// app-core nooit meer kern-image-bytes uit, dus kan een kern-flip het oude
// venster verlaten terwijl geyielde/geparkeerde cores gewoon doordraaien.
//
// Bij een ADOPTIE draaien er al cores in de zittende kopie. Dan wordt er niets
// gekopieerd — alleen de accessors gaan om naar de bestaande adressen. Dat mag
// precies omdat de flip-laag vooraf toetste dat de som gelijk is: een kern die
// andere switch-code draagt kan met levende bewoners niet eens vertrekken.
//
// Klopt de som hier tóch niet, dan is de aanname van dat contract gebroken
// (een bug, of DRAM dat corrumpeerde tussen de toets en de sprong) en is er
// geen nette uitweg meer: we zijn al gesprongen, en een kern zónder werkende
// switch-code kan niets. Dus installeren we alsnog vers, en trekken we de
// adoptie-stand in — wat twee dingen betekent, allebei bedoeld:
//
//   - InitVectors zet de app-core-regio vers neer (thunks, parkeerlus,
//     sched-blokken, ctx-staten), dus de bewoners van de vorige kern zijn hoe
//     dan ook weg;
//   - kern/slots ziet dat via cageAdoptable en geeft hun partities vrij in
//     plaats van ze te reserveren voor apps die niet meer draaien.
//
// Cores die op dít moment ín de oude kopie stonden zijn daarmee verloren. Dat
// is het eerlijke antwoord op een onmogelijke toestand — en de node-watchdog
// is het vangnet als de node er alsnog door omvalt.
func installSwitchCode() {
	blobs := imageBlobs()
	if blobs == nil {
		return // geen EL2-blobs op deze architectuur/host
	}
	// Niet flip-baar en niets te adopteren: laat de switch-code in het image
	// staan. De accessors geven dan de image-adressen — exact het oude gedrag,
	// met dezelfde binary.
	if !flipCapable && !adopting {
		return
	}
	base := layout.SwitchCodePA()
	var flat []byte
	for _, b := range blobs {
		flat = append(flat, b...)
	}
	sum := checksum.FNV64(flat)

	if adopting {
		if dev.Read64(base) != switchMagic || dev.Read64(base+swHash) != sum {
			fmt.Printf("HOPOS_FLIP_SWITCHCODE_MISMATCH: resident switch code (%#x) is not ours (%#x) — refusing to adopt residents\n",
				dev.Read64(base+swHash), sum)
			adopting = false
		} else {
			b := uint64(base)
			el2.SetRelocated(b+dev.Read64(base+swEntry),
				b+dev.Read64(base+swTramp),
				b+dev.Read64(base+swSMP))
			return
		}
	}

	// Zelfde blobs als waar de som over ging (imageBlobs), nu als kopie.
	off := uintptr(swHead)
	dst := make([]uintptr, 0, 3)
	for _, b := range blobs {
		dev.Copy(base+off, b)
		dst = append(dst, base+off)
		off += (uintptr(len(b)) + 63) &^ 63
		if off > uintptr(layout.SwitchCodeMax) {
			panic(fmt.Sprintf("stage2: switch-code-kopie past niet (%#x > %#x) — blok vol", off, layout.SwitchCodeMax))
		}
	}
	dev.Write64(base+swLen, uint64(off))
	dev.Write64(base+swHash, sum)
	dev.Write64(base+swEntry, uint64(dst[0]-base))
	dev.Write64(base+swTramp, uint64(dst[1]-base))
	dev.Write64(base+swSMP, uint64(dst[2]-base))
	dev.Write64(base+0, switchMagic) // magic als laatste: half is nooit geldig

	// Zelfde fetch-contract als de thunks: ongecached geschreven, maar een
	// cacheable instructie-fetch moet ze vers uit DRAM halen.
	dev.CleanInv(base, off)
	dev.MB()

	el2.SetRelocated(uint64(dst[0]), uint64(dst[1]), uint64(dst[2]))
}

// adoptingNow meldt of InitVectors zijn verse-boot-schrijfwerk moet overslaan.
func adoptingNow() bool { return adopting }

// Adopting meldt of deze boot nog steeds als adoptie geldt: SetAdopting zette
// hem, en installSwitchCode kan hem hebben ingetrokken als de zittende
// switch-code niet de onze bleek. De slot-laag leest dit vóór hij bewoners
// probeert over te nemen.
func Adopting() bool { return adopting }
