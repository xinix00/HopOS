//go:build o6n && media

// media_o6n.go — de media-smaak van de O6N: knoopt de Linlon V8 aan de
// codec-dienst. Het board weet hoe het blok stroom, klok en reset krijgt
// (board/o6n/hop/vpu.go), de driver weet hoe je ermee praat
// (media/driver/vpu/mve); dit bestand staat in cmd omdat board media niet mag
// importeren — dezelfde naad als gui_rk3566.go voor de beeldketen.
package main

import (
	"fmt"

	o6n "github.com/xinix00/HopOS/metal/v2/board/o6n/hop"
	"github.com/xinix00/HopOS/metal/v2/driver/codec"
	"github.com/xinix00/HopOS/metal/v2/media/driver/vpu/mve"
)

func init() {
	codecProbe = func(arena uintptr, size uint64, fw codec.FirmwareSource) (codec.Engine, error) {
		base, rcsu, err := o6n.PowerVPU(arena, size)
		if err != nil {
			return nil, err
		}
		d, err := mve.Probe(base, rcsu, mve.NewArena(arena, size), fw)
		if err != nil {
			return nil, err
		}
		fmt.Printf("vpu: %s, arena %d MB, IRQ %d\n", d.Describe(), size>>20, o6n.VPUIRQ)
		return d, nil
	}
}
