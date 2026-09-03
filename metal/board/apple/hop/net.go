// net.go — het netwerk van de Mac mini M4: de Broadcom 57762 (tg3) achter
// Apple's PCIe-rootpoort 2. De board-basis (board/apple) levert de mechaniek
// die geen driver is — link-bring-up over PERST/LTSSM, de DART in bypass, de
// RID→stream-mapping; hier staat de KETEN die daar een netdev van maakt.
//
// Waarom die splitsing: board/apple mag geen driver importeren
// (docs/archief/indeling.md, door tools/importcheck.go bewaakt), en dat is hier
// precies de goede grens — de link opbrengen is silicium-kennis, de ringen zijn
// driverwerk.
package hop

import (
	"fmt"
	"net"
	"time"

	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/board/apple"
	"github.com/xinix00/HopOS/metal/v2/driver/nic/tg3"
	"github.com/xinix00/HopOS/metal/v2/driver/pcie"
	"github.com/xinix00/HopOS/metal/v2/net/netdev"
	"github.com/xinix00/lean/leandhcp"
)

// netDMASize is de regio uit het PA-plan (apple.go: NetDMAPA, 8MB). De check is
// een compile-time-vangnet voor wie de ringen verdiept zonder het plan mee te
// nemen.
const netDMASize = 0x800000

func init() {
	if netDMASize < tg3.NeedBytes {
		panic(fmt.Sprintf("apple: nic-dma %d KB in het plan, driver vraagt %d KB",
			netDMASize>>10, uintptr(tg3.NeedBytes)>>10))
	}
}

// Window is het ECAM/MMIO-plan van dit board voor driver/pcie.
func Window() pcie.Window {
	return pcie.Window{ECAMBase: apple.ECAMBase, MMIOBase: apple.MMIOBase}
}

// EnumerateNIC brengt de link op, geeft de brug een busnummer en haar
// adresvenster, wijst de BAR's toe en levert het endpoint. nil = geen link of
// geen device achter de poort.
func EnumerateNIC() (*pcie.Device, error) {
	if err := apple.LinkUp(500 * time.Millisecond); err != nil {
		return nil, err
	}
	apple.DARTBypass()

	const bus = 2
	br := uintptr(apple.ECAMBase) + uintptr(apple.EthPortDev)<<15
	apple.CfgWrite32(br+0x18, apple.CfgRead32(br+0x18)&^0x00ffffff|bus<<8|bus<<16)

	// Het adresvenster van de brug: zonder dit forwardt ze niets en leest élk
	// BAR-adres all-ones, ook al staat de link en is het endpoint zichtbaar.
	// Onze BAR's zijn 64-bit prefetchable → het prefetch-venster (0x24/0x28/0x2c).
	winEnd := uint64(apple.MMIOBase) + 0x2000_0000 - 1
	apple.CfgWrite32(br+0x24, uint32(winEnd>>20&0xFFF)<<20|1<<16|uint32(apple.MMIOBase>>20&0xFFF)<<4|1)
	apple.CfgWrite32(br+0x28, uint32(uint64(apple.MMIOBase)>>32))
	apple.CfgWrite32(br+0x2c, uint32(winEnd>>32))
	apple.CfgWrite32(br+0x20, 0x0000FFF0) // niet-prefetchbaar venster uit
	time.Sleep(10 * time.Millisecond)

	for _, d := range pcie.ScanConfigured(Window(), bus) {
		if d.IsBridge() {
			continue
		}
		next := uint64(apple.MMIOBase)
		for i := 0; i < 6; i++ {
			size := d.SetBAR64(i, next)
			if size == 0 || size > 1<<28 {
				continue
			}
			next = (next + size + 0xFFFF) &^ 0xFFFF
			i++ // 64-bit BAR beslaat twee slots
		}
		apple.MapRID(0, d.Bus, d.Dev, d.Fn, 0) // RID → DART-stream 0 (bypass)
		d.Enable()
		apple.CfgWrite32(br+0x04, apple.CfgRead32(br+0x04)|0x6) // brug: mem + master
		return d, nil
	}
	return nil, fmt.Errorf("apcie: link up but no endpoint on bus %d", bus)
}

// NIC bouwt de tg3-driver voor het gevonden endpoint: BAR0 als registerblok,
// zijn config space voor het SRAM-venster, en het MAC-adres uit de ADT.
func NIC(d *pcie.Device) *tg3.Net {
	cfg := uintptr(apple.ECAMBase) + uintptr(d.Bus)<<20 + uintptr(d.Dev)<<15 + uintptr(d.Fn)<<12
	return tg3.New(uintptr(d.BAR(0)), cfg, apple.NICMAC())
}

// lease bewaart wat ProbeNIC via DHCP ophaalde; Net en DHCPLease lezen hem.
var lease leandhcp.Lease

// ProbeNIC brengt de hele ethernetketen op: PCIe-link, endpoint, driver, link,
// ringen, en tot slot een DHCP-lease. Elke stap die kan mislukken meldt zichzelf
// met het gemeten getal erbij — een boot-cyclus op deze mini kost drie minuten,
// dus één mislukte boot moet genoeg zijn om te weten waar het stukliep.
//
// Het MAC-adres komt uit de ADT (apple.NICMAC): na een PERST staat er alleen nog
// Broadcom's default in de chip, en het echte adres woont in de firmware-tabel.
func (machine) ProbeNIC() (netdev.Device, net.HardwareAddr, error) {
	d, err := EnumerateNIC()
	if err != nil {
		return nil, nil, err
	}
	// Eén regel over waar de PCIe-controller vandaan komt: door de bootloader
	// opgebracht, of door onszelf (board/apple/apcie.go). Dat is het verschil
	// tussen "we hangen nog aan m1n1" en "dit board is van ons".
	if s := apple.PCIeUp(); s != "" {
		fmt.Println(s)
	}
	nic := NIC(d)

	if err := nic.Reset(); err != nil {
		return nil, nil, err
	}
	nic.SetMAC()

	id, err := nic.PHYID()
	if err != nil {
		return nil, nil, fmt.Errorf("tg3: MDIO bus dead: %w", err)
	}
	if id == 0 || id == 0xffffffff {
		return nil, nil, fmt.Errorf("tg3: no PHY on the MDIO bus (id %#x)", id)
	}

	speed, fd, err := nic.LinkUp(8 * time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("tg3: PHY %#x: %w — cable plugged in?", id, err)
	}
	nic.SetPortMode(speed, fd)

	if err := nic.Init(uintptr(apple.NetDMAPA), netDMASize); err != nil {
		return nil, nil, err
	}

	// Een lease halen is meteen het bewijs dat DMA beide kanten op werkt:
	// DISCOVER de deur uit, OFFER binnen. Mislukt hij, dan zeggen de tellers van
	// de ontvangstketen wat er wél gebeurde.
	mac := nic.HardwareAddr()
	l, err := leandhcp.Acquire(nic, [6]byte(mac), 15*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("dhcp: %w — link %dMbit fd=%v, %s", err, speed, fd, nic.Counters())
	}
	lease = l

	return nic, mac, nil
}

// Net is het IP-plan: wat de DHCP-server gaf. Zonder lease blijft het leeg en
// boot HopOS headless — de eerlijke uitkomst, geen verzonnen statisch adres.
func (machine) Net() board.NetConfig {
	if !lease.Acquired {
		return board.NetConfig{}
	}
	return board.NetFromLease(lease)
}

// DHCPLease vult board.LeaseHolder: hopnet start hiermee de renewal, zodat de
// lease niet verloopt op een node die weken aan staat.
func (machine) DHCPLease() (leandhcp.Lease, bool) { return lease, lease.Acquired }
