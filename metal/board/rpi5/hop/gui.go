//go:build gui

// gui.go — de display-kant van het rpi5-board, alleen in gui-builds
// (`-tags "rpi5 gui"`): het Display-contract van metal/gui/debug. De kale
// (headless) build heeft deze methoden — en daarmee het hele display-vlak —
// niet. FB-discovery is géén gui (de log-console) en blijft in de basis.
package hop

// HVS geeft de registerbasis van de BCM2712-HVS (bcm2712.dtsi:
// hvs@107c580000, "brcm,bcm2712-hvs") — voor de read-only P4-dumptool
// (metal/gui/hvs).
func (machine) HVS() (uintptr, bool) { return 0x10_7c58_0000, true }

// DisplayMMIO geeft het display-blok van de BCM2712 (pixelvalves/mop/
// moplet/disp_intr/HVS, bcm2712.dtsi) — de harde grens van het read-only
// /mmio-endpoint: buiten dit venster kan een verdwaalde read de bus laten
// hangen.
func (machine) DisplayMMIO() (lo, hi uintptr) { return 0x10_7c40_0000, 0x10_7c5a_0000 }
