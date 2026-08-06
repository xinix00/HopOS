//go:build rpi5 && gui

// usb_rpi5.go — de USB-invoerketen van de Pi 5: de twee xHCI's in de RP1.
// Goedkoopste van de drie boards, en dat is geen toeval: de PCIe-link naar de
// RP1 staat al voor de GEM (board/rpi5/hop ProbeNIC → brcmpcie), dus hier
// blijft alleen het offset binnen het RP1-venster over.
package main

import (
	"github.com/xinix00/HopOS/metal/board/rpi5"
	"github.com/xinix00/HopOS/metal/gui/usbin"
)

func init() {
	// BusOff: de RP1 is een PCIe-master. Wat hij een adres noemt gaat door het
	// inbound-window van de BCM2712-root-complex, en dat window legt PCIe
	// 0x10_0000_0000 op DRAM 0 — dezelfde waarde die de GEM hier al gebruikt.
	// Zonder deze term zou de controller zijn ringen 64GB naast het DRAM
	// zoeken; dat geeft geen foutmelding, alleen stilte.
	const busOff = 0x10_0000_0000
	usbin.Register(usbin.Host{Name: "rp1-usb0", Base: rpi5.RP1USB0Base, BusOff: busOff})
	usbin.Register(usbin.Host{Name: "rp1-usb1", Base: rpi5.RP1USB1Base, BusOff: busOff})
}
