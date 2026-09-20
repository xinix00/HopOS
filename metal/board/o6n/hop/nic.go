package hop

// De NIC van de O6N: twee RTL8126/8125-poorten op de Cix P1. Deze driver
// hoort bij dit board en niet bij de UEFI-laag — een Altra-kern hoeft geen
// Realtek-code te dragen, net zomin als deze kern de igb draagt.

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/board/uefi"
	uefihop "github.com/xinix00/HopOS/metal/v2/board/uefi/hop"
	"github.com/xinix00/HopOS/metal/v2/driver/nic/rtl8126"
	"github.com/xinix00/HopOS/metal/v2/driver/pcie"
	"github.com/xinix00/HopOS/metal/v2/net/netdev"
)

func init() { uefihop.RegisterNIC("rtl8126", rtl8126.Supported, upRTL8126) }

// nicRTL is de driver-instantie van de laatste geslaagde probe: de
// IRQ-hooks van dit board hangen eraan.
var nicRTL *rtl8126.Net

// RTL geeft de Realtek-driver als de probe er een opbracht (nil = geen).
func RTL() *rtl8126.Net { return nicRTL }

// upRTL8126: BAR2 (het MMIO-blok van de Realtek; BAR0 is de I/O-alias) →
// reset/MAC → ringen + MAC aan → PHY/autoneg. NBASE-T-autonegotiatie kan
// seconden duren, dus een ruimere link-wacht dan de igb.
func upRTL8126(d *pcie.Device) (netdev.Device, [6]byte, uintptr, uintptr, error) {
	bar := d.BAR(2)
	if bar == 0 || !uefi.MapHigh(bar, 0x10000) {
		return nil, [6]byte{}, 0, 0, fmt.Errorf("rtl8126: BAR2 %#x unreachable", bar)
	}
	d.Enable()
	nic := &rtl8126.Net{Base: uintptr(bar)}
	nicRTL = nic
	if err := nic.Reset(); err != nil {
		return nil, [6]byte{}, 0, 0, err
	}
	if err := nic.Init(layout.NetDMAPA(), layout.NetDMASize); err != nil {
		return nil, [6]byte{}, 0, 0, err
	}
	speed, fd, err := nic.LinkUp(12 * time.Second)
	if err != nil {
		return nil, [6]byte{}, 0, 0, err
	}
	fmt.Printf("net: %s %04x:%04x xid %#x link %dMbps full-duplex=%v MAC %02x:%02x:%02x:%02x:%02x:%02x\n",
		nic.Name, d.VendorID, d.DeviceID, nic.XID, speed, fd,
		nic.MAC[0], nic.MAC[1], nic.MAC[2], nic.MAC[3], nic.MAC[4], nic.MAC[5])
	base, size := nic.BufRegion()
	return nic, nic.MAC, base, size, nil
}
