package main

// nvmeBench meet de NVMe-driver op HOP zelf, los van hopfs, servicer en
// transport: rauwe Read/Write-opdrachten van 4KiB tot 1MiB op de staart van
// ons eigen venster. Alleen met hopos.nvmebench=1, want het schrijft daar —
// hopfs is vluchtig en alloceert van voren, dus de staart is van niemand, maar
// een meetbank die ongevraagd op de schijf schrijft is geen bench maar een bug.

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/driver/nvme"
)

func nvmeBench(disk *nvme.Controller, first, count uint64) {
	bs := disk.BlockSize
	sizes := []uint64{4 << 10, 64 << 10, 256 << 10, 1 << 20}
	const span = 64 << 20 // 64MB aan de staart van het venster
	if count*bs < 2*span {
		fmt.Println("nvme bench: window too small, skipped")
		return
	}
	base := first + count - span/bs
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i * 7)
	}
	for _, sz := range sizes {
		if sz > disk.MaxTransfer {
			continue
		}
		n := uint64(16<<20) / sz // 16MB per maat
		lbaStep := sz / bs
		// Schrijven, dan lezen: dezelfde LBA's, dus lezen leest wat er staat.
		t0 := time.Now()
		for k := uint64(0); k < n; k++ {
			if err := disk.Write(base+k*lbaStep, buf[:sz]); err != nil {
				fmt.Printf("nvme bench: write %d: %v\n", sz, err)
				return
			}
		}
		w := time.Since(t0)
		t1 := time.Now()
		for k := uint64(0); k < n; k++ {
			if err := disk.Read(base+k*lbaStep, buf[:sz]); err != nil {
				fmt.Printf("nvme bench: read %d: %v\n", sz, err)
				return
			}
		}
		r := time.Since(t1)
		fmt.Printf("nvme bench: %4d KiB x%-4d write %6.1f MB/s (%5.0f us/cmd), read %6.1f MB/s (%5.0f us/cmd) HOPOS_NVMEBENCH\n",
			sz>>10, n, float64(n*sz)/w.Seconds()/1e6, float64(w.Microseconds())/float64(n),
			float64(n*sz)/r.Seconds()/1e6, float64(r.Microseconds())/float64(n))
	}
}
