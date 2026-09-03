package main

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/board/rk3566"
	"github.com/xinix00/HopOS/metal/v2/driver/nic/dwmac4"
)

// probeGMAC is trede 3a van de bring-up: alles onder de MAC bewijzen in ÉÉN
// boot, vóórdat er één descriptor bestaat. GEHAALD 06-08 (gigabit-link), maar
// hij blijft staan: dit is het instrument dat een regressie in de glue aanwijst
// zonder van de hele agent af te hangen.
//
// Waarom apart en niet meteen de hele driver: op dit bordje kost een
// boot-cyclus een SD-kaart heen en terug, en de keten onder een DWMAC4 op
// Rockchip-silicium heeft vijf lagen die elk stil kunnen falen — pinmux,
// M1-pinset-selectie, GRF-modus, klokgates, PHY-reset. Faalt er één, dan is het
// symptoom aan de bovenkant identiek: "geen PHY". Deze functie leest daarom
// eerst álles terug wat we gaan schrijven, schrijft het dan, leest het opnieuw
// terug, en doet pas daarna de MDIO-scan. De uitkomst wijst één laag aan in
// plaats van de hele stapel.
//
// Wat ik hier NIET doe is DMA. Dat is trede 3b (GEHAALD 06-08 in de agent: een
// DHCP-lease), en die vraag was pas zinvol toen de MDIO antwoordde — een PHY-id
// op de bus bewijst pinmux, pinset, klok én reset in één regel.
func probeGMAC() {
	nic := &dwmac4.Net{Base: rk3566.GMAC1Base}

	// 1. De fifo-maten uit de hardware. Die heeft de DMA-laag straks nodig voor
	//    TQS/RQS, en ze zijn gratis mee te nemen nu het blok toch geklokt is.
	f0, f1 := nic.HWFeatures()
	txFIFO, rxFIFO := nic.FIFOSizes()
	fmt.Printf("\ngmac: hw-feature0 %#08x hw-feature1 %#08x → tx-fifo %dB rx-fifo %dB (tqs %d rqs %d)\n",
		f0, f1, txFIFO, rxFIFO, txFIFO/256-1, rxFIFO/256-1)

	// 2. De stand VÓÓR onze eerste schrijfactie. Dit is de vraag die bepaalt
	//    hoeveel werk trede 3 echt is: heeft U-Boot iets van deze glue gedaan?
	//    Hij meldt "No ethernet found", dus de verwachting is nul over de hele
	//    lijn — maar verwachting is geen meting.
	dumpGlue("before")

	// 3. En dan de glue zelf. De VOLGORDE is hier gemeten en niet gekopieerd:
	//    klokken, pinnen, modus, PHY uit reset, AXI-reset pulsen, en pas dán de
	//    DMA-softreset van de MAC.
	//
	//    De PHY gaat vóór de MAC-reset omdat dit bord `clock_in_out = "input"`
	//    heeft: de RGMII-referentieklok komt van de PHY af. Een PHY die nog in
	//    reset staat levert geen klok, en dan kan de MAC zijn DMA-domein niet
	//    resetten. Linux komt weg met de andere volgorde omdat het de PHY-reset
	//    aan de mdio-bus overlaat; op een bord dat zijn klok van buiten krijgt is
	//    dat geen vrije keuze.
	rk3566.GMACClockOn()
	rk3566.GMACPinmux()
	rk3566.GMACSetRGMII()
	fmt.Printf("gmac: releasing PHY reset on GPIO3 PC0 (20ms low / 50ms settle)\n")
	rk3566.GMACPHYReset()
	rk3566.GMACAXIReset()
	dumpGlue("after ")

	// 4. De DMA-softreset. GEMETEN 05-08 dat deze zonder de twee stappen
	//    hierboven blijft hangen (`bus mode 0x00000001`). Mislukt hij alsnog, dan
	//    gaan we TOCH door naar de MDIO-scan: die machine zit in het CSR/APB-
	//    domein en kan best werken terwijl de DMA-kant klem zit. De vorige boot
	//    gaf hier op en gooide daarmee het antwoord weg waar hij voor bedoeld was.
	if err := nic.Reset(); err != nil {
		fmt.Printf("gmac: %v — continuing to the MDIO scan anyway (different clock domain)\n", err)
	} else {
		fmt.Printf("gmac: DMA soft reset done\n")
	}

	// 5. De hele bus aflopen, niet alleen adres 1. De DTS zegt 1, maar een
	//    afwijkend adres wil ik in DEZE boot zien staan en niet als "geen PHY" —
	//    dat scheelt een kaart-wissel.
	found := 0
	for addr := 0; addr < 32; addr++ {
		id1 := nic.MDIORead(addr, 2)
		id2 := nic.MDIORead(addr, 3)
		if id1 == 0xFFFF && id2 == 0xFFFF {
			continue
		}
		if id1 == 0 && id2 == 0 {
			continue
		}
		found++
		bmsr := nic.MDIORead(addr, 1)
		fmt.Printf("gmac: PHY at MDIO address %d — id %04x:%04x, BMSR %04x (link %v, autoneg-capable %v)\n",
			addr, id1, id2, bmsr, bmsr&(1<<2) != 0, bmsr&(1<<3) != 0)
	}
	if found == 0 {
		fmt.Printf("gmac: NO PHY on the MDIO bus (%d addresses scanned, mdio-addr reg %#08x) — with the glue "+
			"applied above, so the fault is under it: pin function of mdc/mdio, the M1 pinset switch, "+
			"the CSR clock divider, or the PHY reset line\n", 32, nic.MDIOState())
		return
	}

	// 6. Autonegotiatie is de laatste stap die zonder DMA kan, en hij is het
	//    waard: hij zegt of de KABEL en de RGMII-kant leven. Lukt hij, dan is
	//    trede 3b (descriptors) het enige wat nog tussen dit bord en een node
	//    stond — en die is inmiddels ook gehaald.
	fmt.Printf("gmac: waiting for autonegotiation (8s max — cable plugged in?)\n")
	speed, fd, err := nic.AutoNeg(1, 8*time.Second)
	if err != nil {
		fmt.Printf("gmac: autoneg: %v\n", err)
		return
	}
	rk3566.GMACSetSpeed(speed)
	clksel, gate, srst := rk3566.GMACClocks()
	fmt.Printf("gmac: LINK UP %dMbit full-duplex=%v — rgmii clock set, clksel33 %#08x clkgate17 %#08x softrst14 %#08x\n",
		speed, fd, clksel, gate, srst)
}

// dumpGlue leest alles terug wat de glue schrijft: de twee CRU-registers, de
// GRF-modus, de M1-pinsetschakelaar en de functie van élke GMAC-pin. Twee keer
// aangeroepen (voor en na), zodat een schrijfactie die niet landt zichtbaar is
// als een waarde die niet verandert — en niet pas als een MAC die niets doet.
func dumpGlue(when string) {
	clksel, gate, srst := rk3566.GMACClocks()
	con0, con1, iofunc := rk3566.GMACGlue()
	fmt.Printf("gmac %s: clksel33 %#08x clkgate17 %#08x softrst14 %#08x (a_gmac1 in reset: %v) | gmac1_con0 %#08x con1 %#08x | iofunc_sel0 %#08x (pinset M%d)\n",
		when, clksel, gate, srst, srst&(1<<12) != 0, con0, con1, iofunc, (iofunc>>8)&1)

	banks, bits, want := rk3566.GMACPins()
	line := fmt.Sprintf("gmac %s: pin mux", when)
	for i := range banks {
		got := rk3566.MuxOf(banks[i], bits[i])
		mark := ""
		if got != want[i] {
			mark = "!" // wijkt af van wat de DTS voorschrijft
		}
		line += fmt.Sprintf(" %d.%02d=%d%s", banks[i], bits[i], got, mark)
	}
	fmt.Println(line)
}
