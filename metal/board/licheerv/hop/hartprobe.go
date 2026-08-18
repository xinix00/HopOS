// De HOP-kant van de app-hart-wekker-probe (hartprobe_riscv64.s): start het
// hart op de probe, lees de mailbox, rapporteer één console-regel, en zet het
// hart terug in reset. Pas als stap 7 gehaald is (de wfi keerde terug op de
// wekker, gemeten op dát hart) geeft HartWaker de wekker af — de les van
// 01-08 is dat een bewijs op hart 0 níet overdraagbaar is en dat de fout
// stil is: een hart dat niet wakker wordt is een dode rotatie, geen melding.
//
// Het vangnet is HOP's bestaande machinerie: de probe krijgt een harde
// timeout en HartOff is een SoC-reset die het hart ook uit een eeuwige wfi
// trekt — precies het mechanisme waarmee kill al werkt (hart.go).
package hop

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/board/licheerv"
	"github.com/xinix00/HopOS/metal/dev"
)

func appHartProbePC() uintptr

// De mailbox-indeling — het contract met hartprobe_riscv64.s.
const (
	probeProgress = 0  // laatst geslaagde stap (1..7)
	probeHartID   = 8  // mhartid zoals het hart zichzelf zag
	probeTime     = 16 // rdtime-sample bij start (domein-vergelijking)
	probeFireLat  = 24 // vuur-latentie van mtimecmp in tikken
	probeMip      = 32 // mip-snapshot bij een gestrande stap
	probeMsip     = 40 // 1 = ok, 2 = never pended, 3 = stuck pending
	probeWdt      = 48 // 1 = WDT-blok bereikbaar+beschrijfbaar, 2 = houdt niet
	// vast, 0 = de aanraking bus-faultte (blok in reset?) — dan blijft de
	// watchdog uit; HOP zelf overleeft zo'n fout niet, dit hart parkeert 'm.
	probeLen = 56
)

// WdtUsable meldt of het watchdog-blok bewezen is — door de probe op een
// resetbaar app-hart (stap 8), of sinds de loterij door HOP's eigen aanraking
// in ProbeAppHarts. De voorwaarde waaronder de canary (board_licheerv.go) hem
// mag wapenen.
func WdtUsable() bool { return wdtOK }

var wdtOK bool

// appWaker is de gemeten uitslag; HartWaker leest hem. Eén exemplaar en geen
// per-hart-tabel: dit board heeft één app-hart, en de dag dat er meer komen
// is dit bewust de plek die dan niet meer compileert zonder erbij na te
// denken (probeAppHart neemt het hart al als argument).
//
// mtimecmp is het ADRES dat de probe bewees, niet een afgeleide van het
// reset-blok-nummer. Dat onderscheid is de vondst van boot 5 (01-08): het
// reset-blok noemt de C906L "hart 1", maar de core noemt zichzélf mhartid 0
// en heeft een EIGEN core-lokale CLINT op dezelfde basis — mtimecmp[1] is
// daar niet bedraad, en dáárom werd de eerste park-slaap nooit gewekt. Het
// bewijs dat de CLINTs per core zijn en niet gedeeld: de probe vuurde na
// exact de gearmde 1000µs terwijl HOP's hart ernaast zijn eigen mtimecmp[0]
// continu herarmde — een gedeeld register had die meting tot ruis gemaakt.
var appWaker struct {
	ok       bool   // stap 7 gehaald: mtimecmp vuurt én wfi keert terug op dít hart
	mtimecmp uint64 // het bewezen, core-lokale mtimecmp-adres
}

func init() {
	// De asm draagt het mailbox-adres als immediate; dit houdt beide in de pas.
	if bootScratchPA != 0x8FE00000 {
		panic("hartprobe_riscv64.s: mailbox-adres loopt uit de pas met plan.go")
	}
}

// ProbeAppHarts bewijst de wekker-keten van elk app-hart — op eigenschap, niet
// op naam. Een RESETBAAR app-hart krijgt de self-probe (HartOn ernaartoe en
// terug, het enige eerlijke bewijs; met HopCore = 1 bestaat zo'n hart niet en
// draait dit dus nooit — de probe-asm is dan het instrument voor HopCore = 0).
// Een PARKEREND app-hart valt niet te proben (de switcher woont er vanaf de
// boot) en hoeft het ook niet: zijn kill-tick leunt niet op appWaker, en de
// comparator is er productie-bewezen (HOP woonde er 30-07→16-08 op UseMSleep).
//
// Aangeroepen vanuit boardWarn (cmd/hopos/board_licheerv.go), ná de
// CLINT-probe van HOP's eigen hart. Wat de self-probe meet is tweeledig: is
// het hart wekbaar (mtimecmp vuurt, wfi keert terug), en is de CLINT-decode
// core-lokaal (bij een gedeelde comparator zouden twee slapers elkaars wekker
// overschrijven).
func ProbeAppHarts() {
	m := machine{}
	probed := false
	for _, h := range m.AppHarts() {
		if m.HartResettable(h) {
			probeHart(h)
			probed = true
		}
		// Een parkerend hart valt niet te proben (het wacht op zijn adoptie)
		// en hoeft ook niet: zijn kill-tick leunt niet op appWaker (zie
		// HartTimer — de gate geldt alleen de wekker van een resetbaar hart).
	}
	if probed {
		return
	}
	// Geen resetbaar app-hart om de WDT-aanraking (stap 8) te dragen — sinds de
	// loterij is het enige resetbare hart HOP's eigen huis. Dan raakt HOP het
	// blok zelf aan, en de afweging staat hier omdat hij niet gratis is: als
	// het blok dood is, is een bus-fout op HOP's hart een dode node. Daarom
	// éérst de melding (een dode console wijst dan de dader aan), en daarom is
	// het verantwoord: ditzelfde blok gaf wekenlang probeWdt=1 vanaf de andere
	// core, en dit hart bedient dezelfde peripheral-fabric al de hele boot
	// (UART, dwmac, het reset-blok, de temperatuursensor). De CLINT-les —
	// core-lokale decode — gaat hier niet op: de DW-WDT hangt achter de
	// APB-brug, niet in het core-complex.
	fmt.Println("wdt: probing the DW-WDT block from the HOP hart (no expendable hart to carry the probe)")
	wdtOK = licheerv.WatchdogProbe()
	if wdtOK {
		fmt.Println("wdt: block alive and TORR holds — watchdog usable")
	} else {
		fmt.Println("wdt: TORR readback mismatch — watchdog stays off")
	}
}

func probeHart(h int) {
	m := machine{}
	mb := uintptr(bootScratchPA)
	dev.Clear(mb, probeLen)
	dev.Push(mb, probeLen)

	t0 := licheerv.Rdtime()
	if err := m.HartOn(h, uint64(appHartProbePC()), 0); err != nil {
		fmt.Printf("board: app hart %d probe: cannot start the core (%v) — hart sleep stays off\n", h, err)
		return
	}
	// De probe zelf is in enkele ms klaar; de deadline is ruim omdat een
	// gestrande stap net zo goed een uitslag is — die lezen we uit de
	// voortgang, niet uit geduld.
	deadline := time.Now().Add(500 * time.Millisecond)
	step := uint64(0)
	for time.Now().Before(deadline) {
		dev.Pull(mb, probeLen)
		if step = dev.Read64(mb + probeProgress); step == 7 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	dev.Pull(mb, probeLen)
	hart := dev.Read64(mb + probeHartID)
	skew := int64(dev.Read64(mb+probeTime)) - int64(t0)
	fireUs := dev.Read64(mb+probeFireLat) / 25 // 25MHz → µs
	mip := dev.Read64(mb + probeMip)
	msip := dev.Read64(mb + probeMsip)
	wdtOK = dev.Read64(mb+probeWdt) == 1

	// DE DECODE-TEST. De parkeerlus van de probe schrijft nu continu 1 naar
	// zíjn mtimecmp-adres — hetzelfde getal als het onze, want beide cores
	// zijn mhartid 0. Zien wíj hier ooit een 1 staan, dan is de CLINT gedeeld
	// en vechten twee slapers om één comparator: precies het mes dat HOP's
	// hart op 01-08 tweemaal stil velde (boots 6/7 — node weg, app-hart
	// draaide door). Dan gaat de app-hart-wekker uit. Onze eigen MSleep armt
	// hier grote toekomst-waarden en schrijft nooit een 1, dus één gelezen 1
	// is bewijs. 60 monsters over ~3ms is ruim tegen de race.
	shared := false
	if step == 7 {
		own := licheerv.MtimecmpAddr(hart)
		for range 60 {
			if dev.Read32(uintptr(own)) == 1 {
				shared = true
				break
			}
			for t := licheerv.Rdtime(); licheerv.Rdtime() < t+1250; { // ~50µs
			}
		}
	}
	_ = m.HartOff(h) // altijd terug in reset — het slot-pad boot hem vers
	// Ontsmetten kan alleen ons EIGEN comparator (de decode is core-lokaal:
	// die van hart 1 is voor ons per constructie onbereikbaar — daar staat na
	// de decode-lus een 1 in, wat hooguit een pending MTIP zonder MTIE is; de
	// eerste park-pass met wekker zou hem disarmen). hart == mhartid == 0 hier.
	licheerv.SetMtimecmp(hart, ^uint64(0))

	if step != 7 {
		// De voortgang wijst de gebroken schakel aan; mip en de tijd-skew
		// zijn de diagnose-data voor de volgende ronde silicium-archeologie.
		why := map[uint64]string{
			0: "never reached the probe (boot vector?)",
			1: "mtimecmp does not hold values",
			2: "never armed (unreachable)",
			3: "MTIP never fired — comparator not wired to this hart?",
			4: "MTIP stuck pending after disarm",
			5: "msip test never finished",
			6: "wfi never returned despite pending timer",
		}[step]
		fmt.Printf("board: app hart %d probe: stalled at step %d/7 (%s; mip=%#x, time skew %d ticks) — hart sleep stays off\n",
			h, step, why, mip, skew)
		return
	}
	if shared {
		// De keten werkt, maar het register is van ons allebei: het app-hart
		// zou bij élke park-slaap ook óns comparator herschrijven, en wie zijn
		// wek verliest slaapt voor eeuwig. Geen wekker dus — de park spint,
		// zoals in de bewezen stand, tot dit hart een écht eigen comparator
		// heeft (SoC-onderzoek: is er een tweede CLINT-basis voor de C906L?).
		fmt.Printf("board: app hart %d probe: chain ok BUT the CLINT decode is SHARED with hart 0 — two sleepers, one comparator; hart sleep stays off\n", h)
		return
	}
	appWaker.ok = true
	// De bewezen adressen: afgeleid van hoe het hart ZICHZELF ziet (mhartid
	// uit de mailbox), want dat is de index waarmee de probe zonet vuurde.
	// msip geven we bewust niet door: het kanaal is core-lokaal, dus HOP kan
	// er per constructie niet bij (die zou zijn éígen msip zetten) — de wek
	// van een slapend app-hart is en blijft de 2ms-cap.
	//
	// LET OP: appWaker.ok zegt "de keten is bewezen", niet "de slaap staat
	// aan" — dat laatste beslist appHartSleepEnabled (hart.go), en die staat
	// uit tot de stille-dood-jacht gelopen is.
	appWaker.mtimecmp = licheerv.MtimecmpAddr(hart)
	msipTxt := map[uint64]string{1: "msip ok (core-local)", 2: "no msip", 3: "msip stuck"}[msip]
	fmt.Printf("board: app hart %d probe: chain ok as mhartid %d (core-local CLINT) — mtimecmp fired in %dus, %s, wfi wakes; sleep still gated off pending soak\n",
		h, hart, fireUs, msipTxt)
}
