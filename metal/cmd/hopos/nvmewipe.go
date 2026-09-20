package main

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/driver/nvme"
)

// nvmeWipe nult het begin en het eind van ons LBA-venster: de primaire GPT
// (LBA 0..33) én de backup-GPT (de laatste 33 LBA's, waar EDK2 op terugvalt
// als de primaire weg is), elk ruim genomen als 1 MiB. Daarmee is een oud OS
// op de schijf onvindbaar voor de firmware: precies wat een UEFI-doos nodig
// heeft die HopOS-only wordt (O6N 17-09: de firmware bootte eerst de Linux
// die er nog op stond). Alleen met hopos.nvmewipe=1, en de regel hoort na
// één boot weer uit de config: dit is een sloophamer, geen instelling.
// hopfs zelf is vluchtig en alloceert van voren, dus voor HopOS is de schijf
// daarna gewoon leeg zoals hij dat al was.
// DefaultNVMeWipe: bouwtijd-variant van hopos.nvmewipe=1 (-ldflags -X
// main.DefaultNVMeWipe=1) voor een flip-kern — de stick-config is na een
// flip niet meer te wijzigen zonder flash (O6N 17-09).
var DefaultNVMeWipe = "0"

func nvmeWipe(disk *nvme.Controller, first, count uint64) {
	const span = 1 << 20
	blocks := uint64(span) / disk.BlockSize
	if count < 2*blocks {
		fmt.Println("storage: nvmewipe: window too small to wipe both ends - skipped")
		return
	}
	zero := make([]byte, 64<<10)
	step := uint64(len(zero)) / disk.BlockSize
	wipe := func(lba uint64) error {
		for k := uint64(0); k < blocks; k += step {
			if err := disk.Write(lba+k, zero); err != nil {
				return err
			}
		}
		return nil
	}
	for _, lba := range [...]uint64{first, first + count - blocks} {
		if err := wipe(lba); err != nil {
			fmt.Printf("storage: nvmewipe: write at LBA %d failed: %v\n", lba, err)
			return
		}
	}
	fmt.Printf("storage: nvmewipe: zeroed 1 MiB at LBA %d and at LBA %d (both partition tables gone) - remove hopos.nvmewipe from the config\n",
		first, first+count-blocks)
}
