// Package rkscan is de beeldketen van de RK3566 (Radxa Zero 3E): power-domein
// (pd.go) → VOP2-scanout (vop2.go) → DW-HDMI-transmitter (hdmi.go). Eén
// package, want de drie lagen zijn als één geheel geschreven en delen de
// 1080p60-modus, de foutvorm (pmuError) en de hiword-helper.
//
// WAAROM DIT IN gui/driver/ WOONT en niet in board/rk3566: dit is de eerste
// echte display-driver in de boom (elders is beeld een firmware-buffer, zie
// board/raspi/vcfb), en "geen dood gewicht" betekent dat een headless node hem
// niet meedraagt. De grens is de import: alleen cmd's gui-smaak
// (cmd/hopos/gui_rk3566.go, via rk3566hop.UseScanout — board mag gui niet
// importeren) en het meetinstrument (cmd/proberk3566) linken dit pakket — een
// kale build heeft er geen regel van aan boord. Het board houdt wat bij het
// booten en het net hoort (pinmux, GRF, CRU-GMAC, PSCI); wat alleen voor het
// glas bestaat, staat hier — en elke volgende SoC met beeld krijgt zijn eigen
// map naast deze.
//
// De naad met het board is smal en loopt één kant op: rkscan kent zijn eigen
// registeradressen (dit silicium verandert niet per bord), en de framebuffer —
// wél board-kennis, hij ligt in het PA-plan — komt als argument binnen
// (VOPScanout krijgt rk3566.FB()).
package rkscan

const (
	// De CRU. Zelfde blok als board/rk3566 voor het GMAC gebruikt; het adres
	// staat hier nogmaals zodat dit pakket geen board hoeft te importeren —
	// het is een siliciumconstante, geen bordkeuze.
	cruBase = 0xFDD20000 // rk356x-base.dtsi: clock-controller@fdd20000
)

// hiword bouwt een schrijfactie voor een hiword-masked veld: waarde in de
// onderste 16 bits, maskerbits erboven. Elke CRU-, PMU- en GRF-schrijfactie op
// dit silicium heeft deze vorm (zelfde helper als in board/rk3566/cru.go).
func hiword(val, mask, shift uint32) uint32 {
	return val<<shift | mask<<(shift+16)
}
