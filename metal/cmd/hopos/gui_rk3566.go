//go:build rk3566 && gui

// gui_rk3566.go — de gui-smaak van de Radxa-agent: bedraadt de beeldketen van
// gui/driver/rkscan (PD_VO → VOP2 → HDMI-TX) in het board. Dit bestand staat in cmd
// en niet in board/rk3566/hop omdat de importrichting dat afdwingt
// (indeling.md: board mag gui niet importeren, alleen cmd importeert gui
// terug) — cmd is het knooppunt, net als gui.go voor de fbgrant. Kaal gebouwd
// registreert niemand een scanout en zegt Framebuffer() false: geen regel
// display-code aan boord.
package main

import (
	"github.com/xinix00/HopOS/metal/v2/board/rk3566"
	rk3566hop "github.com/xinix00/HopOS/metal/v2/board/rk3566/hop"
	"github.com/xinix00/HopOS/metal/v2/gui/driver/rkscan"
)

func init() {
	// De framebuffer-geometrie is board-kennis (het PA-plan), de keten is
	// driver-kennis — deze regel is precies de naad tussen die twee.
	rk3566hop.UseScanout(func() { rkscan.Start(rk3566.FB()) })
}
