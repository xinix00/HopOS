package raspi

// De log-console (metal/fb) op de Pi gebruikt de firmware-simple-framebuffer:
// de EEPROM-bootloader zet HDMI al aan en beschrijft de buffer in /chosen van
// de DTB (de universele simplefb-binding, wat Linux' early console ook leest).
// Discovery hier, één keer voor Pi 4 en Pi 5; het renderen zit in metal/fb.
// Géén VideoCore-mailbox-pad meer — dat was Pi-specifieke bring-up, geen
// universeel mechanisme.

import (
	"hop-os/metal/dev"
	"hop-os/metal/fb"
	"hop-os/metal/fdt"
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
