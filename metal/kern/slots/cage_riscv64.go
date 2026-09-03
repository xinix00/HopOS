//go:build riscv64

package slots

import (
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/abi/checksum"
	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/cpu/mmode"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/kern/cage"
	"github.com/xinix00/HopOS/metal/v2/kern/cagestub"
)

// De RISC-V64-helft van de kooi-naad (zie cage_arm64.go voor de andere).
// Zelfde vier beloftes, ander mechanisme — en dat is silicium, geen keuze: de
// XuanTie C906 heeft geen H-extensie, dus er ís geen stage-2. De kooi is hier
// een **PMP-whitelist** (kern/cage) die de slot-stub programmeert en
// verifieert vóór hij de app binnenspringt.
//
// Twee dingen verschillen zichtbaar van ARM, en beide zijn een vereenvoudiging:
//
//   - **Een hart is in reset of hij draait.** Elke start is dan een "koude"
//     start: reset asserten, boot-vector zetten, deasserten. Precies dát wist
//     ook de PMP-locks van het vorige slot, dus een start levert per definitie
//     een schoon slot op — waar ARM daar een expliciete revoke + TLBI voor
//     nodig heeft.
//
//     BEHALVE op een core die HOP niet kan resetten. Sinds HOP naar de kleine
//     core verhuisde (de loterij, board/licheerv/hop/cpuinit_riscv64.s) is de app-core precies de core die
//     HOP verlaten heeft, en daar draait onze switcher permanent: hij komt nooit
//     uit reset, want hij is er nooit in geweest. Voor zo'n core lijkt deze
//     helft op de ARM-helft — starten is een boot-pending die de rotatie oppikt,
//     intrekken is een woord dat de kill-tick uitvoert, en "draait hij?" staat in
//     SchedCurrent in plaats van in het reset-blok. coreParks() is de scheiding,
//     en alle drie die beslissingen hangen eraan.
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
// switchMagic/descriptor-indeling: identiek aan de ARM-kant (kern/stage2), en
// met opzet — het is dezelfde vraag ("draait er code van een vórige kern in
// deze plan-regio, en is het de mijne?"), dus hetzelfde antwoord in dezelfde
// bytes. Alleen het aantal blobs verschilt: hier één (mentry..mmodeEnd).
const (
	switchMagic = 0x3143545753504F48 // "HOPSWTC1"
	swLen       = 8
	swHash      = 16
	swEntry     = 24
	swPark      = 32
	swHead      = 0x40
)

// mmodeAdopting is de adoptie-stand van de kern-flip (docs/kern-flip.md): er
// draait al een switcher in de plan-regio, dus cageInit moet alles wat een
// app-hart aanraakt met rust laten. Gezet door kern/kernflip vóór de eerste
// EnsureVectors; ingetrokken als de zittende kopie niet de onze blijkt.
var mmodeAdopting bool

// flipCapable: zie stage2 op ARM — zonder dit blijft de M-mode-switcher in het
// kern-image en is het gedrag identiek aan vóór de kern-flip.
var flipCapable bool

// SetAdopting/SwitchCodeHash: dezelfde naad als stage2 op ARM — kern/kernflip
// leest ze om te weten of een flip met levende bewoners mag.
func SetAdopting(v bool) { mmodeAdopting = v }

// SwitchCodeHash geeft de som die in de descriptor van de geïnstalleerde kopie
// staat (0 = geen geldige kopie in de plan-regio).
func SwitchCodeHash() uint64 {
	base := layout.SwitchCodePA()
	if dev.Read64(base) != switchMagic {
		return 0
	}
	return dev.Read64(base + swHash)
}

// cageSetFlipCapable: zie de ARM-helft. Hier beslist het of de M-mode-switcher
// naar de plan-regio verhuist.
func cageSetFlipCapable(v bool) { flipCapable = v }

// cageAdoptable: mag de slot-laag de bewoners van de vórige kern overnemen?
func cageAdoptable() bool { return mmodeAdopting }

// installSwitchCode kopieert de M-mode-blob (mentry..mmodeEnd — de trap-entry,
// de rotatie, de parkeerlus) uit het kern-image naar de plan-regio en schakelt
// de mmode-accessors om. Vanaf hier draait een app-hart nooit meer in
// kern-image-bytes, en kan een kern-flip dus zijn eigen venster verlaten
// terwijl dat hart in de switcher staat.
//
// De blob is positie-onafhankelijk (AUIPC/JAL, gemeten 01-09), dus de kopie
// kost geen enkele patch. Cache-contract: HOP schrijft via dev.Copy en veegt
// met CleanInv, zodat de bytes in DRAM staan; het app-hart komt uit reset (lege
// I-cache) en fetcht ze daar. Bij een ADOPTIE wordt er niets gekopieerd — dan
// staat het hart er middenin.
func installSwitchCode() {
	if !flipCapable && !mmodeAdopting {
		return // niet flip-baar: de switcher blijft in het image (oud gedrag)
	}
	start, end := mmode.ImageBlob()
	if end <= start || end-start > mmode.MaxBlobSize {
		panic(fmt.Sprintf("cage: M-mode-blob heeft onmogelijke maat %#x..%#x — eindmarker verschoven?", start, end))
	}
	base := layout.SwitchCodePA()
	blob := make([]byte, end-start)
	dev.CopyOut(blob, uintptr(start))
	sum := checksum.FNV64(blob)

	if mmodeAdopting {
		if dev.Read64(base) != switchMagic || dev.Read64(base+swHash) != sum {
			// Zelfde afweging als op ARM (kern/stage2): we zijn al gesprongen en
			// een kern zonder werkende switcher kan niets, dus installeren we
			// alsnog vers en geven we de bewoners op — kern/slots ziet dat via
			// cageAdoptable en laat hun partities los.
			fmt.Printf("HOPOS_FLIP_SWITCHCODE_MISMATCH: resident M-mode code (%#x) is not ours (%#x) — refusing to adopt residents\n",
				dev.Read64(base+swHash), sum)
			mmodeAdopting = false
		} else {
			b := uint64(base)
			mmode.SetRelocated(b+dev.Read64(base+swEntry), b+dev.Read64(base+swPark))
			return
		}
	}

	if uint64(swHead)+uint64(len(blob)) > uint64(layout.SwitchCodeMax) {
		panic(fmt.Sprintf("cage: M-mode-kopie past niet (%#x > %#x) — blok vol", swHead+len(blob), layout.SwitchCodeMax))
	}
	dev.Copy(base+swHead, blob)
	dev.Write64(base+swLen, uint64(swHead+len(blob)))
	dev.Write64(base+swHash, sum)
	dev.Write64(base+swEntry, uint64(swHead))
	// parkenter ligt op zijn eigen offset binnen de blob; die verschuift mee.
	dev.Write64(base+swPark, uint64(swHead)+(parkEnterImage()-start))
	dev.Write64(base+0, switchMagic) // magic als laatste: half is nooit geldig
	dev.Push(base, uintptr(swHead+len(blob)))
	mmode.SetRelocated(uint64(base)+uint64(swHead), uint64(base)+dev.Read64(base+swPark))
}

// parkEnterImage is het image-adres van parkenter — de offset binnen de blob
// die de kopie moet meenemen.
func parkEnterImage() uint64 { return mmode.ParkEnterPC() }

func cageInit() {
	// Eerst de switcher de plan-regio in (docs/kern-flip.md): daarna geven de
	// mmode-accessors de kopie-adressen, zodat élke stub-tabel (stubTrapPC) en
	// élke park-adoptie (HartOn) buiten het kern-image wijzen.
	installSwitchCode()
	// Draait er al een switcher met bewoners (geadopteerde flip), dan is alles
	// hieronder al gevuld — en zou het wissen ervan precies de levende rotatie
	// slopen die we juist willen sparen.
	if mmodeAdopting {
		return
	}
	base := layout.CageTablePA(0) // = Plan.CagePA; slot 0 draagt geen tabellen
	nc := layout.NumAppCores()
	for c := 0; c <= nc; c++ {
		mb := layout.ParkMboxPA(c)
		// De héle mailbox nullen, óók op een parkerende core. Dat mag hier
		// precies omdat er op dit moment geen switcher draait: sinds de
		// boot-hart-loterij (16-08) hangt zo'n core in zijn cpuinit-lus —
		// caches uit, en hij leest alleen de boot-scratch, nooit deze
		// mailbox. De switcher komt er pas ná de adoptie (adoptParked), en
		// die gebeurt ná deze init (vectorsOnce loopt vóór de eerste
		// dispatch). Zonder deze clear las coreRunning DRAM-vuil als
		// SchedCurrent en koos élke plaatsing het rotatie-pad naar een
		// rotatie die niet bestond — gemeten 17-08, eerste loterij-boot:
		// "sacrificing resident slot -4554780200217279403" en quarantaine.
		dev.Clear(mb, layout.ParkMboxLen)
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
		// daar draait geen switcher, en hartOf(0) bestaat niet — dat zou HOP zijn
		// eigen mtimecmp in een sched-blok schrijven en dat is precies het soort
		// adres dat je niet per ongeluk wilt laten rondslingeren.
		if c == 0 {
			dev.Push(mb, layout.ParkMboxLen)
			continue
		}
		t := hartTimer(physCore(c))
		dev.Write64(mb+layout.SchedClintPA, t.MtimecmpPA)
		dev.Write64(mb+layout.SchedMsipPA, t.MsipPA)
		dev.Write64(mb+layout.SchedSleepCap, t.SleepCap)
		dev.Write64(mb+layout.SchedTickTicks, t.Tick)
		// Een core die niet resetbaar is en geen kill-tick heeft, kan HOP niet
		// meer beëindigen: een bewoner die niet meewerkt blijft er dan tot de
		// watchdog de node omgooit. Dat is geen stand om stil in te belanden.
		if coreParks(c) && (t.MtimecmpPA == 0 || t.Tick == 0) {
			fmt.Printf("HOPOS_CORE_NO_KILL: core %d cannot be reset and has no kill tick — a resident that stops cooperating can only be cleared by rebooting the node\n", c)
		}
		dev.Push(mb, layout.ParkMboxLen)
	}
	// De ctx-blokken van álle slots vers nullen, om dezelfde reden als de
	// mailbox hierboven: een verse boot moet zijn waarheid in DRAM zelf
	// vestigen. Reboots laten restanten achter — gemeten 17-08 (boot 3):
	// slot 1 droeg CtxRunning uit een vórige boot en élke plaatsing weigerde
	// met "still live". De reset-wereld wiste dit impliciet; de loterij niet.
	for i := 1; i <= layout.MaxSlots; i++ {
		dev.Clear(layout.CageTablePA(i)+layout.CtxOff, layout.CtxLen)
		dev.Push(layout.CageTablePA(i)+layout.CtxOff, layout.CtxLen)
	}

	// De parkerende cores meteen intrekken: de switcher zelf als eerste
	// binnenkomer (cpu/mmode parkenter), nu het sched-blok net geprimed is.
	// Dan draait de rotatie daar vanaf de boot en is er nog maar ÉÉN
	// parkeerwereld: een plaatsing is altijd de boot-pending-route, het koude
	// pad bestaat voor zo'n core niet meer, en de tick/slaap-knoppen van de
	// park gelden meteen. De tweede wereld (loterij-park tot de eerste
	// plaatsing) kostte precies de uitzonderingen die boot 7/8 (17-08) de das
	// omdeden: een ongeprimed SchedCurrent en een dispatch-guard die de
	// verkeerde vraag stelde (Derek-review 18-08).
	for c := 1; c <= nc; c++ {
		if !coreParks(c) {
			continue
		}
		if err := cores().Start(physCore(c), mmode.ParkEnterPC(), uint64(layout.ParkMboxPA(c))); err != nil {
			// Zelfredding (de loterij kwam nooit tot parkeren) of een dubbele
			// init: luid melden; plaatsingen op deze core falen daarna met
			// hun eigen foutmelding.
			fmt.Printf("HOPOS_PARK_ADOPT_FAILED: core %d: %v\n", c, err)
		}
	}
}

// cagePrepare rekent de kooi van slot i uit en schrijft haar in de slot-tabel
// van het image dat al in de partitie staat. De app krijgt precies één venster
// — zijn eigen partitie — plus eventueel de granted MMIO; de deny-all die
// cage.Encode erachter zet dekt al het andere, inclusief HOP.
//
// De whitelist bevat zijn app-partitie en precies zijn eigen aaneengesloten
// buffer-slice. Vaste IPA's maken de app-kant uniform; de PMP bindt de fysieke
// kant zodat een slot geen control- of ringpagina van een buur kan benoemen.
//
// De rekenkunde staat in kern/cage (host-getest tegen de op ijzer gemeten
// waarden); hier gaat hij alleen het geheugen in. De stub neemt geen enkele
// beslissing: hij schrijft weg wat hier staat en verifieert het.
func cagePrepare(i int, linkBase, base, size, entry, ctrlPA, queueSize uint64) error {
	// Een intrekking van de VORIGE bewoner van dit slot hoort niet aan de
	// volgende te blijven plakken. Dit woord overleeft een slot-wissel (het staat
	// in het ctx-blok, niet in de partitie), en de kill-tick leest het bij élke
	// tick — dus zonder deze regel zou een verse app op een eerder ingetrokken
	// slot binnen 10ms weer omvallen, zonder fault en zonder reden.
	//
	// Hier en niet bij het intrekken zelf: op het moment van intrekken moet het
	// woord juist blíjven staan tot de switcher hem gezien heeft, en HOP weet
	// niet wanneer dat is. "Schoon bij de start" is de toestand die telt.
	ctxWrite(i, layout.CtxRevoke, 0)

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
		{Base: ctrlPA, Size: uint64(layout.SlotControlStride) + 2*queueSize, R: true, W: true},
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
	// Scratch in het eigen control-blok uit de systeempot: ná het locken van de
	// kooi staat precies deze slice naast de app-partitie op de whitelist.
	put(stubScratch, ctrlPA+layout.AbiStubOff)
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
	root, err := slotMap(i, linkBase, base, size, ctrlPA, queueSize)
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
	// Eén call over de hele partitie plus zijn kleine buffer-slice.
	dev.CleanInv(uintptr(base), uintptr(size))
	dev.CleanInv(uintptr(ctrlPA), uintptr(layout.SlotControlStride+2*queueSize))
	dev.MB()
	return nil
}

// slotMap zet de map-helft in het control-blok uit de systeempot en geeft de
// wortel terug die de stub in zijn map-register schrijft. Drie vensters:
//
//   - **app-RAM, cachebaar** — het canonieke linkadres → de echte partitie. Dit
//     is waar verplaatsen voor bestaat: élk slot ziet zichzelf op hetzelfde adres.
//   - **control, device** — control page, bootstrap-ringen en kooi-metadata;
//   - **TX/RX, device** — de virtuele NIC uit dezelfde fysieke slice.
//
// De stub heeft zelf géén venster nodig: satp vertaalt alleen supervisor- en
// user-mode, en hij draait in machine mode — dus fetcht hij ongetranslateerd, ook
// ná zijn eigen csrw satp.
func slotMap(i int, linkBase, base, size, ctrlPA, queueSize uint64) (uint64, error) {
	netPA := ctrlPA + uint64(layout.SlotControlStride)
	windows := []cage.MapWindow{
		{Link: linkBase, Phys: base, Size: size, R: true, W: true, X: true},
		{Link: uint64(layout.SlotControl(i)), Phys: ctrlPA, Size: layout.SlotControlStride,
			R: true, W: true, Device: true},
		{Link: uint64(layout.NetQueueTX(i)), Phys: netPA, Size: queueSize,
			R: true, W: true, Device: true},
		{Link: uint64(layout.NetQueueRX(i)), Phys: netPA + queueSize, Size: queueSize,
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

	tbl := uintptr(ctrlPA + layout.AbiMapOff)
	m, err := cage.Relocate(cage.MapPlan{TableBase: uint64(tbl), Windows: windows})
	if err != nil {
		return 0, err
	}
	if len(m.Bytes) > layout.SlotControlStride-layout.AbiMapOff {
		return 0, fmt.Errorf("kooi-map vraagt %d KiB, control-blok heeft vanaf AbiMapOff %d KiB",
			len(m.Bytes)>>10, (layout.SlotControlStride-layout.AbiMapOff)>>10)
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

// coreParks: deze core komt nooit uit reset, want er draait onze eigen switcher
// op. Dat is de core die HOP verlaten heeft via de boot-hart-loterij
// (board/licheerv/hop/cpuinit_riscv64.s) — voor hem bestaat het reset-blok-antwoord niet.
//
// Eén predicaat en niet drie losse checks, want er hangen drie beslissingen aan
// en die moeten dezelfde kant op kiezen: hoe een slot start (boot-pending in
// plaats van een hart-reset), hoe hij dood gaat (kill-tick in plaats van
// HartOff), en of HOP in regel 0 van het sched-blok mag schrijven (nooit).
func coreParks(core int) bool {
	h := physCore(core)
	return h >= 0 && !cores().CanReset(h)
}

// De core-levenscyclus is hier normaal gesproken het reset-blok, niet een
// mailbox: een hart draait of staat in reset. Dat is meteen het antwoord op
// "draait hij?" en "staat hij stil?" — géén afspraak in geheugen die stale kan
// worden, maar het silicium zelf. (Een mailbox-woord uitlezen meldde een gestopt
// hart als draaiend zodra HOP het ooit geschreven had — gemeten 30-07 op het
// bordje: elke job-retry viel dan in het gedeelde-core-pad.)
//
// Voor een parkerende core kán dat niet: die staat altijd aan. Daar is
// SchedCurrent het antwoord, en dat woord is wél betrouwbaar om precies de reden
// dat het vorige mailbox-woord dat niet was — de SWITCHER schrijft het, niet HOP,
// en hij schrijft het bij élke overgang (park zet hem op nul, boot/resume op het
// slot). Zoals ARM het al deed, alleen ligt de waarheid daar in een ander woord.
func coreRunning(core int) bool {
	if coreParks(core) {
		return schedCurrent(core) != 0
	}
	return cores().State(physCore(core)) == board.PowerOn
}

func coreStopped(core int) bool {
	if coreParks(core) {
		return schedCurrent(core) == 0
	}
	return cores().State(physCore(core)) == board.PowerOff
}

// schedCurrent leest welk slot er op deze core draait (0 = geen). De switcher
// schrijft dit vanaf een ander hart met zijn eigen cache, dus invalideren vóór
// het lezen — zonder de Pull leest HOP eeuwig de stand van zijn eerste blik.
func schedCurrent(core int) uint64 {
	p := layout.ParkMboxPA(core) + layout.SchedCurrent
	dev.Pull(p, 8)
	return dev.Read64(p)
}

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

// stubScratchAt geeft de fysieke scratch uit de systeempot. De partitiecheck
// houdt een vrij slot weg van de gedeelde slice.
func stubScratchAt(i int) uintptr {
	ctrl, _, _, err := slotBuffers(i)
	if err != nil {
		return 0
	}
	return ctrl + layout.AbiStubOff
}

// cageIdent vertelt wat voor hart er onder slot i zit: misa/marchid/mimpid,
// door de kooi-stub in zijn scratch gelegd (stap 1b). Per-hart-CSR's, dus HOP
// op hart 0 kan ze niet zelf lezen — en het antwoord beslist of dit board ooit
// meerdere apps kan dragen: zonder S-bit in misa bestaat er geen modus onder
// M-mode om een app in te zetten, en dan kan HOP dit hart niet preempten.
//
// De extensieletters komen uit de onderste 26 bits van misa (bit 0 = 'a').
func cageIdent(i int) string {
	_, _, ok := partitionOf(i)
	if !ok {
		return ""
	}
	sc := stubScratchAt(i)
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
	_, _, ok := partitionOf(i)
	if !ok {
		return ""
	}
	sc := stubScratchAt(i)
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

// De kern moet de ÉCHTE cache-ops hebben. Deze package linkt nooit in een
// app-image, dus als dev hier de app-no-ops levert (RealCacheOps = 0) is dat
// de buildtag-drift van 17-08 — vijf boot-cycli jacht omdat élke Push/Pull
// stil een no-op werd. Dan onderstaande constante negatief en weigert de
// compiler: de bug bestaat niet meer als artefact, alleen nog als bouwfout.
const _ uint = dev.RealCacheOps - 1

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
	hart := physCore(core)
	if hart < 0 {
		return fmt.Errorf("cage: no hart for logical core %d", core)
	}
	return cores().Start(hart, entry, 0) // een reset-start kent geen arg
}

// cageRevoke: hart in reset. Dat stopt hem waar hij ook is (ook uit een tight
// loop) én wist zijn PMP-locks — de hard-kill en het schone slot in één.
//
// Op een GEDEELDE core is de reset een moker, geen scalpel: hij velt ook de
// switcher en élke medebewoner. Zonder nasleep loog de boekhouding daarna
// (gemeten 14/15-08): de dode switcher kon CtxDead niet meer melden — de
// kill eindigde in "not dead after revocation"-quarantaine — en de stale
// ctx-staten lieten elke verse boot van de core spoken hervatten. De nasleep
// (coreOrphansDead) schrijft die waarheid zelf; de taak-monitors herstarten
// de onschuldige buren (HOP-leven-filosofie: apps zijn herstartbaar).
// cageRevoke: de intrekking, in twee vormen — en welke het is, is een
// eigenschap van de CORE, niet van het slot.
//
//   - Parkerende core (coreParks: geen reset-recept, onze switcher draait er):
//     één woord in het ctx-blok en de kill-tick van de switcher (cpu/mmode,
//     elke 10ms) voert het uit. Fijner dan een reset: hij velt precies deze
//     bewoner en de buren op dezelfde core blijven staan.
//   - Resetbaar hart: HartOff — stopt hem waar hij ook is (ook een tight
//     loop) én wist de PMP-locks. Grof: de hele core gaat uit, dus mét de
//     nasleep (coreOrphansDead, share.go) die de medebewoners eerlijk dood
//     meldt — de gemeten les van 14/15-08.
func cageRevoke(i int) {
	core := coreOf(i)
	if coreParks(core) {
		ctxWrite(i, layout.CtxRevoke, 1)
		return
	}
	if err := coreReset(core); err != nil {
		fmt.Printf("HOPOS_CAGE_REVOKE_FAILED: slot %d: %v\n", i, err)
		return
	}
	coreOrphansDead(core)
}

// cageForceYield wint een core terug van een bewoner die nooit yieldt. Op een
// parkerende core is de kill-tick de chirurgische vorm; op een resetbaar hart
// is de reset (met nasleep) de enige knop.
func cageForceYield(core, hog int) {
	if coreParks(core) {
		if hog >= 1 && hog <= layout.MaxSlots {
			ctxWrite(hog, layout.CtxRevoke, 1)
			return
		}
		fmt.Printf("HOPOS_CORE_RECLAIM_FAILED: core %d holds no attributable resident\n", core)
		return
	}
	if err := coreReset(core); err != nil {
		fmt.Printf("HOPOS_CORE_RECLAIM_FAILED: core %d: %v\n", core, err)
		return
	}
	coreOrphansDead(core)
	_ = hog // attributie zit in de log van de aanroeper
}

// coreReset is de Reset-poot van het board voor een logische core — de
// hard-kill op een resetbaar hart. Een board zonder die poot, of een hart
// zonder recept, meldt dat als fout (de aanroeper koos dan al het kill-tick-
// pad via coreParks; dit is de vangrail, niet de route).
func coreReset(core int) error {
	k := cores()
	h := physCore(core)
	if h < 0 || !k.CanReset(h) {
		return fmt.Errorf("core %d cannot be reset on this board — revocation goes through the kill tick", core)
	}
	return k.Reset(h)
}

// hartTimer is de comparator-afspraak van het board voor een hart
// (board.HartTimerer, optioneel): zonder blijft het "niets" — de parkeerlus
// spint en er is geen kill-tick, en dat is de veilige stand.
func hartTimer(hart int) board.HartTimer {
	if t, ok := board.Current().(board.HartTimerer); ok {
		return t.HartTimer(hart)
	}
	return board.HartTimer{}
}

// cageSMPEntryPC: geen SMP op dit board. Er is één app-hart, dus een slot heeft
// nooit een tweede core om binnen te laten komen. HOP publiceert 0 en de
// app-OS-laag vraagt er niets bij (CtrlCores blijft 1).
func cageSMPEntryPC() uint64 { return 0 }

// De vertaling logisch → fysiek hart (hartOf) is generiek geworden: physCore
// in slots.go, uit Cores().App van het board. Zie het contract in cage.go.

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
