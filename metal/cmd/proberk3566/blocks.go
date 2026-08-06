package main

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/board/rk3566"
	"github.com/xinix00/HopOS/metal/cpu/memattr"
	"github.com/xinix00/HopOS/metal/dev"
)

// probeBlocks meet de drie kleine randblokken die de agent-fase nodig heeft:
// temperatuursensor, watchdog en TRNG. Alle drie lezen-en-melden, want ze
// beantwoorden vragen die je niet met code kunt beantwoorden.
//
// De watchdog wordt hier BEWUST NIET gewapend. Een gewapende DW-WDT kan niet
// meer uit, dus zou de probe zichzelf na ~90s herstarten en daarmee zijn eigen
// heartbeat-meting slopen. Wat wél kan zonder risico: de TOP-tabel meten.
func probeBlocks() {
	// --- temperatuur -------------------------------------------------------
	//
	// Dit is het getal waar de terugklok-vraag op hangt. Wat we hier zien is de
	// IDLE-basislijn: een probe doet niets, dus dit is de bodem waartegen een
	// belaste node straks vergeleken wordt. Zonder deze meting is "moet dit
	// bord terugklokken" een mening.
	rk3566.TempInit()
	cpuRaw, gpuRaw, autoCon := rk3566.TempRaw()
	fmt.Printf("\ntsadc: auto-con %#08x | raw cpu %d gpu %d\n", autoCon, cpuRaw, gpuRaw)
	if mC, ok := rk3566.Temp(0); ok {
		gpu, _ := rk3566.Temp(1)
		fmt.Printf("tsadc: cpu %d.%d°C  gpu %d.%d°C — IDLE baseline\n",
			mC/1000, abs(mC%1000)/100, gpu/1000, abs(gpu%1000)/100)
	} else {
		g26, s51, grf := rk3566.TempClockInfo()
		fmt.Printf("tsadc: no valid reading (raw %d is below the table's first entry 1584) — "+
			"clkgate26 %#08x clksel51 %#08x grf_tsadc_con %#08x\n", cpuRaw, g26, s51, grf)

		// De delers en de GRF-bits landden aantoonbaar (gemeten 06-08:
		// clksel51 0x1715, grf 0x107), dus de AFGELEIDE waarden waren NIET de
		// fout. Wat dan wel? De volgende verdachte is de parent zelf: gpll_100m
		// hoeft niet te lopen. xin24m loopt gegarandeerd — die voedt de
		// bootketen — dus dat is de goedkoopste tweede meting in dezelfde boot.
		fmt.Printf("tsadc: dividers and GRF bits landed, so the derived values are not the fault. " +
			"Retrying with the tsen clock on xin24m (the only parent that is guaranteed to run)\n")
		rk3566.TempSetTsenParent(0)
		rk3566.TempInit()
		cpuRaw, gpuRaw, autoCon = rk3566.TempRaw()
		if mC, ok := rk3566.Temp(0); ok {
			gpu, _ := rk3566.Temp(1)
			fmt.Printf("tsadc: ON XIN24M it works — cpu %d.%d°C gpu %d.%d°C (so gpll_100m was not running)\n",
				mC/1000, abs(mC%1000)/100, gpu/1000, abs(gpu%1000)/100)
		} else {
			_, s51, grf = rk3566.TempClockInfo()
			fmt.Printf("tsadc: still nothing on xin24m (raw cpu %d gpu %d auto-con %#08x clksel51 %#08x grf %#08x) — "+
				"the parent clock was not the fault either\n", cpuRaw, gpuRaw, autoCon, s51, grf)
			// Twee hypothesen weggestreept en geen onderbouwde derde: dan is
			// kijken goedkoper dan gissen. Een bord op kamertemperatuur hoort
			// ergens rond code 2100 te geven; staat dat getal ergens in dit blok,
			// dan converteert hij wél en zoek ik op de verkeerde offset.
			d := rk3566.TempDump()
			for i := 0; i < 32; i += 8 {
				fmt.Printf("tsadc: +%02x  %08x %08x %08x %08x %08x %08x %08x %08x\n",
					i*4, d[i], d[i+1], d[i+2], d[i+3], d[i+4], d[i+5], d[i+6], d[i+7])
			}
			fmt.Printf("tsadc: looking for a value near 2100 (0x834) anywhere above — that would mean the " +
				"conversion runs and I am reading the wrong offset\n")
		}
	}

	// --- watchdog ----------------------------------------------------------
	//
	// Twee vragen. Eén: meldt dit IP-core de vaste TOP-tabel? Twee: wat is de
	// ECHTE timeout? Die tweede meten we door TORR te zetten en te kicken — de
	// teller laadt dan met de werkelijke TOP — zonder de enable-bit aan te
	// raken. Leest CCVR nul, dan laadt hij niet zolang hij uit staat, en dan is
	// het antwoord dat we het pas bij het wapenen zullen weten.
	rk3566.WatchdogClockOn()
	cr, torr, ccvr, params := rk3566.WatchdogInfo()
	fmt.Printf("\nwdt: cr %#08x torr %#08x ccvr %#08x comp_params_1 %#08x (fixed TOPs: %v, already running: %v)\n",
		cr, torr, ccvr, params, params&(1<<6) != 0, cr&1 != 0)
	if secs, fixed := rk3566.WatchdogProbeTop(); secs > 0 {
		fmt.Printf("wdt: measured timeout at TOP 15 = %.2fs (fixed-top bit %v) — this is what THIS silicon does, "+
			"not what the Linux table claims\n", secs, fixed)
	} else {
		fmt.Printf("wdt: counter does not load while disabled — the real timeout is only measurable at arm time\n")
	}
	// De reset-scope-test staat achter een expliciete sleutel: hij maakt van het
	// bord een herstartlus, en dat wil je kiezen. Hij komt HIER, na alle metingen
	// hierboven, zodat een boot met de sleutel aan nog steeds alles meet vóór hij
	// zichzelf omlegt.
	if rk3566.BootParam("hopos.wdtest") == "1" {
		defer rk3566.WatchdogResetTest() // ná de TRNG-meting hieronder
	} else {
		fmt.Printf("wdt: NOT armed (a probe that reboots itself cannot measure) — " +
			"set hopos.wdtest=1 in hopos.cfg to run the reset-scope test\n")
	}

	// --- TRNG --------------------------------------------------------------
	//
	// Twee rondes, want de belangrijkste storing van een entropieblok is niet
	// "geen antwoord" maar "elke keer hetzelfde antwoord". Twee identieke rondes
	// zijn dus een fout, ook al lijken de bytes willekeurig.
	//
	// De aankondiging staat er VOOR de eerste aanraking, en dat is geen
	// nettigheid: als dit blok de bus vasthoudt sterft de probe hier stil, en dan
	// is deze regel het enige dat zegt wáár. Dezelfde reden dat de GMAC-read in
	// main.go als laatste staat — en dezelfde les die me vannacht een boot kostte
	// (zie board/rk3566/rng.go).
	fmt.Printf("\ntrng: touching the block at 0xfe388000 now — if the log stops here, it holds the bus\n")
	var a, b [8]byte
	src1, ok1 := rk3566.Fill(a[:])
	src2, ok2 := rk3566.Fill(b[:])
	fmt.Printf("\ntrng: round 1 %x (%s, ok=%v)\ntrng: round 2 %x (%s, ok=%v)\n",
		a, src1, ok1, b, src2, ok2)
	switch {
	case !ok1 || !ok2:
		fmt.Printf("trng: unusable — the DRBG falls back to timing jitter (that is the designed behaviour, not a crash)\n")
	case a == b:
		fmt.Printf("trng: BOTH ROUNDS IDENTICAL — the block answers but does not generate; treat as unusable\n")
	default:
		fmt.Printf("trng: two independent rounds — usable as the DRBG seed\n")
		// En NU pas de DRBG erop omhangen. Niet eerder: de boot-seed komt uit
		// jitter omdat de runtime-hook vóór main draait en daar geen ongemeten
		// MMIO hoort.
		if src, ok := rk3566.UseHardwareRNG(); ok {
			fmt.Printf("trng: DRBG reseeded from %s\n", src)
		} else {
			fmt.Printf("trng: reseed refused (%s) — DRBG stays on the jitter seed\n", src)
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// tempLine geeft de temperatuur voor de heartbeat-regel, of een lege string als
// de sensor niets geldigs levert. Elke tik meten maakt van de heartbeat een
// thermisch verloop: loopt het idle al op, dan zegt dat meer dan één momentopname.
func tempLine() string {
	mC, ok := rk3566.Temp(0)
	if !ok {
		return ""
	}
	gpu, _ := rk3566.Temp(1)
	return fmt.Sprintf(" — cpu %d.%d°C gpu %d.%d°C",
		mC/1000, abs(mC%1000)/100, gpu/1000, abs(gpu%1000)/100)
}

// probeVOP2 is trede 4a: de display-controller opbrengen en meten waar het
// stopt. BEWUST vóór de HDMI-kant, want die twee falen met hetzelfde symptoom
// (zwart scherm) en zijn dan niet te scheiden. Wat hier bewezen wordt is:
// power-domein aan, klokketen open, PLL gelockt, en de VP die zijn configuratie
// overneemt. Of er licht uit de connector komt is de vraag daarná.
func probeVOP2() {
	fmt.Printf("\nvop2: powering up the video domain (PD_VO)\n")
	if err := rk3566.PowerOnVO(); err != nil {
		st, ack, idle := rk3566.PowerVOInfo()
		fmt.Printf("vop2: %v (pmu status %#08x ack %#08x idle %#08x)\n", err, st, ack, idle)
		return
	}
	st, ack, idle := rk3566.PowerVOInfo()
	fmt.Printf("vop2: PD_VO on (pmu status %#08x ack %#08x idle %#08x — bit7/bit4 should be 0)\n", st, ack, idle)

	// De liveness-test vóór de scanout, want zonder dit onderscheid is elke
	// verdere meting waardeloos: een blok waarvan de klok dicht staat leest
	// nullen, en dat lijkt op een register dat we verkeerd programmeren.
	rk3566.VOPClockTree()
	g20, s37, s38, s39, raw0, raw4 := rk3566.VOPClockInfo()
	fmt.Printf("vop2: clkgate20 %#08x clksel37 %#08x clksel38 %#08x clksel39 %#08x | cfg_done %#08x VERSION_INFO %#08x\n",
		g20, s37, s38, s39, raw0, raw4)
	if !rk3566.VOPAlive() {
		fmt.Printf("vop2: VERSION_INFO is %#08x — all-zero means not clocked, all-ones a dead bus. "+
			"PD_VO is provably on, so look at the clock tree.\n", raw4)
		return
	}
	// De oude schrijf-lees-test staat er nog als DIAGNOSE, niet als poort: hij
	// gaf een vals negatief en brak daarmee de hele scanout af (zie
	// board/rk3566/vop2.go). Nu is hij één regel in de log.
	fmt.Printf("vop2: alive (VERSION_INFO %#08x) — vp-register write/read-back: %v\n",
		raw4, rk3566.VOPWriteReadBack())

	if err := rk3566.VOPScanout(); err != nil {
		fmt.Printf("vop2: %v\n", err)
		return
	}
	// cfg_done self-clears zodra de VP de configuratie bij frame-start latcht.
	// Blijft hij staan, dan scant de VP niet — en dan is de pixelklok of de
	// timing de verdachte, niet het window.
	taken := rk3566.VOPCfgDoneTaken()
	dsp, cfg, ifEn, ifPol, win, mst, ints := rk3566.VOPInfo()
	fmt.Printf("vop2: dsp_ctrl %#08x cfg_done %#08x if_en %#08x if_pol %#08x | win_ctrl %#08x mst %#08x | vp_int %#08x\n",
		dsp, cfg, ifEn, ifPol, win, mst, ints)
	if taken {
		fmt.Printf("vop2: SCANNING — the VP latched its config within a frame, so the pixel clock runs " +
			"and VP0 is out of standby. HDMI-TX is the next layer.\n")
		fillTestPattern()
	} else {
		fmt.Printf("vop2: cfg_done bit never cleared — the VP is not scanning. Suspect the pixel clock " +
			"(HPLL/DCLK_VOP0) or the timing registers, not the window.\n")
	}
}

// probeHDMI is trede 4b: de transmitter. Komt ná probeVOP2 omdat de PHY tegen
// een stilstaande pixelklok geprogrammeerd zou worden als VOP2 nog niet scant —
// de DRM-ordening (eerst alle CRTC's, dan de bridges) is niet toevallig.
//
// De eerste stap is bewust LEZEN ZONDER SCHRIJVEN: vier identificatieregisters
// bewijzen in één keer het basisadres, de ×4-registerstap, de klokken en het
// power-domein. Leest dat niet goed, dan heeft configureren geen zin en weten we
// dat vóórdat we één bit hebben aangeraakt.
func probeHDMI() {
	rk3566.HDMIClockOn()
	design, rev, p0, p1, cfg2, ok := rk3566.HDMIIDs()
	fmt.Printf("\nhdmi: design %#02x rev %#02x product %#02x/%#02x config2 %#02x → version v%x.%03x\n",
		design, rev, p0, p1, cfg2, (design<<8|rev)>>12, (design<<8|rev)&0xFFF)
	if !ok {
		fmt.Printf("hdmi: NOT a recognised DW-HDMI controller (product id must be 0xa0 / 0x01 with the hdcp bits masked). " +
			"Either the base address, the x4 register stride, the iahb clock or PD_VO is wrong — " +
			"and this measurement cost no writes at all.\n")
		return
	}
	fmt.Printf("hdmi: controller ok, hdcp %v, phy type %#02x (svsret %v), hotplug %v\n",
		p1&0xC0 != 0, cfg2, rk3566.HDMIPhyHasSVSRET(), rk3566.HDMIHotplug())

	if err := rk3566.HDMIEnable(); err != nil {
		st, conf, clkdis, invid, vpc := rk3566.HDMIInfo()
		fmt.Printf("hdmi: %v\n", err)
		fmt.Printf("hdmi: phy_stat %#02x phy_conf0 %#02x mc_clkdis %#02x fc_invidconf %#02x vp_conf %#02x\n",
			st, conf, clkdis, invid, vpc)
		fmt.Printf("hdmi: PHY did not lock — suspect HPLL (must be 148.5MHz, it feeds the phy reference too), " +
			"the svsret bit, or the phy-i2c path (PHY_I2CM_INT must be 0x08 or every write times out)\n")
		return
	}
	st, conf, clkdis, invid, vpc := rk3566.HDMIInfo()
	fmt.Printf("hdmi: TMDS ON — phy_stat %#02x (lock %v hpd %v) phy_conf0 %#02x mc_clkdis %#02x fc_invidconf %#02x vp_conf %#02x\n",
		st, st&0x01 != 0, st&0x02 != 0, conf, clkdis, invid, vpc)
	fmt.Printf("hdmi: 1920x1080p60 DVI-mode should now be on the connector — LOOK AT THE SCREEN, " +
		"that is the only remaining measurement\n")
}

// fillTestPattern tekent vier kleurbalken met een rand in de framebuffer.
//
// WAAROM DIT ER IS: de eerste keer dat er beeld kwam (06-08) was het gekleurde
// sneeuw. Dat was géén fout — de probe wees VOP2 naar 8MB DRAM waar nooit iemand
// iets in had geschreven, en de VOP2 scande dat trouw uit. Ruis op het scherm
// bewees dus juist dat de hele keten liep; alleen de inhoud ontbrak.
//
// Eén frame met een bekend patroon beantwoordt daarna meerdere vragen tegelijk:
//
//   - komen de kleuren goed door (rood links, dan groen, blauw, wit)? Staat de
//     volgorde omgekeerd, dan is R en B verwisseld en moet SwapRB aan;
//   - klopt de stride? Is hij fout, dan lopen de balken schuin in plaats van
//     recht — het klassieke beeld van een verkeerde VIR;
//   - klopt de geometrie? De rand van één pixel hoort rondom precies zichtbaar
//     te zijn; valt er een kant weg, dan staat de actieve regio verkeerd.
//
// De regio wordt eerst naar Normal-NC geremapt (write-combine), zelfde
// behandeling als fb.Init doet: zonder dat is elke pixel een aparte
// Device-nGnRnE-transactie en duurt één frame vullen seconden in plaats van
// milliseconden.
func fillTestPattern() {
	base, w, h, stride := rk3566.FB()
	if err := memattr.NormalNC(base, uintptr(stride*h)); err != nil {
		fmt.Printf("vop2: framebuffer stays device-mapped (%v) — filling will be slow\n", err)
	}

	const (
		red    = 0x00FF0000
		green  = 0x0000FF00
		blue   = 0x000000FF
		white  = 0x00FFFFFF
		border = 0x00FFFF00 // geel
	)
	bars := [4]uint32{red, green, blue, white}

	for y := 0; y < h; y++ {
		row := base + uintptr(y*stride)
		for x := 0; x < w; x++ {
			c := bars[x*4/w]
			if x < 2 || x >= w-2 || y < 2 || y >= h-2 {
				c = border
			}
			dev.Write32(row+uintptr(x*4), c)
		}
	}
	dev.MB()
	fmt.Printf("vop2: test pattern written (%dx%d, stride %d) — red|green|blue|white bars with a yellow border. "+
		"Colours in the wrong order = R/B swapped; slanted bars = wrong stride; missing edge = wrong active region\n",
		w, h, stride)
}
