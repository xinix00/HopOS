// Package vcfb is de gedeelde Pi-framebuffer-discovery voor de HOP-helften
// van rpi4/rpi5 (board/<x>/hop): eerst de universele simple-framebuffer uit
// de DTB (wat Linux' early console ook leest), en anders — GEMETEN 2026-07-11
// op beide boards: de Pi-firmware laat aan raw kernels géén simplefb-node na,
// ook niet met HDMI erin — het framebuffer zelf opeisen via de
// VideoCore-mailbox (vcmail.AllocFB, het officiële pad; nog steeds
// "firmware-buffer, geen driver"). Bewust búíten de raspi-basis: dit
// importeert vcmail/fb en is puur HOP-werk — een app-image (dat de basis wél
// linkt) heeft hier niets te zoeken. Het renderen zit in metal/driver/fb.
package vcfb

import (
	"sync"

	"hop-os/metal/board/raspi"
	"hop-os/metal/dev"
	"hop-os/metal/driver/fb"
	"hop-os/metal/driver/vcmail"
	"hop-os/metal/fw/fdt"
)

// Framebuffer leest de simple-framebuffer uit de DTB waarvan cpuinit de
// pointer op dtbPtr legde (het x0-adres bij boot); ok=false als de firmware
// er geen aanleverde. dtbPtr is een scratch-woord dat het DTB-adres bevat —
// eerst dereferencen, zoals board.MemTotal.
func Framebuffer(dtbPtr uintptr) (fb.Desc, bool) {
	f, ok := fdt.Framebuffer(uintptr(dev.Read64(dtbPtr)))
	if !ok {
		return fb.Desc{}, false
	}
	return fb.Desc{
		Base:   uintptr(f.Base),
		Width:  int(f.Width),
		Height: int(f.Height),
		Stride: int(f.Stride),
		BPP:    f.BPP,
	}, true
}

// FramebufferVC is de volledige Pi-discovery: eerst de DTB-simplefb, anders
// het framebuffer via de firmware-mailbox opeisen (mboxBase = het VCMail-
// basisadres van het board). Board-glue: rpi4/rpi5 geven alleen hun adressen.
//
// De respons telt, niet het verzoek — en bínnen de respons telt de PITCH:
// gemeten 2026-07-11 (Pi 5) meldt de depth-tag 32 terwijl de scanout op de
// 16bpp-bootsplash-config blijft draaien (stride 3840 = 1920×2). De pitch
// beschrijft wat de scanout écht leest, dus dáár leiden we de pixeldiepte
// uit af; metal/driver/fb rendert beide dieptes.
//
// ÉÉN KEER, daarna gecachet (19-07): elke AllocFB is een verse firmware-
// allocatie, en de scanout blijft aan de bóót-buffer hangen — een tweede
// alloc kan een ander (niet-gescand) adres teruggeven, en de gedeelde
// property-buffer racet bovendien met andere mailbox-verkeer (gemeten:
// een grant las "3x1500000000"). Beeld = de firmware-buffer van boot; de
// eerste geslaagde discovery is dus de enige. Rommel (onzinnige maten)
// wordt geweigerd én niet gecachet.
var (
	fbMu     sync.Mutex
	fbCached fb.Desc
	fbOK     bool
)

func FramebufferVC(dtbPtr, mboxBase uintptr) (fb.Desc, bool) {
	fbMu.Lock()
	defer fbMu.Unlock()
	if fbOK {
		return fbCached, true
	}
	d, ok := discoverVC(dtbPtr, mboxBase)
	if !ok || !sane(d) {
		return fb.Desc{}, false
	}
	fbCached, fbOK = d, true
	return d, true
}

// sane weert onzin-descriptors (mailbox-ruis) uit de cache en de grants.
func sane(d fb.Desc) bool {
	return d.Base != 0 && d.Width >= 64 && d.Width <= 8192 &&
		d.Height >= 64 && d.Height <= 8192 &&
		d.Stride >= d.Width*d.BPP/8 && (d.BPP == 16 || d.BPP == 32)
}

func discoverVC(dtbPtr, mboxBase uintptr) (fb.Desc, bool) {
	if d, ok := Framebuffer(dtbPtr); ok {
		return d, true
	}
	m := &vcmail.Mbox{Base: mboxBase, Buf: uintptr(raspi.VCMailBuf)}
	f, ok := m.AllocFB(1920, 1080)
	if !ok || f.Width == 0 {
		return fb.Desc{}, false
	}
	return fb.Desc{
		Base:  f.Base,
		Width: int(f.Width), Height: int(f.Height),
		Stride: int(f.Pitch), BPP: int(f.Pitch / f.Width * 8),
	}, true
}
