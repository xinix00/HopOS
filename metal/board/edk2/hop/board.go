// Package hop is het board van de virtuele EDK2-machine: QEMU virt met
// EDK2-firmware, de proeftuin waarop het UEFI-pad wordt ontwikkeld en waarop
// de gate en image/uefi-run.sh draaien.
//
// Het is de generieke UEFI/ACPI-laag (board/uefi + board/uefi/hop) plus het
// enige dat deze machine eigen heeft: de igb die QEMU als NIC presenteert. Er
// is hier geen thermometer (geen PCCT), geen clusterindeling en geen
// board-specifieke interruptlijn — precies daarom is dit de machine waarop
// zichtbaar wordt of de universele weg op zichzelf werkt.
//
// Bouwen met -tags "edk2 linkcpuinit" (cmd/hopos/board_edk2.go;
// image/uefi-run.sh zonder BOARD).
package hop

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/board/uefi"
	uefihop "github.com/xinix00/HopOS/metal/v2/board/uefi/hop"
	"github.com/xinix00/HopOS/metal/v2/driver/nic/igb"
	"github.com/xinix00/HopOS/metal/v2/driver/pcie"
	"github.com/xinix00/HopOS/metal/v2/net/netdev"
)

// machine is de EDK2-proeftuin: de UEFI-laag, en verder niets eigens.
type machine struct{ uefihop.Machine }

func init() {
	board.Use(machine{})
	uefihop.RegisterNIC("igb", func(v, d uint16) bool { return v == 0x8086 && igb.Supported(d) }, upIGB)
}

var _ board.Board = machine{}

// Name is de consolenaam van dit board.
const Name = "QEMU virt (EDK2/ACPI)"

// CoreClass: de QEMU-N1 is homogeen.
func (machine) CoreClass(i int) string { return "big" }

// upIGB: BAR0 → reset/link → ringen. Zelfde recept als op de Altra; QEMU
// emuleert dezelfde chip.
func upIGB(d *pcie.Device) (netdev.Device, [6]byte, uintptr, uintptr, error) {
	bar := d.BAR(0)
	if bar == 0 || !uefi.MapHigh(bar, 0x20000) {
		return nil, [6]byte{}, 0, 0, fmt.Errorf("igb: BAR0 %#x unreachable", bar)
	}
	d.Enable()
	nic := &igb.Net{Base: uintptr(bar)}
	if err := nic.Reset(); err != nil {
		return nil, [6]byte{}, 0, 0, err
	}
	speed, fd, err := nic.LinkUp(8 * time.Second)
	if err != nil {
		return nil, [6]byte{}, 0, 0, err
	}
	fmt.Printf("net: igb %04x:%04x link %dMbps full-duplex=%v MAC %02x:%02x:%02x:%02x:%02x:%02x\n",
		d.VendorID, d.DeviceID, speed, fd,
		nic.MAC[0], nic.MAC[1], nic.MAC[2], nic.MAC[3], nic.MAC[4], nic.MAC[5])
	if err := nic.Init(layout.NetDMAPA(), layout.NetDMASize); err != nil {
		return nil, [6]byte{}, 0, 0, err
	}
	base, size := nic.BufRegion()
	return nic, nic.MAC, base, size, nil
}
