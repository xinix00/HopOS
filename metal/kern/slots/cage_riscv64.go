//go:build riscv64

package slots

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/board"
	"github.com/xinix00/HopOS/metal/cpu/mmode"
	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/kern/cage"
	"github.com/xinix00/HopOS/metal/kern/cagestub"
)

// De RISC-V64-helft van de kooi-naad (zie cage_arm64.go voor de andere).
// Zelfde vier beloftes, ander mechanisme — en dat is silicium, geen keuze: de
// XuanTie C906 heeft geen H-extensie, dus er ís geen stage-2. De kooi is hier
// een **PMP-whitelist** (kern/cage) die de slot-stub programmeert en
// verifieert vóór hij de app binnenspringt.
//
// Twee dingen verschillen zichtbaar van ARM, en beide zijn een vereenvoudiging:
//
//   - **Er is geen parkeerlus.** Een hart is in reset of hij draait. Elke start
//     is dus een "koude" start: reset asserten, boot-vector zetten, deasserten.
//     Precies dát wist ook de PMP-locks van het vorige slot, dus een start
//     levert per definitie een schoon slot op — waar ARM daar een expliciete
//     revoke + TLBI voor nodig heeft.
//   - **De kooi-beschrijving gaat niet op de control-page** maar in de
//     slot-tabel van de stub. De control-page is vol (0x00-0x100 is bezet) en
//     de stub is hier de trampoline: hij leest zijn kooi uit zijn eigen kop,
//     net zoals de EL2-trampoline die van de control-page leest. De naad is
//     hetzelfde: HOP schrijft de kooi waar de arch-entry hem leest.
//
// Op silicium bewezen (LicheeRV Nano, 30-07): verboden store faultt met
// mcause 7, unlock-poging wordt door WARL genegeerd, en na een hart-reset is
// PMP weer vrij.

// Slot-tabel in het slot-image (contract: image/licheerv/stub-slot). Offsets
// 8/16/24 (entry, bss-start, bss-einde) zet de build al; de kooi zetten wij.
const (
	stubEntry    = 8  // ELF-entry van de app
	stubBSSLo    = 16 // BSS-range die de stub nult (leeg: HOP nulde al)
	stubBSSHi    = 24
	stubCageCfg  = 32  // pmpcfg0
	stubCageAddr = 40  // pmpaddr0..7 — ACHT, want kern/cage codeert elk venster
	stubCageMax  = 8   // als TOR: een onder- en een bovengrens, dus twee entries
	stubScratch  = 104 // waar de stub zijn voortgang kwijt mag (layout.AbiStubOff)
	stubMapRoot  = 112 // wortel van de map-helft van de kooi (0 = niet verplaatsen)
	stubTrapPC   = 120 // mtvec vóór de sprong: HOP's switcher (0 = stub-lokale trap)
	stubSchedPA  = 128 // mscratch vóór de sprong: het sched-blok van dit hart

	// cageFloor is wat HOP vooraan in de partitie reserveert: de stub zelf, plus
	// ruimte om te groeien. Een app-image mag daar niet in landen — place.Build
	// krijgt dit als vloer, dus een image dat op de partitiebasis gelinkt is
	// wordt geweigerd in plaats van de stub te overschrijven die hem moet starten.
	cageFloor = 0x1000
)

// cageInit zet de sched-blokken van de app-harts op nul en legt de plan-basis
// erin. Geen vectoren of parkeerlus (de stub van elk slot brengt zijn eigen
// trap-entry mee en het hart komt op via het reset-blok), maar de switcher
// (cpu/mmode) vindt élk ctx-blok via SchedS2PA — staat dat op nul, dan rekent
// hij bij de eerste trap een adres uit vanaf nul. Op ARM doet stage2.InitVectors
// precies dit stuk; hier is het het enige dat er te initialiseren valt.
func cageInit() {
	base := layout.Stage2TablePA(0) // = Plan.Stage2PA; slot 0 draagt geen tabellen
	nc := layout.NumAppCores()
	dev.Clear(layout.ParkMboxPA(0), uint64(nc+1)*layout.ParkMboxLen)
	for c := 0; c <= nc; c++ {
		mb := layout.ParkMboxPA(c)
		dev.Write64(mb+layout.SchedS2PA, uint64(base))
		// De wekker van dít hart, als het board er een hééft. Zonder deze drie
		// woorden blijft de parkeerlus van de switcher spinnen zoals altijd —
		// dat is de veilige stand en niet een ontbrekende feature.
		//
		// LET OP DE VOLGORDE: het board mag hier pas "ja" zeggen als zijn
		// CLINT-probe gelopen heeft. Dat klopt vandaag (de probe zit in
		// boardWarn, die vóór agentboot.Run draait en dus vóór de eerste
		// slot-start die deze functie via vectorsOnce aanroept), en het is
		// zelf-corrigerend als het ooit verschuift: een probe die te laat komt
		// levert "geen wekker" en dus het oude gedrag.
		//
		// Alleen voor de APP-harts (c ≥ 1). Blok 0 hoort bij HOP's eigen hart,
		// daar draait geen switcher, en hartOf(0) is 0 — dat zou HOP zijn eigen
		// mtimecmp in een sched-blok schrijven en dat is precies het soort
		// adres dat je niet per ongeluk wilt laten rondslingeren.
		if c == 0 {
			dev.Push(mb, layout.ParkMboxLen)
			continue
		}
		if mtimecmp, msip, capTicks, ok := board.Current().HartWaker(hartOf(c)); ok {
			dev.Write64(mb+layout.SchedClintPA, mtimecmp)
			dev.Write64(mb+layout.SchedMsipPA, msip)
			dev.Write64(mb+layout.SchedSleepCap, capTicks)
		}
		dev.Push(mb, layout.ParkMboxLen)
	}
}

// cagePrepare rekent de kooi van slot i uit en schrijft haar in de slot-tabel
// van het image dat al in de partitie staat. De app krijgt precies één venster
// — zijn eigen partitie — plus eventueel de granted MMIO; de deny-all die
// cage.Encode erachter zet dekt al het andere, inclusief HOP.
//
// Dat het één venster is, is de winst van de slot-ABI in de partitie-staart:
// control-page en ringen liggen ín de partitie, dus er is geen tweede venster
// meer en er bestaat helemaal geen gedeelde ctrl-regio die een app zou kunnen
// benoemen. Een slot kan de control-page van een ander slot dus niet eens
// adresseren.
//
// De rekenkunde staat in kern/cage (host-getest tegen de op ijzer gemeten
// waarden); hier gaat hij alleen het geheugen in. De stub neemt geen enkele
// beslissing: hij schrijft weg wat hier staat en verifieert het.
func cagePrepare(i int, linkBase, base, size, entry uint64) error {
	// De stub op de partitiebasis: dít is het stukje dat HOP's vertrouwen draagt
	// en de app in zijn kooi achterlaat. Zonder stub zou het hart op de eerste
	// bytes van de partitie starten — nullen, of het vorige image.
	blob := cagestub.Stub()
	if len(blob) == 0 || uint64(len(blob)) > cageFloor {
		return fmt.Errorf("cage slot %d: cage stub missing or too big (%d bytes, floor %d) — build with -tags embedcagestub (image/licheerv-agent.sh)",
			i, len(blob), cageFloor)
	}
	dev.Copy(uintptr(base), blob)

	plan := cage.Plan{Allow: []cage.Window{
		{Base: base, Size: size, R: true, W: true, X: true},
	}}
	if gb, gs, ok := grantWindow(i); ok {
		plan.Allow = append(plan.Allow, cage.Window{Base: gb, Size: gs, R: true, W: true})
	}
	addrs, cfg, err := cage.Encode(plan)
	if err != nil {
		return fmt.Errorf("cage slot %d: %w", i, err)
	}
	// De vloer onder de tabel: méér entries dan de stub kan wegschrijven zou de
	// overtollige STIL laten vallen — en de laatste is de deny-all. Een kooi die
	// per ongeluk openstaat is precies wat hier nooit mag kunnen, dus faalt de
	// start liever luid. (cage.Encode heeft zijn eigen grens op MaxEntries; dit is
	// de grens van dít transportmiddel.)
	if len(addrs) > stubCageMax {
		return fmt.Errorf("cage slot %d: cage needs %d PMP entries, the slot table carries %d",
			i, len(addrs), stubCageMax)
	}
	put := func(off int, v uint64) { dev.Write64(uintptr(base)+uintptr(off), v) }
	put(stubEntry, entry)
	// BSS: HOP nulde die al bij het plaatsen van de segmenten (placeFromStaging),
	// dus een lege range — de stub slaat zijn lus dan over. Het veld blijft
	// bestaan voor het demo-pad, waar de build hem wél vult.
	put(stubBSSLo, 0)
	put(stubBSSHi, 0)
	// Scratch in de ABI-staart van deze partitie: ná het locken van de kooi mag
	// dit hart niets daarbuiten meer aanraken, en de control page is van de app.
	put(stubScratch, uint64(stubScratchAt(base, size)))
	put(stubCageCfg, cfg)
	for k := range stubCageMax {
		var v uint64
		if k < len(addrs) {
			v = addrs[k]
		}
		put(stubCageAddr+k*8, v)
	}

	// De MAP-helft van de kooi: het canonieke linkadres van dít slot naar zijn
	// echte partitie. Zonder deze tabel draait een app op het fysieke adres
	// waarop hij gelinkt is, en dan bestaat er maar één slot dat zo'n image kan
	// hebben — mét de tabel ziet élk slot zichzelf op linkBase.
	root, err := slotMap(i, linkBase, base, size)
	if err != nil {
		return fmt.Errorf("cage slot %d: %w", i, err)
	}
	put(stubMapRoot, root)

	// Waar een trap van deze app landt, en waaraan de handler zich vastgrijpt.
	// Dít is wat een tweede bewoner op dit hart mogelijk maakt: zonder deze twee
	// velden trapt de app in de stub van zijn eigen partitie, en die kan alleen
	// parkeren — hij kan niet naar een buurman wisselen, en hij ligt in geheugen
	// dat de app zelf kan herschrijven. Met deze velden landt élke trap in HOP's
	// image, buiten alle partities (cpu/mmode; mogelijk sinds de kooi ontlockt is).
	//
	// mscratch draagt het sched-blok van het hart: HOP-eigen geheugen dat in geen
	// enkele kooi zit, en het enige waar de trap-entry een pointer uit haalt vóór
	// hij iets kan opslaan.
	put(stubTrapPC, mmode.EntryPC())
	put(stubSchedPA, uint64(layout.ParkMboxPA(coreOf(i))))

	// De caches uit vóór het hart gaat fetchen. HOP schreef de stub, de
	// app-segmenten en deze tabel als gewoon (cachebaar) geheugen; het app-hart
	// komt vers uit reset met caches ÚIT en leest dus rechtstreeks DRAM. Zonder
	// deze veeg start het op wat er in DRAM stond, niet op wat wij schreven —
	// dezelfde klasse als de write-buffer-les van 30-07, maar nu andersom.
	// Eén call over de hele partitie: 64MB × 64B-regels kost ~ms bij een start,
	// en het dekt image, stub, tabel en de ABI-staart in één keer.
	dev.CleanInv(uintptr(base), uintptr(size))
	dev.MB()
	return nil
}

// slotMap zet de map-helft van de kooi in de ABI-staart van de partitie en geeft
// de wortel terug die de stub in zijn map-register schrijft. Twee vensters:
//
//   - **app-RAM, cachebaar** — het canonieke linkadres → de echte partitie. Dit
//     is waar verplaatsen voor bestaat: élk slot ziet zichzelf op hetzelfde adres.
//   - **de ABI-staart, device** — control page en ringen, alles wat de app mét HOP
//     deelt, ongecachet. Dan hoeft de app daar geen cache-onderhoud te doen,
//     precies zoals op ARM (waar dev.Push/Pull daarom no-ops zijn).
//
// De stub heeft zelf géén venster nodig: satp vertaalt alleen supervisor- en
// user-mode, en hij draait in machine mode — dus fetcht hij ongetranslateerd, ook
// ná zijn eigen csrw satp.
func slotMap(i int, linkBase, base, size uint64) (uint64, error) {
	appRAM := size - layout.AbiTail
	windows := []cage.MapWindow{
		{Link: linkBase, Phys: base, Size: appRAM, R: true, W: true, X: true},
		{Link: linkBase + appRAM, Phys: base + appRAM, Size: layout.AbiTail,
			R: true, W: true, Device: true},
	}
	// Gegrante MMIO moet óók gemapt zijn, anders ziet de app een venster dat de
	// whitelist wel toestaat maar de map niet kent. Identiek gemapt, zodat het
	// adres in de env (FB_BASE) blijft kloppen, en als device — het ís een device.
	if gb, gs, ok := grantWindow(i); ok {
		if gb%cage.BlockSize != 0 || gs%cage.BlockSize != 0 {
			return 0, fmt.Errorf("grant %#x+%#x does not fit the map's %dMB block grain",
				gb, gs, cage.BlockSize>>20)
		}
		windows = append(windows, cage.MapWindow{
			Link: gb, Phys: gb, Size: gs, R: true, W: true, Device: true,
		})
	}

	tbl := layout.AbiTailAt(base, appRAM) + layout.AbiMapOff
	m, err := cage.Relocate(cage.MapPlan{TableBase: uint64(tbl), Windows: windows})
	if err != nil {
		return 0, err
	}
	if i >= 0 && i < len(mapRoot) {
		mapRoot[i] = m.Root // voor het post-mortem: wiens map keek er?
	}
	// Gewone (cachebare) writes: de veeg over de hele partitie onderaan
	// cagePrepare publiceert ze, net als de stub en de app-segmenten.
	dev.Copy(tbl, m.Bytes)
	return m.Root, nil
}

// cageEntryPC: waar het hart binnenkomt voor slot i — de basis van zijn eigen
// partitie, want daar staat zijn kooi-stub. Op ARM is dit één vast adres (de
// EL2-trampoline in HOP's image) omdat de stage-2 daar de partitie onder een
// canoniek IPA legt; hier is het per slot verschillend.
//
// Per SLOT en niet per core, en dat is precies wat een tweede bewoner nodig
// heeft: twee slots op één hart hebben elk hun eigen partitie, dus elk hun eigen
// stub. Dit adres gaat mee in het ctx-blok (CtxBootPC) en de rotatie in de
// switcher springt er bij een cold boot naartoe.
func cageEntryPC(i int) uint64 {
	base, _, ok := partitionOf(i)
	if !ok {
		return 0
	}
	return base
}

// De core-levenscyclus is hier het reset-blok, niet een mailbox: een hart draait
// of staat in reset. Dat is meteen het antwoord op "draait hij?" en "staat hij
// stil?" — géén afspraak in geheugen die stale kan worden, maar het silicium zelf.
// (De ARM-helft heeft een parkeerlus met een mailbox-woord; die bestaat hier niet,
// en een mailbox-woord uitlezen zou daar een gestopt hart als draaiend melden
// zodra HOP het ooit geschreven had — gemeten 30-07 op het bordje: elke
// job-retry viel dan in het gedeelde-core-pad.)
func coreRunning(core int) bool { return board.Current().HartState(hartOf(core)) == board.PowerOn }

func coreStopped(core int) bool { return board.Current().HartState(hartOf(core)) == board.PowerOff }

// cageDispatch geeft het startschot: het hart uit reset op de stub in zijn eigen
// partitie. entry/ctx zijn ARM-begrippen (trampoline-PC, control-page in x0) en
// spelen hier geen rol — de partitie-basis ís de vector, en de stub leest zijn
// kooi uit zijn eigen kop.
func cageDispatch(core int, entry, ctx uint64) error { return cageColdStart(core, entry, ctx) }

// Voortgangswoorden die de stub in zijn scratch schrijft (image/licheerv/
// stub-slot); alleen de twee eindtoestanden zijn hier interessant.
const (
	stubCageOK   = 0xA3   // kooi geverifieerd, de app is ingesprongen
	stubCageFail = 0xFA11 // CageVerify mismatch: geparkeerd, app NOOIT gestart
	stubNoSMode  = 0x5107 // hart zonder misa.S: zonder supervisor-modus zou de app
	// in machine mode draaien, en dáár bindt de ontlockte whitelist hem niet —
	// geen isolatie. De stub parkeert dan, net als bij een CageVerify-mismatch.
	stubTrapped = 0xDEAD // trap ná de sprong: de app liep, de stub parkeerde hem

	// stubScratchLen is hoeveel van de scratch HOP leest: het voortgangswoord,
	// de pmpcfg0-readback, de drie trap-woorden en de vier hart-CSR's — tot en
	// met +64, dus twee cachelines. Als constante omdat de CleanInv ernaast
	// hoort te kloppen: veeg je er 64, dan is de laatste lees (mxstatus op +64)
	// niet geïnvalideerd en kán die stale zijn.
	stubScratchLen = 128
)

// stubScratchAt geeft de scratch-PA van de kooi-stub voor een partitie: in de
// ABI-staart, want ná het locken mag het hart niets daarbuiten aanraken (en de
// control page is van de app). Eén formule voor de schrijver (cagePrepare) en
// de twee lezers (cageIdent/cageWhy).
func stubScratchAt(base, size uint64) uintptr {
	return layout.AbiTailAt(base, size-layout.AbiTail) + layout.AbiStubOff
}

// cageIdent vertelt wat voor hart er onder slot i zit: misa/marchid/mimpid,
// door de kooi-stub in zijn scratch gelegd (stap 1b). Per-hart-CSR's, dus HOP
// op hart 0 kan ze niet zelf lezen — en het antwoord beslist of dit board ooit
// meerdere apps kan dragen: zonder S-bit in misa bestaat er geen modus onder
// M-mode om een app in te zetten, en dan kan HOP dit hart niet preempten.
//
// De extensieletters komen uit de onderste 26 bits van misa (bit 0 = 'a').
func cageIdent(i int) string {
	base, size, ok := partitionOf(i)
	if !ok {
		return ""
	}
	sc := stubScratchAt(base, size)
	dev.CleanInv(sc, stubScratchLen)
	misa := dev.Read64(sc + 40)
	if misa == 0 {
		return "" // stub van vóór deze meting, of nooit gelopen
	}
	var ext []byte
	for b := range 26 {
		if misa&(1<<b) != 0 {
			ext = append(ext, byte('a'+b))
		}
	}
	// mxstatus zoals teruggelezen: welke bits het silicium accepteerde. De
	// S-bit in misa zegt of een app naar supervisor-modus KAN; deze zegt of hij
	// daar zijn eigen cache-onderhoud nog mag doen (th.dcache.*, de basis onder
	// dev.Push/Pull). Samen zijn dat de twee feiten die multi-app op dit board
	// blokkeren — zie docs/technical/isolation.md.
	sup := "no S-mode"
	if misa&(1<<18) != 0 { // 's' = bit 18
		sup = "S-mode present"
	}
	// MXL staat in de bovenste twee bits: 1=RV32, 2=RV64, 3=RV128 — dus 16<<MXL,
	// niet 32<<MXL (die drukte RV64 als "rv128", gemeten op het bordje 31-07).
	return fmt.Sprintf("hart of slot %d: misa %#x (rv%d %s, %s), mxstatus %#x, marchid %#x, mimpid %#x",
		i, misa, 16<<(misa>>62), ext, sup, dev.Read64(sc+64), dev.Read64(sc+48), dev.Read64(sc+56))
}

// cageWhy vertelt hoe ver de kooi-stub van slot i kwam, of leeg als hij gewoon
// de app insprong. Alleen gelezen wanneer er iets mís is (het post-mortem in
// kern/slots) — in het gelukkige geval is er niets te melden, en tijdens een
// start is elke console-regel er één te veel: bij 100Mbit kost één regel meer
// buffering dan de hele NIC-ring heeft (zie driver/nic/dwmac).
func cageWhy(i int) string {
	base, size, ok := partitionOf(i)
	if !ok {
		return ""
	}
	sc := stubScratchAt(base, size)
	dev.CleanInv(sc, stubScratchLen)
	mc, pc, tv := dev.Read64(sc+16), dev.Read64(sc+24), dev.Read64(sc+32)
	switch st := dev.Read64(sc); st {
	case stubCageOK, stubTrapped:
		// stubCageOK betekent "de sprong is gemaakt" — maar dat is nog niet
		// "verder is het het verhaal van de app". Met medeleg op nul komt ÉLKE
		// trap van de app in M-mode uit, dus bij het trap-vector van deze stub,
		// en die schrijft alleen de trap-woorden weg; het voortgangswoord blijft
		// dan op stubCageOK staan. Zonder deze toets is zo'n trap onzichtbaar:
		// het slot leest als "app gestart" en sterft stil (gemeten 31-07 op het
		// bordje, bij de sprong naar S-mode: status Booting, geen logregel).
		if st == stubTrapped || mc != 0 || pc != 0 {
			return fmt.Sprintf("app trapped and was parked by the cage stub (mcause %#x mepc %#x mtval %#x)",
				mc, pc, tv)
		}
		return "" // écht niets te melden: gesprongen en geen trap gezien
	case 0:
		return "cage stub gave no sign of life"
	case stubCageFail:
		return fmt.Sprintf("CAGEVERIFY FAILED (pmpcfg0 readback %#x) — app never started", dev.Read64(sc+8))
	case stubNoSMode:
		return "hart has no supervisor mode (misa.S) — without that level the cage does not bind it, app never started"
	default:
		return fmt.Sprintf("cage stub stopped at %#x (trap mcause %#x mepc %#x mtval %#x)",
			st, mc, pc, tv)
	}
}

// cageFaultRegs is het spoor dat de switcher (cpu/mmode) bij een fault in het
// ctx-blok van de dode bewoner achterlaat: waar was hij (mepc), waar kwam hij
// vandaan (ra), waar stond zijn stack (sp) en met welke map hij keek (satp).
// Alles wordt pas op het FAULT-pad geschreven, dus de wissel zelf betaalt er
// niets voor — een teller op dat pad zou dat wél doen (hij liep in de miljarden). Voor het post-mortem — een wilde
// sprong zonder afzender is alleen een bestemming, en daar jaag je niet mee.
func cageFaultRegs(i int) string {
	c := ctxPA(i)
	dev.Pull(c, 64)
	dev.Pull(c+layout.CtxResume, 64) // 288 en 304 liggen in één regel
	satp := dev.Read64(c + layout.CtxRegime)
	// Wiens map was dit? HOP weet welke wortel hij voor élk slot schreef, dus dit
	// is geen gis: staat er de wortel van een ánder slot, dan keek deze bewoner
	// door de tabel van zijn buurman en is de wissel de dader. Staat de eigen
	// wortel er, dan was de vertaling goed en ligt de fout in wat eronder stond.
	whose := "onbekende map"
	switch {
	case i < len(mapRoot) && satp == mapRoot[i]:
		whose = "own map"
	default:
		for k, r := range mapRoot {
			if r != 0 && r == satp {
				whose = fmt.Sprintf("map of slot %d!", k)
			}
		}
	}
	return fmt.Sprintf(" pc=%#x ra=%#x sp=%#x satp=%#x (%s)",
		dev.Read64(c+layout.CtxResume), dev.Read64(c+layout.CtxGPRs),
		dev.Read64(c+layout.CtxGPRs+8), satp, whose)
}

// mapRoot onthoudt de map-wortel die HOP per slot schreef — alleen om een
// fault-rapport te kunnen toeschrijven (zie cageFaultRegs). Geen vertrouwde
// bron voor iets anders: de app kan zijn eigen satp herschrijven, en dat mag
// hij ook — de whitelist is de invariant, niet deze tabel.
var mapRoot [layout.SlotCap + 1]uint64

// cageColdStart laat het hart lopen op de stub in zijn eigen partitie. Waar de
// ARM-kant hier een trampoline-PC in HOP's image krijgt, is entry hier de
// partitie-basis (cageEntryPC) — dát is de vector. ctx (de control-page) speelt
// geen rol: de stub leest alles uit zijn eigen kop.
func cageColdStart(core int, entry, ctx uint64) error {
	if entry == 0 {
		return fmt.Errorf("cage: core %d has no prepared partition", core)
	}
	hart := hartOf(core)
	if hart == 0 {
		return fmt.Errorf("cage: no hart for logical core %d", core)
	}
	return board.Current().HartOn(hart, entry)
}

// cageRevoke: hart in reset. Dat stopt hem waar hij ook is (ook uit een tight
// loop) én wist zijn PMP-locks — de hard-kill en het schone slot in één.
func cageRevoke(i int) {
	if err := board.Current().HartOff(hartOf(coreOf(i))); err != nil {
		fmt.Printf("HOPOS_CAGE_REVOKE_FAILED: slot %d: %v\n", i, err)
	}
}

// cageSMPEntryPC: geen SMP op dit board. Er is één app-hart, dus een slot heeft
// nooit een tweede core om binnen te laten komen. HOP publiceert 0 en de
// app-OS-laag vraagt er niets bij (CtrlCores blijft 1).
func cageSMPEntryPC() uint64 { return 0 }

// hartOf vertaalt een LOGISCH HopOS-corenummer (1..N, aaneengesloten) naar het
// FYSIEKE hart-ID van dit board. 0 = er is geen hart voor dit nummer.
//
// Waarom die vertaling bestaat: kern/slots rekent overal met aaneengesloten cores
// 1..N — NumSlots telt zo, de pool-allocator loopt zo, sharegroups verdelen zo.
// Op ARM klopt dat met het silicium, want PSCI nummert cores aaneengesloten vanaf
// nul. Een RISC-V-board levert echter een LIJST hart-ID's (AppHarts) en die hoeft
// niet aaneengesloten te zijn en niet bij 1 te beginnen.
//
// Zonder deze laag werkte dat alleen bij toeval. De LicheeRV heeft één app-hart met
// ID 1, dus logisch 1 == fysiek 1 en alles klopte; een board met harts [2,4] zou
// nul slots opleveren, omdat de telling bij logisch 1 begint en daar geen hart
// vindt. Dat is geen theoretisch geval — het is de eerstvolgende SoC.
//
// De regel is nu: buiten dit bestand bestaan alléén logische nummers, en élke
// board-call gaat hierlangs.
func hartOf(core int) int {
	harts := board.Current().AppHarts()
	if core < 1 || core > len(harts) {
		return 0
	}
	return harts[core-1]
}

// coreExists: bestaat er een hart voor dit logische nummer? De topologie is
// board-kennis, niet iets om te proben — er is geen PSCI-telling die kan liegen,
// AppHarts() ís het antwoord. Aaneengesloten per definitie: N harts geven de
// logische cores 1..N, wélke ID's ze ook hebben.
func coreExists(core int) bool { return hartOf(core) != 0 }

// cageLinkBase: waar een verplaatst slot verschijnt. De eerste poolregio van het
// board — dáár is elk app-image op gelinkt (board/licheerv/hop/plan.go zet die
// regio bewust vooraan, en image/licheerv-agent.sh linkt op SlotBase+0x10000).
// Het canonieke IPA-venster dat ARM gebruikt bestaat hier niet: er is geen
// tweede translatiefase, dus het linkadres is een echt fysiek adres dat door de
// map naar de partitie van dít slot wijst.
// Wat de kooi van een partitie eist is sinds TOR alleen nog de 2MB-blokkorrel
// van de map (partAlloc houdt die aan). Een TOR-venster beschrijft een
// WILLEKEURIG bereik met een eigen onder- en bovengrens — de macht-van-twee-eis
// kwam van NAPOT, en die vorm is eruit (kern/cage). Wat het oplevert: een job
// van 124MB krijgt 124MB in plaats van dat hij op een niet-bestaande
// 128MB-partitie strandt.

// cageLinkWindow: precies de partitie. Er is hier geen canoniek venster dat
// groter is dan wat de map beschrijft — de tabel legt exact deze partitie op het
// linkadres, dus alles daarbuiten is per definitie ongemapt. Dat houdt de
// entry-check even streng als vóór de map.
func cageLinkWindow(size uint64) uint64 { return size }

func cageLinkBase() uint64 {
	pool := layout.Pool()
	if len(pool) == 0 {
		return 0
	}
	return pool[0].Base
}

// cageQuiesce zet het hart in reset en wacht tot het silicium dat bevestigt.
// Nodig omdat een app hier zijn eigen hart niet kan parkeren: er is geen laag
// onder hem om naartoe te trappen, dus zijn exit-lus draait tot HOP ingrijpt
// (applib/park_riscv64.go). Dezelfde handeling als cageRevoke, andere bedoeling:
// geen kill maar een geplande wisseling van de wacht — de loader is klaar, de
// echte app mag het hart hebben.
//
// Gemeten 30-07: zonder dit weigerde armSlot fase 2 terecht met "core 1 still
// running" en bleef een gestagede app liggen.
func cageQuiesce(core int) {
	if coreStopped(core) {
		return
	}
	if err := board.Current().HartOff(hartOf(core)); err != nil {
		fmt.Printf("HOPOS_CAGE_QUIESCE_FAILED: core %d: %v\n", core, err)
		return
	}
	for range 100 { // ~100ms; een reset is meteen klaar — dit is de wacht ertegen
		if coreStopped(core) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	fmt.Printf("HOPOS_CAGE_QUIESCE_SLOW: core %d blijft draaien na reset\n", core)
}
