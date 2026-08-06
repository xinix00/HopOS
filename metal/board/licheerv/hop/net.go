package hop

import (
	"fmt"
	"net"
	"time"

	gnet "github.com/usbarmory/go-net"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/board"
	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/driver/nic/dwmac"
	"github.com/xinix00/HopOS/metal/driver/nic/mdio"
	"github.com/xinix00/HopOS/metal/net/dhcp"
	"github.com/xinix00/HopOS/metal/net/nodemac"
)

// Het netwerk van dit board: de DWMAC1000 op 0x04070000 met de interne 100M-
// ePHY (RMII). De driver is de IP-core (driver/nic/dwmac), de SoC-glue staat
// in ephy.go, en dit bestand is de keten: klokken → ePHY → PHY-scan →
// autonegotiatie → DMA → DHCP.
const gmacBase = 0x04070000

// nodeMAC is het MAC-adres van deze node. De afleiding (hopos.mac, anders uit
// hopos.node) is board-onafhankelijk en staat in metal/net/nodemac — de SG2002
// heeft geen MAC in een fuse waarvan wij de registerkaart hebben, en dat geldt
// voor de meeste bordjes in deze boom.
var nodeMAC = nodemac.Fallback

// UseNetIdentity zet het MAC-adres uit de config. Aanroepen vóór ProbeNIC (de
// board-kant van de main doet dat in zijn init, zie cmd/hopos/board_licheerv.go).
func UseNetIdentity(mac, node string) {
	nodeMAC = nodemac.Identity(mac, node, "LicheeRV")
}

// lease bewaart wat ProbeNIC via DHCP ophaalde; Net en DHCPLease lezen hem.
var lease dhcp.Lease

// ProbeNIC brengt de ethernet-keten op. Elke stap die kan mislukken meldt
// zichzelf mét het gemeten getal erbij: op dit board is een boot-cyclus duur
// (kaart eruit, in de Mac, terug), dus één mislukte boot moet genoeg zijn om te
// weten waar het stukliep — niet "geen netwerk".
func (machine) ProbeNIC() (gnet.NetworkDevice, net.HardwareAddr, error) {
	// 1. Klokken. Na de FSBL staan ze open (gemeten 30-07); dit is de
	//    zekerheid voor het geval een andere first-stage ze dicht laat.
	if !ethClocksOn() {
		ethClocksEnable()
		time.Sleep(time.Millisecond)
		if !ethClocksOn() {
			return nil, nil, fmt.Errorf("dwmac: ethernet clock gates stay closed (REG_CLK_EN_0 %#08x)",
				dev.Read32(clkEn0))
		}
	}

	nic := &dwmac.Net{Base: gmacBase, MAC: nodeMAC}

	// 2. Leeft de MAC? 0x1037 is wat dit silicium meldt; nul of alles-één
	//    betekent dat we tegen een dode bus praten en dat de rest van deze
	//    keten zinloos is.
	if v := nic.Version(); v == 0 || v == 0xffffffff {
		return nil, nil, fmt.Errorf("dwmac: no MAC at %#x (version register reads %#08x)", uintptr(gmacBase), v)
	}

	// 3. De interne ePHY aanzetten (analoge kalibratie, vendor-recept) en hem
	//    even laten komen voordat MDIO iets kan vinden.
	ephyInit()
	time.Sleep(50 * time.Millisecond)

	phy, id1, id2, found := mdio.Scan(nic)
	if !found {
		return nil, nil, fmt.Errorf("dwmac: no PHY on the MDIO bus (ePHY init did not take)")
	}

	// 4. Autonegotiatie. AutoNegFast i.p.v. AutoNeg: deze PHY kan alleen
	//    10/100 en heeft geen gigabit-controleregister om te adverteren.
	speed, fd, err := mdio.AutoNegFast(nic, phy, 8*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("dwmac: PHY %d (id %04x:%04x): %w — cable plugged in?", phy, id1, id2, err)
	}

	// 5. DMA. De ringen liggen in de plan-regio (zie plan.go): buiten élke
	//    RAM-declaratie, maar op dit silicium wél cachebaar — de driver doet
	//    daarom cache-onderhoud per overdracht.
	if err := nic.Init(layout.NetDMAPA(), netDMASize, speed, fd); err != nil {
		return nil, nil, err
	}

	// 6. Een lease halen is tegelijk het bewijs dat DMA in beide richtingen
	//    werkt: DISCOVER de deur uit, OFFER binnen. Mislukt hij, dan zegt de
	//    driver-diagnose wat er wél gebeurde (TX weg? RX leeg? ring vast?).
	l, err := dhcp.Acquire(nic, nic.MAC, 15*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("dhcp: %w — link %dMbit fd=%v, %s", err, speed, fd, nic.Diag())
	}
	lease = l

	return nic, net.HardwareAddr(nic.MAC[:]), nil
}

// Net is het IP-plan: wat de DHCP-server gaf. Zonder lease (geen kabel, geen
// server) blijft het leeg en boot HopOS headless — dat is de eerlijke uitkomst,
// geen verzonnen statisch adres.
func (machine) Net() board.NetConfig {
	if !lease.Acquired {
		return board.NetConfig{}
	}
	return board.NetFromLease(lease)
}

// DHCPLease vult board.LeaseHolder: hopnet start hiermee de renewal, zodat de
// lease niet verloopt op een node die weken aan staat.
func (machine) DHCPLease() (dhcp.Lease, bool) { return lease, lease.Acquired }
