//go:build rpi4 && gui

// usb_rpi4.go — de USB-invoerketen van de Pi 4: de VL805 achter de
// BCM2711-root-complex.
//
// Dit is het duurste van de drie boards, want op de Pi 4 IS alle USB die
// PCIe-kaart. De Pi 5 heeft zijn xHCI in de RP1 waarvan de link al staat voor
// de GEM, en de Radxa heeft hem gewoon op de SoC-bus; hier moet er eerst een
// root-complex opgebracht worden die voor niets anders bestaat.
//
// Waarom dan toch: het bord wordt nog verkocht en ligt bij iedere tinkerer in
// de la (Derek, 06-08). En de regels zijn goedkoop — ze linken alleen mee in de
// gui-smaak van dít board.
package main

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/board/rpi4"
	"github.com/xinix00/HopOS/metal/driver/brcmpcie"
	"github.com/xinix00/HopOS/metal/gui/usbin"
)

func init() {
	usbin.Register(usbin.Host{
		Name: "vl805",
		Base: rpi4.VL805Base,
		// BusOff 0: de dma-ranges van dit bord leggen PCIe-adres 0 op DRAM 0,
		// dus wat de VL805 een adres noemt is het fysieke adres. Dat is
		// precies waarom het inbound-window hieronder identiek is — een
		// verschoven window zou hier een term vragen die niemand kan meten.
		Prepare: bringUpVL805,
	})
}

func bringUpVL805() (uintptr, error) {
	rc := &brcmpcie.RC{
		SoC:  brcmpcie.BCM2711,
		Base: rpi4.PCIeBase,
		Gen:  2, // de VL805 is een gen2 x1-endpoint
		Out: brcmpcie.OutWin{
			CPU: rpi4.PCIeCPUWin, PCIe: rpi4.PCIeBusWin, Size: rpi4.PCIeWinSize,
		},
		// Inbound: op de BCM2711 is RC_BAR2 hét DRAM-venster en horen BAR1 en
		// BAR3 uit te staan (pcie-brcmstb.c doet dat expliciet). De lege eerste
		// entry schrijft dus size-encoding 0 in BAR1 = uit, en de tweede is het
		// echte venster.
		//
		// 4GB en niet het volledige DRAM van een 8GB-Pi: de enige DMA die hier
		// gebeurt is die van de xHCI naar de USB-regio van het plan, en die
		// ligt op 0x14800000. Een groter venster zou geheugen openzetten waar
		// niets voor deze controller staat.
		In: []brcmpcie.InWin{
			{},
			{PCIe: 0, CPU: 0, Size: 0x1_0000_0000},
		},
	}
	if err := rc.BringUp(brcmpcie.BringConfig{
		WantID: rpi4.VL805ID,
		Bars: []brcmpcie.EPBar{
			{Off: 0x10, Val: rpi4.PCIeBusWin}, // BAR0 laag: de xHCI-registers
			{Off: 0x14, Val: 0},               // BAR0 hoog: 64-bit BAR, bovenhelft nul
		},
	}); err != nil {
		return 0, fmt.Errorf("bcm2711 pcie: %w", err)
	}
	fmt.Printf("usb: vl805 on PCIe, xHCI window at %#x (bus %#x)\n", rpi4.VL805Base, rpi4.PCIeBusWin)
	return rpi4.VL805Base, nil
}
