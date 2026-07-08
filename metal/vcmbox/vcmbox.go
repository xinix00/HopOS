// Package vcmbox is de minimale client voor het VideoCore-mailbox-
// property-interface (bcm2835-mbox, kanaal 8) — het kanaal waarover de
// Pi-firmware framebuffers uitdeelt, de geheugensplitsing rapporteert en
// (P2b) klok/temperatuur regelt. Registerblok en protocol zijn op de hele
// Pi-familie gelijk; alleen de base verschilt (Pi 4: 0xFE00B880, Pi 5:
// 0x10_7C01_3880 — uit bcm2712-rpi-5-b.dtb: soc@107c000000/mailbox@7c013880,
// ranges 0x0 → 0x10_00000000).
//
// Het property-buffer leeft op een vast fysiek scratch-adres onder de
// RAM-declaratie (device-gemapt → ongecachet voor ons, gewone DRAM voor de
// VC — geen cache-maintenance nodig) en moet < 4GB en 16-byte-gealigneerd
// zijn: het mailbox-register is 32 bits met het kanaal in de lage 4.
// Linux' firmware-driver geeft op de hele familie het kale fysieke adres
// door (geen 0xC0000000-alias) — wij dus ook.
//
// Alle waits zijn begrensd op ITERATIETELLING, bewust niet op de klok: dit
// pakket moet ook werken als de generic timer dood is (CNTFRQ=0 of een
// getrapte counter-read — boot-meting Pi 5 2026-07-08: main hing in z'n
// eerste time.Sleep). Een dode mailbox levert ok=false, geen hang — dit is
// blind-debug-gereedschap, het mag zelf nooit het levensteken worden dat
// uitblijft.
//
// Alleen voor GOOS=tamago GOARCH=arm64.
package vcmbox

import "hop-os/metal/dev"

// bcm2835-mbox registeroffsets: MAIL0 = VC→ARM (lezen), MAIL1 = ARM→VC
// (schrijven); status-bit 31 = vol, bit 30 = leeg.
const (
	mail0RD     = 0x00
	mail0Status = 0x18
	mail1WR     = 0x20
	mail1Status = 0x38

	statusFull  = 1 << 31
	statusEmpty = 1 << 30

	chProperty = 8 // property-tags (ARM → VC)

	respSuccess = 0x80000000 // buffer[1] na een geslaagde call

	// maxPolls: elke status-read is een echte MMIO-round-trip (~100ns+),
	// dus 5M reads ≈ ruim een halve seconde — zat voor een firmware-call,
	// klokvrij begrensd.
	maxPolls = 5_000_000
)

// Chan is één mailbox met zijn scratch-buffer.
type Chan struct {
	Base uintptr // registerblok (bcm2835-mbox)
	Buf  uintptr // property-buffer: fysiek, < 4GB, 16-byte-gealigneerd
}

// Call voert één property-call uit. tags is de tag-sectie (id, value-size,
// req-code, value-woorden, ...); Call zet er de header en de end-tag omheen.
// Het antwoord is de complete buffer zoals de VC hem terugschreef (zelfde
// indeling, values in-place ingevuld), of ok=false bij timeout/foutstatus.
func (c *Chan) Call(tags []uint32) (resp []uint32, ok bool) {
	total := len(tags) + 3 // header (2) + tags + end-tag (1)
	dev.Write32(c.Buf, uint32(total*4))
	dev.Write32(c.Buf+4, 0) // request
	for i, w := range tags {
		dev.Write32(c.Buf+8+uintptr(i)*4, w)
	}
	dev.Write32(c.Buf+uintptr(total-1)*4, 0) // end-tag
	dev.MB()

	// Schrijfslot vrij? Dan het buffer-adres + kanaal 8 posten.
	polls := 0
	for dev.Read32(c.Base+mail1Status)&statusFull != 0 {
		if polls++; polls > maxPolls {
			return nil, false
		}
	}
	dev.Write32(c.Base+mail1WR, uint32(c.Buf)|chProperty)

	// Antwoord poppen tot het óns bericht is (adres + kanaal matchen);
	// vreemde/verweesde berichten worden weggelezen.
	for {
		for dev.Read32(c.Base+mail0Status)&statusEmpty != 0 {
			if polls++; polls > maxPolls {
				return nil, false
			}
		}
		if m := dev.Read32(c.Base + mail0RD); m == uint32(c.Buf)|chProperty {
			break
		}
	}
	dev.MB()

	if dev.Read32(c.Buf+4) != respSuccess {
		return nil, false
	}
	resp = make([]uint32, total)
	for i := range resp {
		resp[i] = dev.Read32(c.Buf + uintptr(i)*4)
	}
	return resp, true
}

// FB is een door de firmware toegewezen framebuffer.
type FB struct {
	Base          uintptr
	Size          uint32
	Width, Height int
	Pitch         int
}

// FBInit vraagt in één batched call een 32bpp-framebuffer van w×h aan
// (fysiek = virtueel, offset 0, RGB) en geeft de toewijzing terug.
func (c *Chan) FBInit(w, h int) (FB, bool) {
	resp, ok := c.Call([]uint32{
		0x00048003, 8, 0, uint32(w), uint32(h), // set physical w/h
		0x00048004, 8, 0, uint32(w), uint32(h), // set virtual w/h
		0x00048009, 8, 0, 0, 0, // set virtual offset
		0x00048005, 4, 0, 32, // set depth (32bpp)
		0x00048006, 4, 0, 1, // set pixel order (RGB)
		0x00040001, 8, 0, 4096, 0, // allocate (align, → base+size)
		0x00040008, 4, 0, 0, // get pitch
	})
	if !ok {
		return FB{}, false
	}
	// Tag-secties terugvinden: vaste offsets in dit vaste request (woorden
	// vanaf 2): fysiek=2, virtueel=7, offset=12, depth=17, order=21,
	// allocate=25, pitch=30 — value-woorden op +3.
	fb := FB{
		Width:  int(resp[2+3]),
		Height: int(resp[2+4]),
		// De VC meldt het FB-adres als busadres; op oudere firmware staat
		// daar de 0xC0000000-alias omheen — maskeren is op de hele familie
		// veilig (het FB leeft altijd < 4GB, het veld is 32 bits).
		Base:  uintptr(resp[25+3] &^ 0xC0000000),
		Size:  resp[25+4],
		Pitch: int(resp[30+3]),
	}
	if fb.Base == 0 || fb.Pitch == 0 || fb.Width == 0 || fb.Height == 0 {
		return FB{}, false
	}
	return fb, true
}

// FirmwareRev geeft de firmware-revisie (tag 0x1).
func (c *Chan) FirmwareRev() (uint32, bool) {
	resp, ok := c.Call([]uint32{0x00000001, 4, 0, 0})
	if !ok {
		return 0, false
	}
	return resp[5], true
}

// BoardRev geeft de board-revisie (tag 0x10002).
func (c *Chan) BoardRev() (uint32, bool) {
	resp, ok := c.Call([]uint32{0x00010002, 4, 0, 0})
	if !ok {
		return 0, false
	}
	return resp[5], true
}

// MemSplit geeft de ARM- en VC-geheugenvensters (tags 0x10005/0x10006) —
// dé bron voor de VC-carveout-grens die het P1-slot-plan moet kennen.
func (c *Chan) MemSplit() (armBase, armSize, vcBase, vcSize uint32, ok bool) {
	resp, ok := c.Call([]uint32{
		0x00010005, 8, 0, 0, 0,
		0x00010006, 8, 0, 0, 0,
	})
	if !ok {
		return 0, 0, 0, 0, false
	}
	return resp[5], resp[6], resp[10], resp[11], true
}
