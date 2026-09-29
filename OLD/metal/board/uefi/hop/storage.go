package hop

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/board/uefi"
	"github.com/xinix00/HopOS/metal/v2/cpu/memattr"
	"github.com/xinix00/HopOS/metal/v2/driver/nvme"
	"github.com/xinix00/HopOS/metal/v2/driver/pcie"
)

// Disk is de opslag van een UEFI/ACPI-platform (het Disk()-contract van
// cmd/hopos: controller + het LBA-venster dat van ons is): de eerste NVMe-
// controller (klasse 01.08) in de firmware-geconfigureerde PCIe-hiërarchie —
// achter een root-port dus, waar nvme.Probe's vlakke bus-0-scan (QEMU, kale
// fabrics) hem nooit zag. BAR0 wees de firmware al toe (alleen lezen, net als
// bij de NIC); hoge BAR's gaan door MapHigh. DMA uit de carve (uefi.NVMeDMA).
//
// Het venster is de HÉLE schijf: op een UEFI-doos is de NVMe van HopOS alleen
// (Altra, O6N — anders dan de Mac mini, die macOS op dezelfde SSD heeft en
// daarom een GPT-gat kiest). Staat er nog een ander OS op, dan is dit precies
// het moment om de schijf eruit te halen: hopfs beschouwt hem als leeg.
func (Machine) Disk() (*nvme.Controller, uint64, uint64, error) {
	var nd *pcie.Device
	eachECAM(func(win pcie.Window, startBus int) bool {
		for _, d := range pcie.ScanConfigured(win, startBus) {
			if d.Class>>8 == 0x0108 {
				nd = d
				return true
			}
		}
		return false
	})
	if nd == nil {
		return nil, 0, 0, fmt.Errorf("no NVMe controller in the PCIe hierarchy (MCFG)")
	}
	bar := nd.BAR(0)
	// Het registerblok: CAP/CC/CSTS/AQA/ASQ/ACQ in de eerste 4KB, de
	// doorbells vanaf 0x1000 met stride 4<<DSTRD — 64KB dekt elke DSTRD tot 4.
	if bar == 0 || !uefi.MapHigh(bar, 0x10000) {
		return nil, 0, 0, fmt.Errorf("nvme: %v BAR0 %#x unreachable", nd, bar)
	}
	nd.Enable()
	c := &nvme.Controller{Base: uintptr(bar)}
	dma, size := uefi.NVMeDMA()
	if err := c.Init(dma, size); err != nil {
		return nil, 0, 0, fmt.Errorf("nvme: %v: %w", nd, err)
	}
	// Reuse the ANS data path: fast copies through Normal memory, with the
	// existing xfer Push/Pull handing cache ownership to and from the device.
	// Init has quiesced the old controller and completed its identify commands.
	if err := memattr.NormalNC(dma, uintptr(size)); err != nil {
		fmt.Printf("nvme: DMA stays device-mapped (%v)\n", err)
	} else if err := memattr.NormalWB(dma+nvme.DataOff, nvme.DataSize); err != nil {
		fmt.Printf("nvme: data buffer stays uncached (%v)\n", err)
	} else {
		fmt.Println("nvme: data buffer write-back cached; queues and PRPs uncached")
	}
	return c, 0, c.Blocks, nil
}
