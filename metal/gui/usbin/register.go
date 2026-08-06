//go:build gui

package usbin

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/gui/driver/usb/xhci"
	"github.com/xinix00/HopOS/metal/gui/fbgrant"
)

// Dit bestand is de naad tussen board en dienst. De boards zélf importeren
// gui niet (indeling.md regel 4: alleen cmd importeert gui terug), dus de
// bedrading gebeurt in cmd/hopos/usb_<board>.go — die roept Register aan met
// wat op dít bordje een xHCI is, en main roept daarna Start.

// Host is één controller zoals een board hem aanbiedt.
type Host struct {
	// Name komt in élke logregel van deze controller. Een node met drie
	// controllers moet leesbaar zijn.
	Name string

	// Base is het xHCI-capabilityvenster. Vast op een SoC, een BAR achter PCIe.
	// Nul is toegestaan als Prepare hem invult (de PCIe-gevallen weten hem pas
	// ná de enumeratie).
	Base uintptr

	// BusOff is wat de controller bij een CPU-fysiek adres optelt (zie
	// xhci.HC.BusOff).
	BusOff uint64

	// Prepare is de board-specifieke voorbereiding: PCIe-link trainen, een
	// DWC3-core in hostmodus zetten, een klok aan. Nil = niets te doen. Geeft
	// het (eventueel gecorrigeerde) basisadres terug.
	Prepare func() (uintptr, error)
}

var hosts []Host

// Register meldt een controller aan. Alleen uit init() of vóór Start.
func Register(h Host) { hosts = append(hosts, h) }

// Default is de invoerdienst van deze node — één per node, want er is één
// scherm om in te typen.
var Default *Manager

// Start brengt alle geregistreerde controllers op en begint te scannen. Geen
// harde eis: een node zonder werkende USB draait door, alleen typ je er niet
// op. Elke controller die het niet doet is één logregel — en die regel ís de
// meting, want dit pad is per bord anders bedraad.
//
// De luisterpost gaat pas open als er mínstens één controller draait, en pas
// dán komt INPUT_ADDR in de fb-grant. Een display die het veld niet ziet weet
// dus dat er niets te bellen valt, in plaats van in een reconnect-lus te gaan
// zitten voor een toetsenbord dat niet bestaat.
func Start() {
	if len(hosts) == 0 {
		return
	}
	if !layout.HasUSBDMA() {
		fmt.Println("usb: this board has no USB DMA region in its plan — input disabled")
		return
	}

	// De sink bestaat vóór de controllers, want een apparaat dat tijdens de
	// scan al aan hangt levert meteen rapporten. Zonder verbinding vallen die
	// weg — dat is de bedoeling.
	d, addr, err := listen()
	if err != nil {
		fmt.Printf("usb: input listener: %v — no physical input on this node\n", err)
		return
	}
	Default = New(d.Sink)

	// Elke controller krijgt zijn eigen stuk van de regio: ze draaien
	// tegelijk en delen niets.
	span := uintptr(layout.USBDMASize) / uintptr(len(hosts))
	live := 0
	for i, h := range hosts {
		base := h.Base
		if h.Prepare != nil {
			b, err := h.Prepare()
			if err != nil {
				fmt.Printf("usb: %s: %v\n", h.Name, err)
				continue
			}
			if b != 0 {
				base = b
			}
		}
		if base == 0 {
			fmt.Printf("usb: %s: no register window — skipped\n", h.Name)
			continue
		}
		hc := &xhci.HC{Base: base, Name: h.Name, BusOff: h.BusOff}
		if err := Default.Add(hc, layout.USBDMAPA()+uintptr(i)*span, span); err != nil {
			fmt.Printf("usb: %s: %v\n", h.Name, err)
			continue
		}
		live++
	}
	if live == 0 {
		fmt.Println("usb: no working controller on this node — input stays off")
		d.close()
		Default = nil
		return
	}
	fbgrant.UseInput(addr)
	fmt.Printf("usb: %d controller(s), input served on %s (goes out with the fb grant)\n", live, addr)
	go Default.Run()
}
