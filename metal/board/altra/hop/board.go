// Package hop is het board van de Ampere Altra (en AmpereOne): de generieke
// UEFI/ACPI-laag (board/uefi + board/uefi/hop) plus wat deze machine eigen
// heeft.
//
// Alles wat de firmware vertelt komt langs de universele weg: cores uit de
// MADT, RAM uit de memory-map, PCIe uit de MCFG, beeld uit GOP, console en
// watchdog uit SPCR/GTDT, CPU_ON via PSCI. Wat hier staat is de kennis die
// geen ACPI-tabel draagt: de igb-NIC die op deze machines zit, de SoC-
// thermometer via de SMpro, en het gedrag als de firmware over clusters
// zwijgt.
//
// Registratie: dit pakket importeert board/uefi/hop (de laag, die zich NIET
// zelf registreert) en meldt zich daarna aan. Bouwen met -tags "altra
// linkcpuinit" (cmd/hopos/board_altra.go; image/uefi-run.sh BOARD=altra).
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

// machine is de Altra: de UEFI-laag met de Ampere-kennis eroverheen.
type machine struct{ uefihop.Machine }

func init() {
	board.Use(machine{})
	// De NIC van deze machines. Een Altra-kern draagt hierdoor alleen de
	// Intel-driver; de Realtek van de O6N zit er niet in.
	uefihop.RegisterNIC("igb", func(v, d uint16) bool { return v == 0x8086 && igb.Supported(d) }, upIGB)
}

var _ board.Board = machine{}

// Name is de consolenaam van dit board.
const Name = "Ampere Altra (UEFI/ACPI)"

// CoreClass: deze machines zijn homogeen — alles is "big". De UEFI-laag laat
// deze vraag bewust aan het board: een O6N heeft drie clusters en leidt zijn
// klassen af uit de efficiëntieklasse in de MADT.
func (machine) CoreClass(i int) string { return "big" }

// upIGB: BAR0 → reset/link → ringen (Altra-recept, sinds 13-07).
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
