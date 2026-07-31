//go:build !rpi5 && !rpi4 && !uefi && !licheerv

// board_virt.go — de QEMU-virt-kant van de agent-main: board-registratie en
// de RAM-declaratie van de HOP-kern (het enige dat per board verschilt; de
// main zelf is boardvrij).
package main

import (
	_ "unsafe"

	"hop-os/metal/abi/layout"
	_ "hop-os/metal/board/qemuvirt/hop" // registreert het board (init) + basis-hooks
)

// QEMU-virt heeft geen bootmedium en dus geen platform-config: bootParamAll
// blijft de nil-variant uit main.go. Dit board ís de werkbank (demo, regressie,
// hostfwd-poorten op de Mac), dus het declareert hier de enige sleutel die het
// nodig heeft — de bewuste opt-out op de auth-poort. Zonder dit zou de agent op
// QEMU weigeren te starten (terecht: op écht ijzer hoort er een hopos.apikey te
// staan), en dat is precies het onderscheid dat we willen: de bank mag open,
// een board met een bootmedium niet.
func init() {
	bootParamAll = func(key string) []string {
		if key == "hopos.insecure" {
			return []string{"1"}
		}
		return nil
	}
}

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = layout.HopRAMStart

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = layout.HopRAMSize
