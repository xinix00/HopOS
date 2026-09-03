//go:build tamago && arm64

// firmware.go — wat de firmware ons vertelt, bij de bron.
//
// Er is precies één ingang: x0. Daar staat iBoot's boot_args-blok in, dat het
// RAM-contract draagt en zegt waar de device tree ligt; die boom draagt de
// rest. De bootstub vangt x0 op en legt hem op de scratch (bootstub.s), dus
// deze twee functies werken of we nu zelf het bootobject zijn of dat een
// loader ons neerzette.
//
// Dat was niet altijd zo. Tot 29-08 las een Python-loader op de LAPTOP de boom
// en legde de gevonden adressen in een param-blok; het board las dát, met de
// ADT als terugval. Twee bronnen voor hetzelfde feit, en per accessor een eigen
// voorkeur — precies het soort constructie waar je later een middag aan kwijt
// bent omdat er twee antwoorden zijn. Nu is de boom de bron en draagt het
// param-blok alleen nog wat de loader áls enige weet: m1n1's spin-table en de
// hop (params.go).
package apple

import (
	"github.com/xinix00/HopOS/metal/v2/driver/fb"
	"github.com/xinix00/HopOS/metal/v2/fw/xnuboot"
)

var (
	bootArgs xnuboot.Args
	bootOK   bool
	bootRead bool
)

// Boot geeft iBoot's boot_args: het RAM dat van ons is, tot waar de firmware
// het zelf gevuld heeft, het framebuffer en het adres van de device tree.
//
// Hier zit één kip-en-ei in, en die is de reden dat dit een functie is en geen
// veld. Op dit silicium faultt een 1GB-blokdescriptor met een uitvoeradres
// boven 2^40 (mmu.go), dus het lage DRAM is onbereikbaar tot MapDRAM gedraaid
// heeft — en MapDRAM wil weten waar het DRAM ligt, wat in boot_args staat, wat
// ín dat lage DRAM ligt. Doorbroken door eerst alleen de GB te mappen waar x0
// zelf in valt. (GEMETEN 29-08: zonder die twee regels stierf de eerste boot
// zonder loader hier, met FAR = precies het boot_args-adres.)
func Boot() (xnuboot.Args, bool) {
	if bootRead {
		return bootArgs, bootOK
	}
	bootRead = true
	x0 := FirmwareX0()
	if x0 == 0 {
		return bootArgs, false
	}
	const gb = 1 << 30
	MapDRAM(x0&^(gb-1), gb)
	if bootArgs, bootOK = xnuboot.Read(uintptr(x0)); bootOK {
		MapDRAM(bootArgs.PhysBase, bootArgs.MemSize)
	}
	return bootArgs, bootOK
}

// MemTotal geeft het fysiek aanwezige DRAM in bytes (0 = onbekend).
func MemTotal() uint64 {
	ba, ok := Boot()
	if !ok {
		return 0
	}
	return ba.MemSizeActual
}

// FB geeft de framebuffer die de firmware achterliet (boot_args video): adres,
// maat, stride en bits per pixel. ok=false als er geen boot_args zijn of als de
// firmware geen buffer meldt.
//
// Alleen doorgeven wat er staat — geen aannames. Een diepte die we niet kennen
// (alles behalve 32 en 16) melden we als "geen buffer": liever niets tekenen
// dan ruis op een scherm dat iemand nodig heeft.
func FB() (fb.Desc, bool) {
	b, ok := Boot()
	if !ok || b.FB.Base == 0 || b.FB.W == 0 || b.FB.H == 0 || b.FB.Stride == 0 {
		return fb.Desc{}, false
	}
	// De diepte draagt VLAGGEN in de hoge bits; alleen de onderste byte is het
	// aantal bits per pixel. Gemeten 30-08 op de M4: 0x10020 — dat is 32 bpp
	// met bit 16 gezet, en een gelijkheidstest op 32 zag daar "geen
	// framebuffer" in terwijl hij er gewoon stond.
	bpp := int(b.FB.Depth & 0xFF)
	if bpp != 32 && bpp != 16 {
		return fb.Desc{}, false
	}
	return fb.Desc{
		Base:   uintptr(b.FB.Base),
		Width:  int(b.FB.W),
		Height: int(b.FB.H),
		Stride: int(b.FB.Stride),
		BPP:    bpp,
	}, true
}
