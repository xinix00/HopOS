// Package mdio bevat de gedeelde clause-22-PHY-logica voor HopOS' twee
// gigabit-MAC-drivers: metal/gem (Cadence GEM op de Pi 5/RP1) en metal/genet
// (Broadcom GENET v5 op de Pi 4). Beide hangen aan dezelfde PHY (BCM54213PE)
// met dezelfde clause-22-registers en advertise-waarden; alleen de MDIO-master
// (het registerpad naar de bus) verschilt per SoC. Die master zit achter de
// MDIO-interface, zodat de scan en de autonegotiatie hier één keer staan.
//
// Alleen de arithmetiek/decodering; geen board- of registeraannames buiten de
// clause-22-standaard (BMCR/BMSR/ANAR/ANLPAR/GBCR/GBSR).
package mdio

import (
	"fmt"
	"time"
)

// MDIO is een clause-22-managementpoort: één register lezen/schrijven op een
// PHY-adres. Zowel *gem.Net als *genet.Net voldoen (publieke MDIORead/Write).
type MDIO interface {
	MDIORead(phy, reg int) uint16
	MDIOWrite(phy, reg int, val uint16)
}

// Scan zoekt PHY's op de MDIO-bus en geeft (adres, id1, id2) van de eerste
// hit; de BCM54213PE meldt zich met OUI 0x600d. Een register dat 0x0000 of
// 0xFFFF teruggeeft is een lege/afwezige poort.
func Scan(rw MDIO) (addr int, id1, id2 uint16, found bool) {
	for a := 0; a < 32; a++ {
		v1 := rw.MDIORead(a, 2)
		if v1 == 0xFFFF || v1 == 0 {
			continue
		}
		return a, v1, rw.MDIORead(a, 3), true
	}
	return 0, 0, 0, false
}

// AutoNeg start autonegotiatie op de PHY en wacht (begrensd) op een link;
// geeft (snelheid in Mbps, full-duplex). Adverteert 10/100 HD+FD + 1000FD,
// (her)start autoneg, pollt BMSR op AN-complete + link, en leidt de snelheid
// af uit GBSR (1000FD) en anders ANLPAR.
func AutoNeg(rw MDIO, phy int, timeout time.Duration) (speed int, fd bool, err error) {
	const (
		bmcr = 0
		bmsr = 1
		adv  = 4
		lpa  = 5
		gctl = 9
		gsta = 10
	)
	// Adverteer alles: 10/100 HD/FD + 1000FD, dan autoneg (her)starten.
	rw.MDIOWrite(phy, adv, 0x01E1)  // 10/100 HD+FD, 802.3
	rw.MDIOWrite(phy, gctl, 0x0200) // 1000BASE-T FD
	rw.MDIOWrite(phy, bmcr, 0x1200) // ANENABLE|ANRESTART
	deadline := time.Now().Add(timeout)
	for {
		s := rw.MDIORead(phy, bmsr)
		if s&(1<<5) != 0 && s&(1<<2) != 0 { // AN complete + link up
			break
		}
		if time.Now().After(deadline) {
			return 0, false, fmt.Errorf("phy: geen link binnen %v (BMSR %#x)", timeout, s)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if rw.MDIORead(phy, gsta)&(1<<11) != 0 { // LP 1000FD
		return 1000, true, nil
	}
	l := rw.MDIORead(phy, lpa)
	switch {
	case l&(1<<8) != 0:
		return 100, true, nil
	case l&(1<<7) != 0:
		return 100, false, nil
	case l&(1<<6) != 0:
		return 10, true, nil
	default:
		return 10, false, nil
	}
}
