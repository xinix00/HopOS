// apple.go — het platte image verpakken als Apple-bootobject.
//
// Op elk ander bord laadt de bootloader ons op een afgesproken adres. iBoot
// niet: die legt een raw bootobject neer waar het hem uitkomt (gemeten 29-08 op
// de M4-mini: m1n1 stond de ene boot op 0x100_2bb8_000, de andere op
// 0x100_3a90_000) en springt naar bestandsoffset 0x800. Ons image is op een
// vast adres gelinkt, dus er moet iets vooraan dat de rest verplaatst.
//
// Dat "iets" is niet hier geschreven maar in board/apple/bootstub.s, en dit
// bestand zet het alleen op zijn plaats. Reden: de stub draait op silicium en
// hoort dus bij het board, en de Go-assembler codeert hem — met de hand
// instructiewoorden verzinnen in host-gereedschap is precies het soort werk dat
// pas op de derde bootcyclus fout blijkt.
//
// De indeling van de eerste bladzijde (pariteit: board/apple/bootstub.go):
//
//	0x0000  stubReset   — RVBAR van de secundaire cores wijst hierheen
//	0x0100  parameters  — magic, doel, grootte, entry
//	0x0800  stubEntry   — waar de firmware de boot-core aflevert
//	0xE000  scratch     — vanaf hier is het board de baas (apple.BootScratch)
//	0x10000 de kern
package main

import (
	"encoding/binary"
	"fmt"
	"os"
)

// De offsets uit board/apple/bootstub.go.
const (
	appleResetOff  = 0x000
	appleParamsOff = 0x100
	appleEntryOff  = 0x800
	appleScratch   = 0xE000 // vanaf hier: apple.BootScratch, niet van ons
)

// appleStubMagic staat in het parameterblok zodat een hexdump van het image
// meteen laat zien dat de stub erin zit. De stub zelf controleert hem niet: hij
// heeft geen register over voor een 64-bit constante zonder literal pool, en
// een doel of entry van nul vangt dezelfde fout af.
const appleStubMagic = "HOPSTUB1"

// writeAppleBoot schrijft het bootobject: het platte image met de twee stubs
// en hun parameters vooraan.
func writeAppleBoot(p *payload, out string) {
	// De grootte die de stub kopieert, op 64 afgerond: zijn kopieerlus doet 16
	// bytes per slag en heeft dan geen staartgeval nodig.
	size := (uint64(len(p.img)) + 63) &^ 63

	// Het BESTAND daarentegen loopt door tot een veelvoud van 16KB — de
	// paginamaat van dit silicium. iBoot mapt een raw bootobject zelf (kmutil
	// --raw --lowest-virtual-address 0), en een lengte die geen heel aantal
	// pagina's is, is precies het soort ding waar zo'n mapper op assert.
	// Gemeten 30-08: de eerste installatie van probeapple.img (2.426.048 bytes
	// = 1216 over een paginagrens) gaf een iBoot Panic in stage 2 vóór onze
	// eerste instructie — géén regel van ons kwam langs. m1n1.bin, het enige
	// bootobject waarvan we weten dát het werkt, is exact 68 pagina's groot,
	// en zijn linkerscript lijnt élke sectie op 0x4000. Dus doen wij dat ook.
	// De stub kopieert nog steeds alleen `size`: de staart is nul en hoort bij
	// het bestand, niet bij het image.
	file := (size + 0x3FFF) &^ 0x3FFF
	img := make([]byte, file)
	copy(img, p.img)

	reset := p.stub(img, "stubReset", appleResetOff, appleParamsOff-appleResetOff)
	entry := p.stub(img, "stubEntry", appleEntryOff, appleScratch-appleEntryOff)

	binary.LittleEndian.PutUint64(img[appleParamsOff+0x00:], binary.LittleEndian.Uint64([]byte(appleStubMagic)))
	binary.LittleEndian.PutUint64(img[appleParamsOff+0x08:], p.load)
	binary.LittleEndian.PutUint64(img[appleParamsOff+0x10:], size)
	binary.LittleEndian.PutUint64(img[appleParamsOff+0x18:], p.f.Entry)

	if err := os.WriteFile(out, img, 0o644); err != nil {
		die("%v", err)
	}
	fmt.Printf("%s: %d bytes (apple boot object; %d pages, image %d, stubs %d+%d bytes, target %#x, entry %#x)\n",
		out, len(img), file/0x4000, size, reset, entry, p.load, p.f.Entry)
}

// stub kopieert een assembly-symbool naar zijn offset in het image en geeft
// zijn lengte. De Go-linker geeft een assembly-functie de ABI0-naam; zonder de
// verwijzing in board/apple/bootstub.go zou hij hem bovendien helemaal
// weggooien, en dan struikelt dit.
func (p *payload) stub(img []byte, name string, at, room uint64) uint64 {
	const pkg = "github.com/xinix00/HopOS/metal/board/apple."
	off, size := p.symbol(pkg + name + ".abi0")
	if size == 0 || size > room {
		die("%s: %d bytes past niet op image-offset %#x (ruimte %d)", name, size, at, room)
	}
	if off+size > uint64(len(p.img)) {
		die("%s: ligt buiten het image (offset %#x, %d bytes)", name, off, size)
	}
	copy(img[at:at+size], p.img[off:off+size])
	return size
}
