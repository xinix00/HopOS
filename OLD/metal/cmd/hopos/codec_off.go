//go:build !media

package main

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/kern/hopfs"
)

// Buiten de media-smaak blijft het codec-blok uit: geen VPU-driver, geen
// firmware-lader, geen meetinstrument in de kern. De codec-ops van de ABI
// bestaan wel (kern/slots), en die antwoorden hier gewoon ErrUnsupported —
// een app ziet dus hetzelfde als op een board zonder codec-ijzer.
func codecUp(*hopfs.FS) {}

func codecDemo(string) { fmt.Println("codecdemo: this kernel is not the media flavour") }
