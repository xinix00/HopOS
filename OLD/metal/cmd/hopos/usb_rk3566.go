//go:build rk3566 && gui

// usb_rk3566.go — de USB-invoerketen van de Radxa: twee DWC3-cores, elk eerst
// in hostmodus en dan als xHCI. Zelfde plek en zelfde reden als gui_rk3566.go:
// board mag gui niet importeren, dus cmd is het knooppunt.
//
// De cores hangen aan hun eigen klokken en PHY's, en die zetten wij NIET aan.
// Dat is een keuze en geen gat: de CRU-gates en de USB2-PHY-GRF-bits van dit
// silicium zijn niet geverifieerd, en de TSADC van vorige week is precies wat
// er gebeurt als je Rockchip-klokregisters gokt. Wat U-Boot achterlaat is het
// startpunt; leest CAPLENGTH nul, dan zegt de probe "niet geklokt" en is dát
// de volgende meting.
package main

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/board/rk3566"
	"github.com/xinix00/HopOS/metal/v2/gui/driver/usb/dwc3"
	"github.com/xinix00/HopOS/metal/v2/gui/usbin"
)

func init() {
	for _, c := range []struct {
		name string
		base uintptr
	}{
		{"usbdrd30", rk3566.USBDRD30Base},
		{"usbhost30", rk3566.USBHost30Base},
	} {
		core := &dwc3.Core{Base: c.base, Name: c.name}
		usbin.Register(usbin.Host{
			Name: c.name,
			Base: c.base,
			// BusOff 0: de cores hangen rechtstreeks op de geheugenbus, dus
			// wat de CPU een adres noemt noemt de controller ook zo.
			Prepare: func() (uintptr, error) {
				err := core.HostMode()
				// De globale registers erbij, ook als het lukte. Op dit
				// silicium is de vraag níet "werkt de driver" maar "staat de
				// klok en de PHY aan", en dit is precies de regel die dat
				// beantwoordt — één boot i.p.v. drie.
				id, ctl, u2, u3, sts := core.Regs()
				fmt.Printf("usb: %s dwc3 id=%08x gctl=%08x usb2=%08x usb3=%08x gsts=%08x\n",
					c.name, id, ctl, u2, u3, sts)
				return core.Base, err
			},
		})
	}
}
