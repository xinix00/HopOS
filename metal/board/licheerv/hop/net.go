package hop

import (
	"fmt"
	"net"
	"time"

	gnet "github.com/usbarmory/go-net"

	"hop-os/metal/abi/layout"
	"hop-os/metal/board"
	"hop-os/metal/dev"
	"hop-os/metal/driver/nic/dwmac"
	"hop-os/metal/driver/nic/mdio"
	"hop-os/metal/net/dhcp"
)

// Het netwerk van dit board: de DWMAC1000 op 0x04070000 met de interne 100M-
// ePHY (RMII). De driver is de IP-core (driver/nic/dwmac), de SoC-glue staat
// in ephy.go, en dit bestand is de keten: klokken → ePHY → PHY-scan →
// autonegotiatie → DMA → DHCP.
const gmacBase = 0x04070000

// nodeMAC is het MAC-adres van deze node: locally administered (bit 1 van het
// eerste byte), met "HOP" in ASCII als middenstuk. Bewust NIET willekeurig — een
// MAC die elke boot verandert kost bij iedere reboot een nieuwe DHCP-lease en een
// nieuwe hopdns-registratie.
//
// Vast mag hij óók niet zijn: twee LicheeRV's op één LAN zouden botsen, en dat
// blokkeert precies de gemengde fleet waar dit board voor bedoeld is. De SG2002
// heeft geen MAC in een fuse (de vendor-Linux haalt hem uit de device-tree of
// verzint er een), dus komt hij hier uit de CONFIG — de plek waar de identiteit
// van deze node toch al staat:
//
//	hopos.mac=02:48:4f:50:aa:bb   expliciet, als je hem zelf wil beheren
//	hopos.node=<naam>             anders: de laatste twee bytes volgen uit de naam
//
// De naam-afleiding is stabiel over reboots (dezelfde naam geeft hetzelfde adres)
// en verschilt tussen nodes zolang hun namen verschillen — wat ze per definitie
// doen, want de node-naam is zijn identiteit in het cluster. Blijft alles leeg,
// dan valt hij terug op de historische waarde mét een waarschuwing: dat is de
// enige stand waarin twee bordjes elkaar nog in de weg zitten.
//
// De chip-UID uit het efuse-blok blijft het nettere antwoord, maar die
// registerkaart hebben we niet — en adressen gokken op ijzer is hoe je een board
// stilzet. Config is hier geen omweg: het is de bron die er al is.
var nodeMAC = [6]byte{0x02, 0x48, 0x4f, 0x50, 0x00, 0x01}

// UseNetIdentity zet het MAC-adres uit de config. mac heeft voorrang; anders
// wordt het uit node afgeleid. Aanroepen vóór ProbeNIC (de board-kant van de
// main doet dat in zijn init, zie cmd/hopos/board_licheerv.go).
func UseNetIdentity(mac, node string) {
	if m, ok := parseMAC(mac); ok {
		nodeMAC = m
		return
	}
	if node == "" {
		fmt.Println("net: WARNING — no hopos.mac and no hopos.node: falling back to the built-in MAC. A second LicheeRV on this LAN will collide. HOPOS_MAC_FIXED")
		return
	}
	// FNV-1a over de naam, de onderste twee bytes eruit. Geen crypto nodig: het
	// enige dat telt is dat verschillende namen verschillende adressen geven en
	// dezelfde naam hetzelfde. Het "HOP"-voorvoegsel blijft staan, zodat een node
	// van ons herkenbaar is in een ARP-tabel.
	h := uint32(2166136261)
	for i := 0; i < len(node); i++ {
		h = (h ^ uint32(node[i])) * 16777619
	}
	nodeMAC[4] = byte(h >> 8)
	nodeMAC[5] = byte(h)
}

// parseMAC leest "aa:bb:cc:dd:ee:ff". Faalt stil (ok=false) zodat een typefout in
// de config terugvalt op de afleiding in plaats van de node zonder netwerk te
// zetten — met de naam erbij is dat nog altijd een uniek adres.
func parseMAC(s string) ([6]byte, bool) {
	var m [6]byte
	if len(s) != 17 {
		return m, false
	}
	for i := range m {
		hi, ok1 := hexNibble(s[i*3])
		lo, ok2 := hexNibble(s[i*3+1])
		if !ok1 || !ok2 || (i < 5 && s[i*3+2] != ':') {
			return m, false
		}
		m[i] = hi<<4 | lo
	}
	return m, true
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
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
