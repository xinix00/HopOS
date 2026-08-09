package hop

import (
	"fmt"
	"net"
	"time"

	gnet "github.com/usbarmory/go-net"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/board"
	"github.com/xinix00/HopOS/metal/board/rk3566"
	"github.com/xinix00/HopOS/metal/driver/nic/dwmac4"
	"github.com/xinix00/HopOS/metal/driver/nic/mdio"
	"github.com/xinix00/HopOS/metal/net/nodemac"
	"github.com/xinix00/lean/leandhcp"
)

// Het netwerk van de Radxa Zero 3E: het GMAC1-blok (DWMAC4, snpsver 5.10 —
// gemeten VERSION 0x3051 op 05-08) via RGMII naar een externe gigabit-PHY.
//
// De IP-core is driver/nic/dwmac4; de SoC-glue staat in board/rk3566
// (pinmux.go, grf.go, cru.go). Dit bestand is de KETEN, en die is op dit board
// langer dan op alle andere: U-Boot doet niets aan het ethernet ("No ethernet
// found"), dus komen pinmux, IO-mux-selectie, GRF-modus, klokgates, klokdeler én
// PHY-reset allemaal bij ons terecht.
const (
	gmacBase = rk3566.GMAC1Base

	// De PHY zit op MDIO-adres 1 (rk3566-radxa-zero-3e.dts: rgmii_phy1 met
	// reg = <0x1>). We scannen alsnog de hele bus: één boot-cyclus op dit bordje
	// kost een kaart-wissel, dus een adres dat afwijkt van de DTS wil ik in de
	// eerste boot zien staan en niet als "geen PHY".
	phyAddrDTS = 1
)

// nodeMAC: zelfde afleiding als de andere bordjes zonder leesbare MAC-fuse
// (metal/net/nodemac). De RK3566 heeft wél een OTP met een chip-ID, maar die
// registerkaart hebben we niet gemeten — en adressen gokken op ijzer is hoe je
// een board stilzet.
var nodeMAC = nodemac.Fallback

// UseNetIdentity zet het MAC-adres uit de config; aanroepen vóór ProbeNIC (zie
// cmd/hopos/board_rk3566.go).
func UseNetIdentity(mac, node string) {
	nodeMAC = nodemac.Identity(mac, node, "Radxa Zero 3E")
}

// netDMASize is de regio uit het plan (plan.go: netDMAPA, 8MB). Ruim boven wat
// de driver vraagt, en de check hieronder is een compile-time-vangnet voor wie
// de ringen verdiept zonder het plan mee te nemen.
const netDMASize = 0x800000

func init() {
	if netDMASize < dwmac4.NeedBytes {
		panic(fmt.Sprintf("rk3566: nic-dma %d KB in het plan, driver vraagt %d KB",
			netDMASize>>10, uintptr(dwmac4.NeedBytes)>>10))
	}
}

// lease bewaart wat ProbeNIC via DHCP ophaalde; Net en DHCPLease lezen hem.
var lease leandhcp.Lease

// ProbeNIC brengt de ethernet-keten op. Elke stap die kan mislukken meldt
// zichzelf mét het gemeten getal erbij: een boot-cyclus op dit bordje kost een
// SD-kaart heen en terug, dus één mislukte boot moet genoeg zijn om te weten
// waar het stukliep — niet "geen netwerk".
func (machine) ProbeNIC() (gnet.NetworkDevice, net.HardwareAddr, error) {
	// 1. Klokken open en de bronkeuze zetten. PCLK stond al open (de probe las
	//    VERSION), ACLK is de vraag die pas de DMA beantwoordt — dus openen we
	//    hem hier, vóórdat er descriptors bestaan om over te gokken.
	rk3566.GMACClockOn()

	// 2. Pinnen naar hun ethernet-functie, inclusief de M1-pinset-schakelaar.
	//    Dit bord gebruikt gmac1m1, en zonder die schakelaar praat een perfect
	//    geconfigureerde MAC tegen pinnen die nergens uitkomen.
	rk3566.GMACPinmux()

	// 3. RGMII-modus met nul-delays (phy-mode = "rgmii-id": de PHY doet ze zelf).
	rk3566.GMACSetRGMII()

	nic := &dwmac4.Net{Base: gmacBase, MAC: nodeMAC}

	// 4. Leeft de MAC? 0x3051 is wat dit silicium meldt; nul of alles-één
	//    betekent dat we tegen een dode bus praten en dat de rest zinloos is.
	if v := nic.Version(); v == 0 || v == 0xFFFFFFFF {
		clksel, gate, srst := rk3566.GMACClocks()
		return nil, nil, fmt.Errorf("dwmac4: no MAC at %#x (version %#08x, clksel33 %#08x clkgate17 %#08x softrst14 %#08x)",
			uintptr(gmacBase), v, clksel, gate, srst)
	}

	// 5. PHY uit reset, dan de AXI-reset pulsen, en pas dán de DMA-softreset van
	//    de MAC. Die volgorde is GEMETEN 05-08 en niet van Linux overgenomen:
	//
	//    - de PHY eerst, omdat dit bord `clock_in_out = "input"` heeft — de
	//      RGMII-referentieklok komt van de PHY af, en een PHY in reset levert
	//      geen klok. De DTS-tijden (20ms laag, 50ms wachten) zijn niet
	//      cosmetisch: te kort en de PHY is er nog niet als we hem aanspreken.
	//    - de AXI-reset omdat de eerste trede-3a-boot hier bleef hangen
	//      (`bus mode 0x00000001`): APB-kant leefde, AXI-kant stond in reset.
	rk3566.GMACPHYReset()
	rk3566.GMACAXIReset()
	if err := nic.Reset(); err != nil {
		clksel, gate, srst := rk3566.GMACClocks()
		return nil, nil, fmt.Errorf("%w (clksel33 %#08x clkgate17 %#08x softrst14 %#08x)",
			err, clksel, gate, srst)
	}

	phy, id1, id2, found := mdio.Scan(nic)
	if !found {
		clksel, gate, srst := rk3566.GMACClocks()
		return nil, nil, fmt.Errorf("dwmac4: no PHY on the MDIO bus "+
			"(expected address %d; mdc/mdio mux %d/%d, mdio-addr %#08x, clksel33 %#08x clkgate17 %#08x softrst14 %#08x)",
			phyAddrDTS, rk3566.MuxOf(4, 8+6), rk3566.MuxOf(4, 8+7), nic.MDIOState(), clksel, gate, srst)
	}
	// De scan geeft de EERSTE hit, en dat is op deze PHY adres 0 — het
	// broadcast-adres dat veel clause-22-PHY's naast hun echte adres
	// beantwoorden. Voor lezen maakt dat niet uit, voor SCHRIJVEN wel: op adres
	// 0 schrijf je naar alles wat op de bus hangt. Dus als het DTS-adres
	// antwoordt, gebruiken we dat.
	if phy != phyAddrDTS {
		if a, b := nic.MDIORead(phyAddrDTS, 2), nic.MDIORead(phyAddrDTS, 3); a == id1 && b == id2 {
			fmt.Printf("net: PHY answers at both %d and %d (id %04x:%04x) — using %d, the device-tree address\n",
				phy, phyAddrDTS, id1, id2, phyAddrDTS)
			phy = phyAddrDTS
		} else {
			fmt.Printf("net: PHY at MDIO address %d, the device tree says %d (id %04x:%04x)\n",
				phy, phyAddrDTS, id1, id2)
		}
	}

	// De RGMII-klokvertragingen ZITTEN IN DE PHY, niet in de MAC. Dit board
	// heeft phy-mode = "rgmii-id", wat letterlijk betekent: de PHY doet ze en de
	// MAC zet ze op nul (dat is wat GMACSetRGMII met CON0 = 0 doet).
	//
	// GEMETEN 06-08 waarom dit er moet staan: zonder deze stap kwam er een
	// gigabit-link tot stand en werkten 1124 ontvangen frames foutloos, terwijl
	// geen enkel VERZONDEN frame ergens aankwam. De TX-delay ontbrak, dus
	// bemonsterde de switch onze data op het verkeerde moment en gooide alles weg
	// als CRC-fout. Eén bit in de PHY, en de Linux-driver zegt er zelf bij dat de
	// beginstand uit pin-strapping of de bootloader komt — er is dus geen
	// betrouwbare default om op te vertrouwen.
	if mdio.IsRTL8211F(id1, id2) {
		txWas, rxWas := mdio.RTL8211FDelays(nic, phy)
		mdio.ConfigureRTL8211F(nic, phy, true, true) // rgmii-id: beide aan
		txNow, rxNow := mdio.RTL8211FDelays(nic, phy)
		fmt.Printf("net: RTL8211F rgmii delays tx %v→%v rx %v→%v (rgmii-id needs both on)\n",
			txWas, txNow, rxWas, rxNow)
	} else {
		fmt.Printf("net: PHY %04x:%04x is not an RTL8211F — no rgmii delay configuration applied; "+
			"if TX frames vanish while RX works, this is the first place to look\n", id1, id2)
	}

	// 6. Autonegotiatie. Volledige AutoNeg (niet de Fast-variant van de
	//    LicheeRV): dit is een gigabit-PHY en die moet 1000BASE-T adverteren.
	speed, fd, err := mdio.AutoNeg(nic, phy, 8*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("dwmac4: PHY %d (id %04x:%04x): %w — cable plugged in?", phy, id1, id2, err)
	}

	// 7. En NU de klokdeler op de onderhandelde snelheid. Deze stap heeft geen
	//    tegenhanger op de andere boards (de RP1 volgt de MAC vanzelf): hier
	//    zit de RGMII-klok in de CRU, en op gigabit-tempo blijven klokken bij een
	//    100Mbit-link geeft een link die staat en frames die nergens aankomen.
	rk3566.GMACSetSpeed(speed)

	// 8. DMA. De ringen liggen in de plan-regio: buiten élke RAM-declaratie en
	//    dus device-gemapt → ongecachet en coherent zonder cache-onderhoud.
	if err := nic.Init(layout.NetDMAPA(), netDMASize, speed, fd); err != nil {
		return nil, nil, err
	}

	// 9. Een lease halen is tegelijk het bewijs dat DMA in beide richtingen
	//    werkt: DISCOVER de deur uit, OFFER binnen. Mislukt hij, dan zegt de
	//    driver-diagnose wat er wél gebeurde (TX weg? RX leeg? ring vast?).
	l, err := leandhcp.Acquire(nic, nic.MAC, 15*time.Second)
	if err != nil {
		return nil, nil, fmt.Errorf("dhcp: %w — link %dMbit fd=%v, %s", err, speed, fd, nic.Diag())
	}
	lease = l

	return nic, net.HardwareAddr(nic.MAC[:]), nil
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
