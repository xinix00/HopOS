//go:build !rpi5 && !rpi4 && !uefi && !licheerv

// board_virt.go — de QEMU-virt-kant van de agent-main: board-registratie en
// de RAM-declaratie van de HOP-kern (het enige dat per board verschilt; de
// main zelf is boardvrij).
package main

import (
	_ "unsafe"

	"hop-os/metal/abi/layout"
	_ "hop-os/metal/board/qemuvirt/hop" // registreert het board (init) + basis-hooks
	"hop-os/metal/fw/bootcfg"
)

// QEMU-virt heeft geen bootmedium en dus geen platform-config: bootParamAll
// kent alleen wat de werkbank zelf declareert. Dit board ís de werkbank
// (demo, regressie, hostfwd-poorten op de Mac), dus: de bewuste opt-out op de
// auth-poort — zonder die zou de agent op QEMU weigeren te starten (terecht:
// op écht ijzer hoort er een hopos.apikey te staan), en dat is precies het
// onderscheid dat we willen: de bank mag open, een board met een bootmedium
// niet — plus de bij het bouwen meegegeven extraCfg hieronder.
// extraCfg is werkbank-config die de Mac bij het BOUWEN meegeeft, in
// kernel-cmdline-formaat (whitespace-gescheiden, dus waarden zonder
// spaties): image/qemu-run.sh agent geeft $HOPCFG door via
// -ldflags "-X 'main.extraCfg=…'". QEMU-virt heeft geen bootmedium; dit is
// de regressie-knop voor config-gedreven paden (zoals de object-store)
// zonder een nep-stick te verzinnen. Leeg = precies het oude gedrag.
var extraCfg string

func init() {
	bootParamAll = func(key string) []string {
		if key == "hopos.insecure" {
			return []string{"1"}
		}
		return bootcfg.Cmdline(extraCfg, key)
	}
}

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = layout.HopRAMStart

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = layout.HopRAMSize
