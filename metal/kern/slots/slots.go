// Package slots is HopOS' slot-manager: de primitieven waarop HOP's
// HopRunner straks 1-op-1 aansluit (Runner.Run/Stop/Status).
//
//   - Start:  laad een gesigneerde app-image in de slot-partitie, patch de
//     RAM-declaratie naar job.MemoryLimit, en wek de core via PSCI.
//   - Stop:   coöperatieve kill via de control-page (de app-lib zet de core
//     zelf uit met PSCI CPU_OFF); wacht tot de core echt uit is.
//   - Status: powertoestand (PSCI AFFINITY_INFO) + app-status + heartbeat
//     uit de control-page (hang-detectie).
//
// Restart = Stop + Start: de image wordt altijd vers geladen, dus elke start
// is een schone lei — consistent met "niets is persistent".
package slots

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"

	"hop-os/metal/abi/layout"
	"hop-os/metal/abi/place"
	"hop-os/metal/abi/ring"
	"hop-os/metal/board"
	"hop-os/metal/dev"
	"hop-os/metal/kern/apploaderblob"
	"hop-os/metal/net/hopswitch"
)

// Eén servicer per slot: de outbox is SPSC, dus er mag nooit meer dan één
// consumer leven. De servicer draint logs (TypeLog) én bedient hop-ABI-RPC's
// (TypeRPCReq → fs/fetch → TypeRPCResp op de inbox). Start verdringt de oude
// servicer synchroon vóór de ringen opnieuw geïnitialiseerd worden — anders
// kan bij een snelle Stop→Start een oude naast de nieuwe blijven lezen (twee
// schrijvers op tail). Alles draait op de HOP-kern: Go-synchronisatie volstaat.
type servicer struct {
	slot int
	stop chan struct{} // gesloten: servicer moet weg
	done chan struct{} // gesloten zodra de servicer weg is

	sawLive   bool        // ctx ooit levend gezien (dan is niet-levend = einde)
	idleStart time.Time   // eerste lege pass zonder levende ctx (start-gratie)
	logs      chan string // logregels (drop bij trage lezer)
	root      string      // eigen (lege) hopfs-root van deze task
	job       string      // job-naam = de store-naamruimte ("" = geen store)
	mounts    [][2]string // {local, shared}, langste local eerst

	// De levenslijn van de store-ops (storage.go): evict cancelt hem, zodat
	// een servicer die minutenlang in een S3-transfer hangt een Stop→Start
	// nooit ophoudt — de transfer breekt af en de run-lus ziet stop.
	ctx    context.Context
	cancel context.CancelFunc
}

var (
	svcMu     sync.Mutex
	servicers = map[int]*servicer{}
)

// De slot-lifecycle (Start/Stop) is GESERIALISEERD en draait in een
// DMA-stil venster — generieke semantiek, geen board-paadje: een task start
// liever een fractie trager maar schoon in zijn eigen huisje. De fabric-brede
// operaties van een lifecycle (imagecopy, stage-2-CleanInv, heap-zeroing en
// TLBI's van een bootende of parkerende core) lopen zo nooit gelijktijdig
// met elkaar óf met inbound netwerk-DMA. Aanleiding: het BCM2712-C1-erratum
// (gemeten 2026-07-13) — maar safe-by-default is het ontwerp; silicium dat
// zelfs dít niet trekt kopen we niet. quiesce() werkt via board.NetQuiescer
// (optioneel): boards zonder stilzetbare NIC hebben géén venster nodig.
var (
	lifecycleMu   sync.Mutex
	lastLifecycle time.Time // voor de adempauze (board.LifecyclePacer)
)

func quiesce(off bool) {
	if q, ok := board.Current().(board.NetQuiescer); ok {
		q.NetQuiesce(off)
	}
}

// drain laat na het sluiten van het venster de in-flight DMA landen: RX uit
// stopt níeuwe transacties, maar posted writes die al in de pijp zitten
// (NIC→fabric→DRAM) landen vlak daarna nog. Twee milliseconden is ruim voor
// elke pijpdiepte — generieke silicium-hygiëne, geen board-specifiek pad.
func drain() { time.Sleep(2 * time.Millisecond) }

// coopSched meldt of de node-runtime tijdens een plaatsing coöperatief mag
// afgeven — grote geheugenops in brokken met een runtime.Gosched ertussen.
// WAAR op boards zónder DMA-stil venster (Altra/QEMU): daar draait de hele node
// op één core (GOMAXPROCS=1), dus een ononderbroken asm-veeg over de hele
// partitie (96MB × 127 loaders ≈ 12s gemeten) verhongert de netstack, /health,
// de switch en de heartbeat. Afgeven ís op één core de concurrency (het Go-idee).
// ONWAAR op een NetQuiescer (Pi, C1-erratum): dat houdt zijn strikte,
// ononderbroken venster — en doet ook geen 127-plaatsings-storm. Eén keer
// bepaald; het board wisselt niet na boot.
var (
	coopSchedOnce sync.Once
	coopSchedVal  bool
)

func coopSched() bool {
	coopSchedOnce.Do(func() {
		_, isQuiescer := board.Current().(board.NetQuiescer)
		coopSchedVal = !isQuiescer
	})
	return coopSchedVal
}

// coopCleanInv veegt [addr,addr+size) net als dev.CleanInv, maar op coöperatieve
// boards (coopSched) in brokken van 4MB met een yield ertussen: zo blijft core 0
// tijdens een plaatsings-storm de netstack/health/switch bedienen i.p.v. één
// ononderbroken veeg. Zelfde bytes, alleen coöperatief. Aanroepen wanneer het
// slot niet aan de switch hangt (de partitie wordt zo meteen toch overschreven).
func coopCleanInv(addr, size uintptr) {
	if !coopSched() {
		dev.CleanInv(addr, size)
		return
	}
	const chunk = 4 << 20
	for size > 0 {
		n := size
		if n > chunk {
			n = chunk
		}
		dev.CleanInv(addr, n)
		addr += n
		size -= n
		runtime.Gosched()
	}
}

// pace wacht (onder lifecycleMu, mét RX aan) tot de board-adempauze sinds de
// vorige lifecycle verstreken is, en stempelt het nieuwe beginmoment.
func pace() {
	if p, ok := board.Current().(board.LifecyclePacer); ok {
		if d := p.LifecyclePace() - time.Since(lastLifecycle); d > 0 {
			time.Sleep(d)
		}
	}
	lastLifecycle = time.Now()
}

// lifecycleWindow opent het DMA-stille lifecycle-venster: geserialiseerd
// (lifecycleMu), gepaced, NIC gequiesced en de in-flight DMA gedraineerd. De
// teruggegeven closer heropent in omgekeerde volgorde — gebruik als
//
//	defer lifecycleWindow()()
//
// zodat het venster op élk pad (ook errors) weer opent. Eén definitie voor
// Start, StartStaged en Stop: het trio lock+pace+quiesce+drain kan niet meer
// per pad uit de pas lopen.
func lifecycleWindow() func() {
	lifecycleMu.Lock()
	pace()
	quiesce(true)
	drain()
	return func() {
		quiesce(false)
		lifecycleMu.Unlock()
	}
}

// prepStart valideert de pure job-invoer van een slot-start — alles wat geen
// lock of stille hardware nodig heeft — VÓÓR het lifecycle-venster: een
// kapotte job opent het venster nooit, en het DMA-stille venster zelf blijft
// zo kort mogelijk. Eén definitie voor Start én StartStaged (het was ~45
// regels letterlijke duplicatie op een ABI-kritisch pad). Geeft de
// mount-tabel, de env-blob en het genormaliseerde core-aantal terug.
func prepStart(i int, memLimit uint64, cores int, env map[string]string, mounts map[string]string, ports map[string]int) (mtab [][2]string, envBlob []byte, coresOut int, err error) {
	if err := checkSlot(i); err != nil {
		return nil, nil, 0, err
	}
	for name, p := range ports {
		if p < 1 || p > 65535 {
			return nil, nil, 0, fmt.Errorf("poort %q: %d ongeldig", name, p)
		}
	}
	if mtab, err = mountTable(mounts); err != nil {
		return nil, nil, 0, err
	}
	if memLimit == 0 {
		return nil, nil, 0, fmt.Errorf("memLimit 0 ongeldig")
	}
	// SMP (fase 5): cores ≥ 1. cores > 1 = één app over meerdere cores met
	// een gedeelde heap op de partitie van dít slot; de OS-laag vraagt de
	// extra cores lazy op (goos.Task → CtrlSMPReq → HOP dispatcht).
	if cores < 1 {
		cores = 1
	}
	// SMP-apps (cores>1) zijn dedicated: ze pakken cores i..i+cores-1, dus die
	// moeten binnen de fysieke app-cores vallen. Een cores=1-app mag op elke
	// kooi (die kan een gedeelde core zijn, ver boven NumAppCores) — checkSlot
	// bewaakt daar de kooi-grens (MaxSlots).
	//
	// De aanname "SMP ⇒ slot == core" staat elders in dit bestand als commentaar
	// (Stop's kill-scan rekent op core..core+n-1) maar werd nergens afgedwongen.
	// Voor een pool-geplaatste kooi (coreOf(i) != i) liep dat stil uit de pas:
	// smp.Configure leidt de secundaire cores af van de ÉCHTE core (coreOf(i)),
	// terwijl dispatchSMP ze tegen slot i toetste — dan wordt elke lazy
	// SMP-aanvraag geweigerd en degradeert de app stilletjes naar één core.
	// Liever hier hard falen: SMP hoort op zijn eigen core te wonen.
	if cores > 1 && coreOf(i) != i {
		return nil, nil, 0, fmt.Errorf("SMP: kooi %d woont op core %d (gedeelde/pool-plaatsing) — %d cores vraagt een eigen core (slot == core)",
			i, coreOf(i), cores)
	}
	if cores > 1 && i+cores-1 > layout.NumAppCores() {
		return nil, nil, 0, fmt.Errorf("SMP: %d cores vanaf slot %d overschrijden de %d app-cores", cores, i, layout.NumAppCores())
	}
	// DeviceGrant-haak (grants.go; gui/fbgrant is de eerste provider): een
	// job met FB=1 krijgt — als het board een framebuffer heeft en niemand
	// hem houdt — de FB_*-beschrijving in zijn env; de kooi-mapping volgt in
	// armSlot (na stage2.Build).
	env = grantEnv(i, env)
	// DNS-resolver van de node meegeven, zodat een app die naar buiten praat
	// (cloudflared, servers) namen kan opzoeken — de query loopt als gewoon
	// UDP door de masquerade. HOP zet 'm als env (net als ER_PORT_*), tenzij
	// de job 'm al expliciet koos. Leeg (Pi vóór P2) = geen HOP_DNS.
	envBlob = encodeEnv(withDNS(env, board.Current().Net().DNS))
	if len(envBlob) > layout.CtrlEnvMax {
		return nil, nil, 0, fmt.Errorf("env te groot: %d > %d bytes", len(envBlob), layout.CtrlEnvMax)
	}
	return mtab, envBlob, cores, nil
}

// coresFree bewaakt (ín het venster) dat de cores van het slot niet draaien —
// geparkeerd of cold mag: dat is precies een core die HOP kan (her)starten.
func coresFree(i, cores int, why string) error {
	for c := i; c < i+cores; c++ {
		if coreRunning(c) {
			return fmt.Errorf("core %d still running (%s)", c, why)
		}
	}
	return nil
}

// evictServicer stopt de actieve servicer van slot i en wacht tot hij weg is.
func evictServicer(i int) {
	svcMu.Lock()
	old := servicers[i]
	delete(servicers, i)
	svcMu.Unlock()
	if old != nil {
		// Eerst de levenslijn kappen: hangt de servicer in een store-op
		// (S3-transfer), dan breekt die meteen af — anders wachtte <-old.done
		// hier tot de volle transfer-timeout, mét lifecycleMu in de hand.
		old.cancel()
		close(old.stop)
		<-old.done
	}
}

// registerServicer registreert de nieuwe servicer van slot i (nog niet
// gestart — placeFromStaging start hem ná de ring-init). Verdringen hoeft
// hier niet meer: evictServicer draait altijd eerder op hetzelfde pad, onder
// dezelfde lifecycleMu, en registratie gebeurt nérgens anders — er kán dus
// geen oude servicer meer staan.
func registerServicer(i int, root, job string, mounts [][2]string) *servicer {
	ctx, cancel := context.WithCancel(context.Background())
	s := &servicer{
		slot:   i,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		logs:   make(chan string, 64),
		root:   root,
		job:    job,
		mounts: mounts,
		ctx:    ctx,
		cancel: cancel,
	}
	svcMu.Lock()
	servicers[i] = s
	svcMu.Unlock()
	return s
}

// run is de servicer-lus: outbox lezen, logs doorzetten, RPC's afhandelen.
// Stopt bij evict, corrupte ring of core-off (met lege ring).
func (s *servicer) run() {
	defer close(s.done)
	defer close(s.logs)
	defer s.cancel() // ook bij een natuurlijke dood (ring corrupt, ctx weg)
	// Diepteverdediging: één servicer-panic (een bug in handle/fs/fetch, of een
	// onverwachte record-inhoud) mag core 0 — en dus álle andere slots — niet
	// vellen. Recover, log zichtbaar, en laat alléén deze goroutine sterven;
	// het slot kan herstart worden. Dit dekt geen validatie af (die hoort bij
	// de bron), het begrenst de blast-radius. Deze defer staat als laatste
	// geregistreerd → draait als eerste bij het afwikkelen, vóór de closes.
	defer func() {
		if r := recover(); r != nil {
			fmt.Printf("HOPOS_SERVICER_PANIC slot %d: %v\n", s.slot, r)
		}
	}()
	ram, ramSize, ok := abiOf(s.slot)
	if !ok {
		fmt.Printf("HOPOS_SERVICER_NO_PARTITION slot %d\n", s.slot)
		return
	}
	out := ring.Open(layout.RingOutboxAt(ram, ramSize))
	in := ring.Open(layout.RingInboxAt(ram, ramSize))
	// Eén hergebruikte leesbuffer i.p.v. een allocatie per record: de payload
	// wordt synchroon verwerkt (log → string-kopie; RPC → handle retourneert
	// vóór de volgende lees), dus hergebruik is veilig.
	buf := make([]byte, layout.RingDataCap)
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		// SMP-dispatch: de app-runtime kan geparkeerde cores niet zelf starten
		// (de mailboxen liggen buiten elke stage-2-map), dus vraagt ze via
		// CtrlSMPReq aan; HOP dispatcht namens hem. Onder de app-bootLock, dus
		// hooguit één verzoek tegelijk.
		s.dispatchSMP()
		typ, n, ok := out.ReadInto(buf)
		if !ok {
			// Geen outbox-werk: stoppen zodra het SLOT niet meer leeft (op
			// een gedeelde core zegt de core-mailbox niets over déze
			// bewoner; de ctx-staat dekt beide werelden) of de ring corrupt is.
			if out.Corrupt() {
				return
			}
			// Niet-levende ctx = het slot is weg — máár bij de start is de ctx
			// heel even nog Empty (registratie gebeurt vóór residentReset/de
			// dispatch). Zonder gratie stierf de servicer van fase 2 hier
			// meteen, en verdronk niemand meer de logs van de échte app
			// (gemeten 30-07: welcome's stervensreden bleef in de ring staan).
			if !ctxLive(ctxState(s.slot)) {
				if s.sawLive {
					return
				}
				if s.idleStart.IsZero() {
					s.idleStart = time.Now()
				} else if time.Since(s.idleStart) > 2*time.Second {
					return
				}
			} else {
				if !s.sawLive {
					identOnce(s.slot) // eerste keer dat dit slot leeft
				}
				s.sawLive = true
			}
			select {
			case <-s.stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			continue
		}
		p := buf[:n]
		switch typ {
		case ring.TypeLog:
			// De app mag nooit blokkeren op zijn logs: past de regel niet in het
			// kanaal, dan valt hij weg. Wat níet wegvalt is de láátste regel — die
			// bewaart lastLog voor het post-mortem, en dát was het echte gat
			// (30-07: de apploader meldde netjes waarom hij stopte en niemand kon
			// het terugvinden). Onder diagMu, want de lifecycle leest en wist dit
			// veld terwijl deze servicer erin schrijft.
			line := string(p)
			diagMu.Lock()
			lastLog[s.slot] = line
			diagMu.Unlock()
			select {
			case s.logs <- line:
			default:
			}
		case ring.TypeRPCReq:
			resp := s.handle(p)
			if !in.Fits(len(resp)) {
				// Een respons die nooit in de ring past zou de schrijf-lus
				// hieronder eeuwig laten spinnen (Write weigert 'm blijvend,
				// niet tijdelijk). Handlers begrenzen hun data al; dit is het
				// vangnet dat ook toekomstige ops afdekt.
				resp = oversizeResp(p)
			}
			for !in.Write(ring.TypeRPCResp, resp) {
				select {
				case <-s.stop:
					return
				case <-time.After(time.Millisecond):
				}
			}
		}
	}
}

// dispatchSMP kijkt of de app-runtime een extra SMP-core vroeg (CtrlSMPReq,
// gezet door goos.Task) en dispatcht die namens hem. De app kan het niet zelf:
// de parkeer-mailboxen liggen bewust buiten elke stage-2-map, zodat een app
// nooit een core (van zichzelf of een ander) kan opbrengen — alleen HOP.
// Ctx = de fysieke control-page van de primaire (de SMP-trampoline leest daar
// de M-context, gedeelde stage-2 en VMID van); de secundaire mailbox gaat via
// CtrlSMPMbox mee (de primaire page is gedeeld). Klaar → CtrlSMPReq weer 0,
// waar de app op wacht.
func (s *servicer) dispatchSMP() {
	c := int(ctrlRead(s.slot, layout.CtrlSMPReq))
	if c == 0 {
		return
	}
	// Vertrouwde core-telling uit HOP-geheugen (smpCores), NOOIT ctrlRead
	// (CtrlCores) — die page is app-schrijfbaar; een opgehoogde CtrlCores zou
	// anders een app buurcores in zijn kooi laten trekken. Zie smp.go.
	cores := coreCount(s.slot)
	if c <= s.slot || c > s.slot+cores-1 || c > layout.NumAppCores() {
		// Buiten het toegewezen core-bereik: weiger (de app hoort dit niet te
		// vragen). Verzoek intrekken zodat de app niet eeuwig wacht.
		fmt.Printf("HOPOS_SMP_REJECT slot %d: core %d outside [%d,%d]\n", s.slot, c, s.slot+1, s.slot+cores-1)
		ctrlWrite(s.slot, layout.CtrlSMPReq, 0)
		dev.MB()
		return
	}
	ctrlWrite(s.slot, layout.CtrlSMPMbox, uint64(layout.ParkMboxPA(c)))
	dev.MB()
	cp, ok := CtrlPageOf(s.slot)
	if !ok {
		fmt.Printf("HOPOS_SMP_DISPATCH_FAIL slot %d core %d: slot heeft geen partitie\n", s.slot, c)
		return
	}
	if err := dispatchCore(c, cageSMPEntryPC(), uint64(cp)); err != nil {
		fmt.Printf("HOPOS_SMP_DISPATCH_FAIL slot %d core %d: %v\n", s.slot, c, err)
	}
	ctrlWrite(s.slot, layout.CtrlSMPReq, 0) // app-handshake: verzoek afgehandeld
	dev.MB()
}

// abiOf geeft de basis waarmee de ABI-adressen van slot i te berekenen zijn: de
// partitiebasis en de app-RAM-maat (de staart erboven draagt control page,
// hop-ABI-ringen en frame-ringen — zie layout, de slot-ABI).
//
// Er is dus geen vast plan-adres per slot meer, en dat is precies de bedoeling:
// de ABI van een slot bestaat exact zolang zijn partitie bestaat. ok=false
// betekent letterlijk "dit slot heeft nu geen ABI" — lezen zou vrij DRAM lezen,
// schrijven zou het geheugen van de volgende huurder verminken.
func abiOf(i int) (ram, ramSize uint64, ok bool) {
	base, size, ok := partitionOf(i)
	if !ok {
		return 0, 0, false
	}
	appRAM, err := appRAMSize(size)
	if err != nil {
		return 0, 0, false
	}
	return base, appRAM, true
}

// CtrlPageOf geeft de fysieke control page van app-slot i. Geëxporteerd voor de
// klok-governor (driver/dvfs leest er de idle-teller van): sinds de ABI in de
// partitie woont, is de slotlaag de enige die weet waar die page ligt. De
// bedrading staat in de main (cmd/hopos), niet hier — dit pakket is host-getest
// en driver/dvfs sleept via cpu/idle tamago-only code mee.
func CtrlPageOf(i int) (uintptr, bool) {
	ram, ramSize, ok := abiOf(i)
	if !ok {
		return 0, false
	}
	return layout.CtrlPageAt(ram, ramSize), true
}

// ctrlRead/ctrlWrite: 64-bit velden op de control-page van een slot. HOP-kant,
// dus fysieke adressen; de app leest dezelfde bytes via zijn eigen basis
// (RamStart/RamSize) — op ARM legt de stage-2 die op deze partitie, op RISC-V is
// het letterlijk hetzelfde adres.
//
// Een slot zonder partitie heeft geen control-page: lezen geeft 0 en schrijven
// is een no-op. Dat is geen stille fout maar het contract — élke schrijver
// hieronder draait op een slot dat een task draagt, en een lezer die 0 krijgt
// ziet "geen app" (StatusEmpty is 0).
func ctrlRead(slot int, off uintptr) uint64 {
	cp, ok := CtrlPageOf(slot)
	if !ok {
		return 0
	}
	dev.Pull(cp+off, 8)
	return dev.Read64(cp + off)
}

func ctrlWrite(slot int, off uintptr, v uint64) {
	if cp, ok := CtrlPageOf(slot); ok {
		dev.Write64(cp+off, v)
		dev.Push(cp+off, 8)
	}
}

// Parkeer-model: HopOS bezit zijn cores. Een gestopte/gevelde app-core gaat
// niet terug naar de firmware (PSCI CPU_OFF is op de Pi 5-stock een one-way
// door) maar parkeert op EL2 in de WFE-lus (stage2.InitVectors). De mailbox
// (buiten elke stage-2-map — de app kan zichzelf dus niet dispatchen) is de
// enige bron van waarheid over de core-toestand:
//
//	word0 == 0  cold   — nooit geparkeerd; eerste bring-up gaat via PSCI CPU_ON
//	word0 == 1  parked — gestopt en wachtend op dispatch
//	word0 >= 2  running — word0 draagt de ctx (fysieke ctrl-page) die HOP zette
//
// dispatchCore start een core op entry met ctx in x0. Cold (nooit geparkeerd):
// PSCI CPU_ON — de éénmalige bring-up per core. Anders (geparkeerd): schrijf
// {ctx, entry} in de mailbox en wek de WFE-lus met SEV; die springt de
// (idempotente) trampoline in. Zet word0 sowieso op ctx zodat coreRunning klopt.
// errDispatch markeert dat het startschot zélf faalde. Dat is het enige
// faalpad met een ONBEKENDE uitkomst: dispatchCore primet de core-mailbox
// (word0=ctx, word1=PC) vóór de PSCI CPU_ON, dus bij een fout kán die core
// alsnog aangaan en de trampoline inspringen. Daarom gaat de partitie op dit
// pad fail-closed in quarantaine i.p.v. terug de pool in — zelfde afweging als
// releaseSlot(_, false) bij een onbevestigde intrekking.
var errDispatch = errors.New("dispatch failed")

func dispatchCore(core int, entry, ctx uint64) error {
	// Nooit een core dispatchen die al draait: dat zou een app (of een tweede
	// Start) een core midden in de uitvoering laten kapen. Start's pad checkt dit
	// al vóór de aanroep (coreRunning-lus), maar de lazy-SMP-weg (dispatchSMP,
	// met een app-beïnvloed core-nummer uit CtrlSMPReq) komt hier ook langs — dus
	// de guard hoort hier, op het gedeelde punt. Het startschot zelf is
	// arch-werk (cageDispatch): een parkeerlus wekken is iets anders dan een hart
	// uit reset halen.
	if coreRunning(core) {
		return fmt.Errorf("core %d draait al — dispatch geweigerd", core)
	}
	return cageDispatch(core, entry, ctx)
}

// waitSlotQuiet wacht tot slot i géén werk meer doet — en dat is niet hetzelfde
// als "de core staat stil". Op een architectuur waar de app zich dood MELDT (de
// exit-trap naar HOP's switcher, cpu/mmode) blijft het hart daarna voor zijn
// buren doorlopen; de ctx-staat is dan het teken, niet de core-toestand. Waar de
// app zijn core parkeert (ARM) zegt de core-toestand het, en de ctx-staat volgt.
//
// Beide accepteren, want anders wacht de dedicated Stop-weg op RISC-V áltijd de
// volle timeout vol: de bewoner is al dood terwijl het hart nog draait.
func waitSlotQuiet(i, core int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if coreStopped(core) {
			return true
		}
		if st := ctxState(i); st == layout.CtxDead || st == layout.CtxEmpty {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// waitStopped polt tot core echt stilstaat (geparkeerd of in reset — wat op deze
// architectuur "gestopt" betekent, zie de kooi-naad).
func waitStopped(core int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if coreStopped(core) {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// Status van een slot zoals HOP het ziet.
type Status struct {
	CoreOn    bool
	App       uint64 // layout.Status*-waarde
	ExitCode  uint64
	Heartbeat uint64
	RAMSize   uint64 // door de app gerapporteerde (gepatchte) RAM-maat
	MemSys    uint64 // werkelijke draw: MemStats.Sys van de app (0 = nog niet gemeld)
	IdleTicks uint64 // ruwe idle-tik-teller (CtrlIdle; bij SMP gedeeld door de cores)
	Wakes     uint64 // ruwe wek-teller (CtrlWakes): idle-rondes van de app-scheduler
	Cores     uint64 // aantal cores van het slot (CtrlCores; 0 = geen SMP-veld gezet)
	Shared    bool   // deelt zijn core met een medebewoner (CtrlShared, door HOP gezet)

	// Door de EL2-vectoren gerapporteerd bij een onvrijwillig einde:
	// FaultVec = layout.FaultSync (stage-2-fault; ESR/FAR geldig) — zowel bij
	// een spontane kooi-overtreding als bij HOP's hard-kill (stage2.Revoke).
	// layout.FaultNone = geen fault gezien.
	FaultVec uint64
	FaultESR uint64
	FaultFAR uint64

	// Cage is de architectuur-eigen kooi-diagnose in één regel, of leeg als er
	// niets te melden is. Bestaansreden is dezelfde als die van de Fault*-velden
	// hierboven — een headless node vertelt anders niets — maar de UART bleek
	// een slecht meetinstrument: op 115200 verliest die lijn bytes, en een
	// gehavend hex-getal is erger dan geen (gemeten 31-07: misa kwam als
	// "rv128 …0094112d" binnen, alleen te lezen doordat de extensieletters de
	// hex bevestigden). Dit veld gaat over het netwerk mee naar HOP, waar het
	// foutloos aankomt.
	//
	// Op ARM leeg: daar dekken de Fault*-velden het al.
	Cage string
}

// checkSlot valideert een slot-index; elke publieke functie begint hiermee —
// de control-page- en ringadressen worden er rechtstreeks uit berekend.
func checkSlot(i int) error {
	if i < 1 || i > layout.MaxSlots {
		return fmt.Errorf("slot %d out of range 1..%d", i, layout.MaxSlots)
	}
	return nil
}

// devReaderAt is een io.ReaderAt over een stuk device-geheugen (de partitie-
// staging waar de image in staat) — zo parseert debug/elf de ELF zonder dat
// het bestand ooit volledig in de kern-RAM staat.
type devReaderAt struct {
	base uintptr
	size int64
}

func (d devReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= d.size {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > d.size-off {
		n = int(d.size - off)
	}
	dev.CopyOut(p[:n], d.base+uintptr(off))
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Start laadt image in slot i (1-based, = core-index): de bytes gaan de staging
// bovenin de partitie in, waarna de ELF daaruit geparsed en geplaatst wordt
// (placeFromStaging), de RAM-declaratie naar memLimit gepatcht, en de core
// gewekt. De image is een gewone tamago-ELF, canoniek gelinkt (TEXT_START =
// SlotBase(1)+0x10000): de stage-2-map legt het canonieke bereik op de partitie
// van dít slot, dus één artifact draait op elk slot.
//
// image is een in-memory slice — de ingebakken apploader (StartLoader) of een
// Pi-demo-image — GÉÉN io.Reader/download-body meer: de app downloadt zijn eigen
// image zelf (apploader → StartStaged). Zo leest core 0 hier nooit van het
// netwerk terwijl de NIC gequiesced is (dat gaf een deadlock — finding #3) en
// buffert de kern nooit 127 downloads tegelijk. De blob is gedeeld (ingebakken),
// dus dev.Copy hieronder alloceert niets per start.
//
// mounts is de volume-tabel (shared path → local path, HOP's Job.Volumes): de
// task ziet zijn eigen lege root plus de gemounte shared dirs. ports (HOP's
// Task.Ports) worden na de start gepubliceerd (stateloze DNAT bij de switch).
//
// job is de naam van de JOB (niet de task): de naamruimte van deze app in de
// object-store (apps/<cluster>/<job>/ — storage.go). De jób, zodat een
// herstart of failover elders dezelfde map ziet en replica's hem delen als
// een shared volume. "" = geen store-toegang (embed-demo's, de apploader).
func Start(i int, image []byte, memLimit uint64, cores int, env map[string]string, mounts map[string]string, ports map[string]int, job string) error {
	return startImage(i, image, memLimit, cores, env, mounts, ports, job, false)
}

// startImage is het gedeelde startpad van Start en StartShared (share.go).
// shared verlegt één wacht: niet "de cores van het slot zijn geparkeerd/cold"
// (de gedeelde core drááit meestal juist), maar "dít slot leeft nergens".
func startImage(i int, image []byte, memLimit uint64, cores int, env map[string]string, mounts map[string]string, ports map[string]int, job string, shared bool) (err error) {
	imgSize := int64(len(image))
	if imgSize <= 0 {
		return fmt.Errorf("Start: lege image")
	}
	// Pure invoervalidatie vóór het venster (prepStart); size/appRAM zijn ook
	// puur rekenwerk.
	mtab, envBlob, cores, err := prepStart(i, memLimit, cores, env, mounts, ports)
	if err != nil {
		return err
	}
	// prepStart is niet puur: grantEnv vergeeft de fb-grant (en zet daarmee de
	// HOP-console uit) vóórdat de env-groottecheck, de allocatie en de plaatsing
	// zijn geweest. Elk faalpad hierna moet het glas dus teruggeven, anders
	// blijft de console weg voor een task die nooit gedraaid heeft.
	var started bool
	defer func() {
		if !started {
			grantRelease(i)
		}
	}()
	// size/appRAM worden hieronder OPNIEUW gezet uit wat partAlloc werkelijk
	// reserveerde: de kooi van dit board kan een grotere korrel eisen dan 2MB, en
	// dan is dít getal niet de partitie. Hier alleen om de invoer te valideren.
	size := align2M(memLimit)
	appRAM, err := appRAMSize(size)
	if err != nil {
		return err
	}
	// Eén lifecycle tegelijk, in een DMA-stil venster: de defer dekt álle
	// paden — op quiescer-boards sluit het venster pas na de WaitReady
	// onderaan placeFromStaging, zodat ook de app-boot (heap-zeroing)
	// erbinnen valt. t0 meet de venster-tijd: dít is wat de convoy bij een
	// storm serialiseert, dus dit hoort zichtbaar te zijn op een headless node.
	defer lifecycleWindow()()
	t0 := time.Now()
	// De EL2-vectoren + parkeerlus + mailboxen moeten klaar zijn vóór de
	// eerste dispatch (mailbox cold-detectie leest een geveegde mailbox).
	vectorsOnce.Do(cageInit)
	if shared {
		if st := ctxState(i); ctxLive(st) {
			// De task-boekhouding is hier de autoriteit: wie plaatst, heeft
			// het slot vrij bevonden — een "levende" ctx zonder eigenaar is
			// dus een LIJK, geen bewoner. De klassieke bron is DRAM-residu:
			// DDR overleeft een warme herstart (snelle herprik, watchdog),
			// en gemeten 02-08 weigerde een verse boot zijn állereerste
			// plaatsing op het Saved-lijk van de vorige run — beide jobs
			// stormden tot give-up. Detecteren zonder opruimen is dan half
			// werk (Derek): ruim het lijk en ga door.
			//
			// De scheidsrechter is de BEWONERSLIJST, niet het hart: die lijst
			// is HOP-eigen RAM (vers bij elke boot, onder lock gemuteerd), en
			// de rotatie hervat uitsluitend wat erin staat. Een ctx-lijk
			// búiten de lijst kan dus nooit meer tot leven komen — draaiend
			// hart of niet — en dat is precies het gat dat "alleen op een
			// stilstaand hart" liet liggen: op één gedeeld hart start de
			// eerste plaatsing het hart, waarna het lijk van de twééde slot
			// onruimbaar werd (gemeten 02-08, cloudflared-storm na de
			// welcome-evict). Stáát hij wél in de lijst, dan is de
			// inconsistentie echt en faalt het luid.
			if residentListed(coreOf(i), i) {
				return fmt.Errorf("slot %d still live (ctx-state %d, scheduled on core %d) — stop it before StartShared", i, st, coreOf(i))
			}
			fmt.Printf("slot %d: unowned resident (ctx-state %d, in no rotation) — evicting the corpse, reusing the slot HOPOS_CTX_EVICT\n", i, st)
			ctxWrite(i, layout.CtxState, layout.CtxEmpty)
		}
	} else if err := coresFree(i, cores, "not parked/cold"); err != nil {
		return err
	}

	// De fysieke partitie éérst alloceren (partAlloc heeft alleen i+memLimit
	// nodig, niet het linkadres): we kopiëren de image erin vóór we hem
	// parsen. started markeert een geslaagde start: valt Start eerder
	// uit, dan geeft de defer de gealloceerde partitie terug.
	base, size, err := partAlloc(i, memLimit)
	if err != nil {
		return err
	}
	// De maat die de allocator ÉCHT gaf — één bron van waarheid voor de kooi, de
	// ABI-staart, de ringen en de RAM-declaratie van de app. Zie partAlloc.
	if appRAM, err = appRAMSize(size); err != nil {
		return err
	}
	// Eén regel per plaatsing: op een headless node is dít hoe je ziet wáár
	// een slot fysiek landt (sinds 15-07 ook boven de 512GB-grens).
	fmt.Printf("slot %d: partition %d MB @ %#x\n", i, size>>20, base)
	defer func() {
		if started {
			return
		}
		// Faalde het startschot zélf, dan is onbekend of de core tóch aangaat
		// (zie errDispatch): het geheugen gaat dan NIET terug de pool in, want
		// first-fit zou het aan een volgende huurder uitdelen terwijl er nog
		// leven in kan zitten. Alle andere faalpaden zijn eenduidig — daar is de
		// partitie gewoon vrij.
		if errors.Is(err, errDispatch) {
			fmt.Printf("slot %d: partition quarantined — dispatch outcome unknown HOPOS_PART_QUARANTINE\n", i)
			return
		}
		partRelease(i)
	}()

	// Coherentie vóór de ongecachte writes: de vórige huurder draaide
	// cacheable (hele heap); zijn dirty lines eerst wegschrijven+invalideren,
	// anders clobberen ze straks onze verse image (QEMU verhult dit — geen
	// caches; op de A76 echt, gemeten 2026-07-10). Coöperatief (coopCleanInv):
	// dit is de zware core-0-op van de 127-loader-burst — in brokken vegen met
	// een yield ertussen houdt de netstack/health/switch levend (het slot hangt
	// hier niet aan de switch: bij hergebruik detachte releaseSlot, vers nooit).
	coopCleanInv(uintptr(base), uintptr(size))

	// De image bovenin het app-RAM plaatsen (staging, layout.StageAddr — het
	// gedeelde contract met de apploader), zodat de laag geplaatste segmenten
	// er niet mee botsen; de net-ring dáárboven (de partitie-staart) blijft
	// vrij. Eén dev.Copy van de gedeelde in-memory blob — geen
	// per-start-allocatie, geen netwerk (finding #3).
	addr, _, fits := layout.StageAddr(base, appRAM, imgSize)
	if !fits {
		return fmt.Errorf("image %d bytes past niet in partitie %d MB (app-RAM %d MB)", imgSize, size>>20, appRAM>>20)
	}
	stageAddr := uintptr(addr)
	dev.Copy(stageAddr, image)

	if err := placeFromStaging(i, base, size, stageAddr, imgSize, memLimit, cores, envBlob, mtab, ports, job); err != nil {
		return err
	}
	started = true // partitie blijft van deze task tot Stop
	fmt.Printf("slot %d: image placed in %s%s\n", i, time.Since(t0).Round(time.Millisecond), kernMemOnce())
	return nil
}

// kernMemOnce meldt HOP's eigen geheugengebruik één keer: bij de eerste
// geslaagde plaatsing. Dát is HOP's zwaarste moment (de imagecopy en het
// cache-onderhoud over een hele partitie), dus dit is het getal waarop je
// HopBase mag afrekenen — en één keer, want een regel per start is op een
// 115200-console duurder dan hij waard is. KernMem wordt gezet door de main
// (die kent de runtime-cijfers); zonder setter blijft de regel leeg.
var (
	kernMemFn   func() string
	kernMemDone bool
)

// SetKernMem geeft slots de functie waarmee het HOP's eigen geheugenstand
// opvraagt (cmd/hopos: hopUsage). Optioneel — zonder blijft de melding weg.
func SetKernMem(f func() string) { kernMemFn = f }

func kernMemOnce() string {
	if kernMemDone || kernMemFn == nil {
		return ""
	}
	kernMemDone = true
	return " — HOP: " + kernMemFn()
}

// StartLoaderOn is StartLoader op een door HOPOS gekozen core (coöperatieve
// core-deling): de apploader draait als (mede)bewoner van `core` i.p.v. op
// core=slot. slotmgr kiest de core met de pool-allocator (PlaceCage) en de kooi
// erft hem via StartShared (hostCore), zodat fase 2 (StartStaged) en Stop
// dezelfde core hergebruiken. Bij één bewoner op een verse core gedraagt dit
// zich exact als StartLoader.
func StartLoaderOn(core, i int, memLimit uint64, env map[string]string) error {
	img := apploaderblob.Loader()
	if len(img) == 0 {
		return fmt.Errorf("apploader niet ingebakken of uitpakken faalde (bouw de node met -tags embedloader)")
	}
	// job="": de loader is systeemwerk zonder store-toegang; de échte app
	// krijgt zijn naamruimte in fase 2 (StartStaged).
	return StartShared(core, i, img, memLimit, env, nil, nil, "")
}

// appRAMSize is het deel van de partitie dat de app als RAM ziet: de bovenste
// AbiTail is zijn ABI-staart (control page, ringen, net-ringen) ("512MB → 510 Go + 2 netbuffer"). Zo komt het
// ring-geheugen uit de eigen memLimit van de job — er draait geen statische
// SlotCap-reservering meer in het board-plan — en blijft de coherentie gratis:
// de app declareert de staart niet als RAM (zijn stage-1 mapt hem nooit
// cacheable), HOP raakt hem alleen device-side, en de bestaande CleanInv over
// de hele partitie veegt de dirty lines van de vórige huurder.
func appRAMSize(size uint64) (uint64, error) {
	if size < 2*layout.AbiTail {
		return 0, fmt.Errorf("memLimit te klein: partitie %d MB laat geen app-RAM over naast de %d MB net-ring",
			size>>20, uint64(layout.AbiTail)>>20)
	}
	return size - layout.AbiTail, nil
}

// placeFromStaging is de tweede helft van een slot-start: de image staat al in
// de staging bovenin de partitie — óf door Start (een ingebakken blob, zoals de
// apploader), óf door de apploader vanaf zíjn eigen download (StartStaged). Van
// hieraf is alles geprivilegieerd HOP-werk: ELF parsen, segmenten plaatsen,
// RAM-symbolen patchen, stage-2 bouwen en de core (her)dispatchen. Eén bron van
// waarheid voor beide startpaden.
func placeFromStaging(i int, base, size uint64, stageAddr uintptr, imgSize int64, memLimit uint64, cores int, envBlob []byte, mtab [][2]string, ports map[string]int, job string) error {
	// De net-ring van dit slot: de partitie-staart. Puur een lokale berekening —
	// de PA gaat als parameter naar ring-init, hopswitch.Attach en stage2.Build,
	// dus er bestaat geen register dat stale kan worden (de PA leeft precies zo
	// lang als de partitie).
	appRAM, err := appRAMSize(size)
	if err != nil {
		return err
	}
	// Het plan (parse + álle validatie + patchwaarden) komt uit abi/place —
	// dezelfde bron van waarheid als de zelfplaatsing (applib/selfplace.go).
	// Gelezen vanuit de gestreamde device-kopie (geen kern-RAM-kopie); het
	// linkvenster is arch-bepaald (de kooi-naad): op ARM het canonieke
	// slot-1-adres, want daar ís de stage-2 de relocatie en draait één artifact
	// in elk slot; op een architectuur zonder tweede fase de partitie zelf, want
	// daar draait een image op de adressen waarop het gelinkt is. Het plafond is
	// de staging-onderkant: segmenten mogen hun eigen kopieerbron niet raken.
	linkBase := cageLinkBase()
	plan, err := place.Build(devReaderAt{base: stageAddr, size: imgSize}, imgSize,
		linkBase, appRAM, cageFloor, uint64(stageAddr)-base, i, layout.ABIVersion)
	if err != nil {
		return err
	}
	if max := maxLimitFor(linkBase); memLimit > max {
		return fmt.Errorf("memLimit %d MB > %d MB slot-cap (één GB-blok vanaf linkadres %#x, geklemd onder CtrlBase; groter vergt vensteruitbreiding — zie slots/partmem.go)", memLimit>>20, max>>20, linkBase)
	}
	delta := base - linkBase // PA = linkadres + delta (identiek slot: 0)

	// Het plan uitvoeren, device→device (dev.Move, kleine stack-buffer — geen
	// kern-RAM voor de hele image); RamStart blijft het línkadres (de app
	// ziet IPA's, de stage-2 vertaalt), RamSize = app-RAM (partitie −
	// net-ring — de staart is nooit heap/stack).
	for _, s := range plan.Segs {
		dev.Move(uintptr(s.Dst+delta), stageAddr+uintptr(s.Off), s.Filesz)
		dev.Clear(uintptr(s.Dst+delta)+uintptr(s.Filesz), s.Memsz-s.Filesz)
	}
	for _, p := range plan.Patches {
		dev.Write64(uintptr(p.Addr+delta), p.Val)
	}

	return armSlot(i, base, size, plan.Entry, memLimit, cores, envBlob, mtab, ports, job)
}

// armSlot is de gedeelde slotstart-staart: servicer/switch-hygiëne, verse
// control-page + ringen, stage-2-kooi, en de (her)dispatch van de core op
// entry. Twee aanroepers: placeFromStaging (HOP plaatste de bytes zelf, entry
// = de app-entry uit de ELF) en het zelfplaats-pad van StartStaged (de loader
// plaatste voor, entry = het stubje dat op de eigen core de segmenten schuift
// en dan de app inspringt — zie applib/selfplace.go).
func armSlot(i int, base, size uint64, entry, memLimit uint64, cores int, envBlob []byte, mtab [][2]string, ports map[string]int, job string) (err error) {
	appRAM, err := appRAMSize(size)
	if err != nil {
		return err
	}
	// Transactioneel: hieronder gaan switch-poort, poort-publicaties, fb-grant
	// en control-page-status áán, en die horen bij DEZE task. Faalt er daarna
	// nog iets — een poortcollisie, de stage-2-bouw, het startschot — dan moet
	// alles weer uit. Anders blijft er een switch-poort achter met ringadressen
	// in een partitie die de aanroeper zo terugsluist naar de pool: first-fit
	// deelt die dan uit aan de volgende huurder, en de switch leest/schrijft
	// diens geheugen onder de identiteit van dit slot. Alleen bij succes
	// (armed) blijft de opbouw staan.
	var armed bool
	defer func() {
		if armed {
			return
		}
		hopswitch.Detach(i)
		hopswitch.UnpublishSlot(i)
		grantRelease(i)
		ctrlWrite(i, layout.CtrlStatus, layout.StatusEmpty)
		smpCores[i] = 0
	}()
	// De frame-ringen van dit slot: in de staart van zijn eigen partitie (layout:
	// de slot-ABI), net als zijn control-page en hop-ABI-ringen.
	netPA := uint64(layout.NetRingBaseAt(base, appRAM))
	// De entry moet in het linkvenster van dit slot liggen. Dat venster is niet
	// universeel: verplaatst de kooi adressen (stage-2), dan is het het canonieke
	// slot-1-IPA en draait één artifact in elk slot; verplaatst hij niets, dan is
	// het de partitie zelf. Deze check bewaakt vooral het ONVERTROUWDE
	// CtrlPlaceEntry van het zelfplaats-pad — een app mag zijn core nergens
	// anders laten binnenkomen dan in zijn eigen venster.
	linkBase := cageLinkBase()
	window := cageLinkWindow(size)
	if entry < linkBase || entry >= linkBase+window {
		return fmt.Errorf("entry %#x buiten het linkvenster van dit slot %#x..%#x",
			entry, linkBase, linkBase+window)
	}
	if max := maxLimitFor(linkBase); memLimit > max {
		return fmt.Errorf("memLimit %d MB > %d MB slot-cap (één GB-blok vanaf linkadres %#x)", memLimit>>20, max>>20, linkBase)
	}

	// SPSC-hygiëne: geen oude servicer meer op deze ringen vóór her-init,
	// en de switch van de frame-ringen af vóór díé opnieuw geïnitieerd
	// worden. Poort-publicaties horen bij de vorige task: intrekken (de
	// nieuwe task publiceert de zijne ná deze Start).
	evictServicer(i)
	hopswitch.Detach(i)
	hopswitch.UnpublishSlot(i)

	// Storage: verse (lege) eigen root — schone lei per start — en de
	// shared dirs van de mounts aanmaken als ze nog niet bestaan.
	root := fmt.Sprintf("/.tasks/slot%d", i)
	if fsys != nil {
		if err := fsys.RemoveAll(root); err != nil {
			return fmt.Errorf("root vegen: %w", err)
		}
		if err := fsys.MkdirAll(root); err != nil {
			return fmt.Errorf("root maken: %w", err)
		}
		for _, m := range mtab {
			if err := fsys.MkdirAll(m[1]); err != nil {
				return fmt.Errorf("shared dir %q: %w", m[1], err)
			}
		}
	} else if len(mtab) > 0 {
		return fmt.Errorf("mounts requested but no storage layer (UseFS)")
	}

	// Control-page vegen, env-blob schrijven, hop-ABI-ringen klaarzetten,
	// BOOTING, core wekken — alles op de fysieke plekken uit het board-plan.
	ctrlPA := layout.CtrlPageAt(base, appRAM)
	dev.Clear(ctrlPA, layout.CtrlStride)
	if len(envBlob) > 0 {
		dev.Copy(ctrlPA+layout.CtrlEnvData, envBlob)
	}
	dev.Push(ctrlPA, layout.CtrlStride) // de hele verse page publiceren
	ctrlWrite(i, layout.CtrlEnvLen, uint64(len(envBlob)))
	// Klok doorgeven: de teller is gedeeld, dus HOP's offset geldt 1-op-1.
	ctrlWrite(i, layout.CtrlWallOff, uint64(board.Current().TimerOffset()))
	// Geen net-config meer op de control-page: elke task heeft altijd een adres
	// op het interne net en leidt IP/gateway/MAC deterministisch af uit zijn
	// slotnummer (layout-net-plan, gedeeld met de switch); de app initieert een
	// stack pas als hij appnet.Up aanroept.
	ring.Init(layout.RingOutboxAt(base, appRAM), layout.RingDataCap)
	ring.Init(layout.RingInboxAt(base, appRAM), layout.RingDataCap)
	ring.Init(layout.NetRingTXAt(base, appRAM), layout.NetRingDataCap)
	ring.Init(layout.NetRingRXAt(base, appRAM), layout.NetRingDataCap)

	// De core krijgt stage-2-isolatie: de EL2-trampoline activeert de hier
	// gebouwde tabel en dropt pas dan naar de app-entry (een canoniek IPA — de
	// stage-2 vertaalt hem naar deze partitie). De app-image draait nooit op
	// EL2. De trampoline is data-gedreven: alles staat op deze control-page.
	if err := cagePrepare(i, linkBase, base, size, entry); err != nil {
		return err
	}
	// DeviceGrant-haak: het venster van de houder de kooi in (no-op voor
	// alle andere slots) — vóór de dispatch, zelfde walker-regime als Build.
	if err := grantArm(i); err != nil {
		return fmt.Errorf("grant slot %d: %w", i, err)
	}
	// De fysieke core van dit slot: het klassieke model is slot = core, maar
	// een StartShared-slot woont op de core die HOP koos (coreOf, share.go).
	// De mailbox/het sched-blok is een CORE-ding: TPIDR_EL2 en de parkeerlus
	// horen bij de core waar dit slot daadwerkelijk draait.
	core := coreOf(i)
	ctrlWrite(i, layout.CtrlEntry, entry)
	ctrlWrite(i, layout.CtrlVecPA, uint64(layout.VecBasePA()))
	ctrlWrite(i, layout.CtrlSlot, uint64(i))
	ctrlWrite(i, layout.CtrlMboxPA, uint64(layout.ParkMboxPA(core))) // → TPIDR_EL2
	// Het aantal cores op de control-page; de app-OS-laag leest 'm en vraagt bij
	// cores > 1 de extra cores lazy op (CtrlSMPReq → HOP dispatcht). Altijd
	// zetten (ook 1 = gewone app), zodat de app-kant niet hoeft te weten of dit
	// SMP is. LET OP: dit is HOP → app-informatie; HOP vertrouwt de readback
	// NOOIT (de app kan de page herschrijven). De vertrouwde bron voor HOP's
	// eigen beslissingen is smpCores, hier uit het al-gevalideerde `cores` gezet.
	smpCores[i] = cores
	ctrlWrite(i, layout.CtrlCores, uint64(cores))
	if cores > 1 {
		// Fysiek adres van de EL2 SMP-trampoline publiceren (op ditzelfde slot
		// z'n partitie/stage-2 → gedeelde heap).
		ctrlWrite(i, layout.CtrlSMPTramp, cageSMPEntryPC())
	}
	ctrlWrite(i, layout.CtrlStatus, layout.StatusBooting)

	// Poorten publiceren en de slot aan de switch hangen VÓÓR het startschot:
	// faalt Publish, dan geeft de defer de partitie terug terwijl er nog geen
	// app op draait. Ná een geslaagde dispatchCore zou datzelfde faalpad de
	// partitie vrijgeven met een nog-lévende app erin — de pool kan 'm dan
	// heruitdelen aan een ander slot: isolatiebreuk. Attach/Publish zetten alleen
	// switch/NAT-state en hebben de draaiende core niet nodig, dus dit mag ervóór;
	// ná de dispatch volgt meteen started=true, zonder faalbare stap ertussen.
	hopswitch.Attach(i, uintptr(netPA))
	for name, p := range ports {
		if err := hopswitch.Publish("tcp", uint16(p), i, uint16(p)); err != nil {
			return fmt.Errorf("poort %q: %w", name, err)
		}
	}

	// Startschot, drie routes. Ctx = de fysieke control-page; de trampoline
	// leest er alles van, dus alle drie eindigen in exact dezelfde boot:
	//   - core draait al (gedeelde core, bewoner erbij): boot-pending in het
	//     ctx-blok — de EL2-rotatie boot hem bij de eerstvolgende yield;
	//   - geparkeerd: bewonerslijst = [dit slot], mailbox + SEV;
	//   - cold: idem, maar eenmalig via PSCI CPU_ON.
	tramp := cageEntryPC(i)
	ctx := uint64(ctrlPA)
	// De control-page-PA in het ctx-blok: de EL2-trampoline krijgt hem als x0 en
	// het fault-rapport van switch.s leest hem daar terug. Voor élk slot, niet
	// alleen het gedeelde-core-pad — sinds de ABI in de partitie woont kan die
	// asm het adres niet meer uitrekenen.
	ctxWrite(i, layout.CtxCtrlPA, ctx)
	if coreRunning(core) {
		if err := bootPendingDispatch(core, i, tramp, ctx); err != nil {
			return fmt.Errorf("%w: %v", errDispatch, err)
		}
	} else {
		residentReset(core, i)
		if err := dispatchCore(core, tramp, ctx); err != nil {
			return fmt.Errorf("%w: %v", errDispatch, err)
		}
	}
	// Vanaf hier draait de app: de opbouw blijft staan (geen faalbare stap meer).
	armed = true

	go registerServicer(i, root, job, mtab).run()

	// Alleen op boards met een écht DMA-stil venster (NetQuiescer — de
	// C1-erratum-familie) wachten we hier tot de app READY meldt: dáár hoort
	// ook de app-boot (heap-zeroing, TLBI's) binnen het venster te vallen.
	// Overal anders is quiesce een no-op en zou dit de geserialiseerde
	// lifecycle tot 3s per start vasthouden — bij 127 slots een convoy van
	// minuten (Altra-meting 15-07). Daar boot de app parallel verder; wie
	// READY nodig heeft pollt WaitReady zelf. Best-effort deadline: een app
	// die láng doet over z'n init houdt de lifecycle ook op de Pi niet eeuwig
	// vast.
	if _, ok := board.Current().(board.NetQuiescer); ok {
		_ = WaitReady(i, 3*time.Second)
	}
	return nil
}

// StartStaged plaatst de échte app vanaf de image die de apploader al in de
// staging bovenin de partitie heeft gedownload (control-page StatusStaged). De
// partitie is al gealloceerd (fase 1: de runner startte de apploader via
// StartLoader), dus we hergebruiken 'm, plaatsen de app eroverheen en
// her-dispatchen de geparkeerde core. Zo verhuist het downloaden naar de app
// (eigen core+netstack), terwijl het geprivilegieerde plaatsen bij HOP blijft.
//
// imgSize komt van de control-page (door de loader gezet) en is NIET vertrouwd:
// een verkeerde maat faalt hooguit de ELF-parse/segment-validatie van dít slot.
//
// job is de store-naamruimte van de app (zie Start) — fase 2 is waar de échte
// app hem krijgt; de apploader van fase 1 heeft er niets te zoeken.
func StartStaged(i int, memLimit uint64, cores int, env map[string]string, mounts map[string]string, ports map[string]int, job string) (err error) {
	// Pure invoervalidatie vóór het venster — gedeeld met Start (prepStart).
	mtab, envBlob, cores, err := prepStart(i, memLimit, cores, env, mounts, ports)
	if err != nil {
		return err
	}
	// Zelfde niet-pure prepStart als in startImage: de fb-grant terug op elk
	// faalpad. De partitie NIET vrijgeven — die is van fase 1 (de apploader) en
	// wordt hier alleen hergebruikt; opruimen is de taak van Stop.
	var staged bool
	defer func() {
		if !staged {
			grantRelease(i)
		}
	}()
	// De grootte die de loader in de staging heeft gezet (control-page). Niet
	// vertrouwd — een verkeerde maat faalt hooguit de ELF-parse van dit slot.
	imgSize := int64(ctrlRead(i, layout.CtrlStagedSize))
	if imgSize <= 0 {
		return fmt.Errorf("StartStaged: geen gestagede image in slot %d (CtrlStagedSize=%d)", i, imgSize)
	}
	// Zelfplaatsing: heeft de loader een plaatsings-stubje klaargezet
	// (applib/selfplace.go), dan hoeft HOP geen byte te schuiven — alleen de
	// kooi wapenen en de core op het stubje dispatchen; dat schuift op zijn
	// eigen core de segmenten en springt de app in. 0 = legacy (HOP plaatst
	// vanaf de staging). Vóór het venster gelezen: de ctrl-clear in armSlot
	// veegt het veld zo meteen.
	placeEntry := ctrlRead(i, layout.CtrlPlaceEntry)
	defer lifecycleWindow()()
	t0 := time.Now() // venster-tijd — wat de convoy serialiseert (zie Start)
	vectorsOnce.Do(cageInit)
	// De apploader parkeerde ná het seinen — "staged" op de ctrl-page landt
	// dus eerder dan zijn park/exit; kort wachten in plaats van meteen falen
	// (gemeten Pi 5 19-07: de A76 verliest die race consequent). En op een
	// gedeelde core parkeert de core nóóit zolang de buren leven — dáár is
	// het juiste teken de ctx-staat van dít slot (de HVC-exit van de loader
	// zet Dead), niet de core-mailbox. Zelfde onderscheid als Start (shared-
	// tak boven zijn coresFree).
	// De reset-vraag hangt af van wie er nog op dit hart woont — zie de takken.
	if coreOf(i) != i || slotShares(i) {
		// Gedeeld hart: NIET quiescen. Een quiesce is hier een hart-reset en die
		// neemt de BUREN mee — precies waarop de tweede app stukliep (gemeten
		// 31-07: "loader still live on shared core 1"). De loader meldt zich hier
		// zelf dood: op ARM met hvc #0, op RISC-V met de exit-trap (cpu/idle
		// ExitTrap → cpu/mmode), en de arch-switch zet CtxDead. Dáár wachten we op.
		if !waitCtxDead(i, 2*time.Second) {
			return fmt.Errorf("place staged slot %d: loader still live on shared core %d", i, coreOf(i))
		}
	} else {
		// Eigen hart: hier mág de reset, en op een architectuur zonder parkeerlus
		// is hij nodig — daar draait de exit-lus van de loader door tot HOP het
		// hart ophaalt. Op ARM is dit een no-op (de app parkeert zichzelf op EL2).
		cageQuiesce(coreOf(i))
		deadline := time.Now().Add(2 * time.Second)
		for {
			err := coresFree(i, cores, "loader not parked?")
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				return err
			}
			time.Sleep(time.Millisecond)
		}
	}
	// De partitie van fase 1 (de loader) hergebruiken — niet opnieuw alloceren.
	base, size, ok := partitionOf(i)
	if !ok {
		return fmt.Errorf("StartStaged: slot %d heeft geen partitie (loader niet gestart?)", i)
	}
	// De loader stagede via layout.StageAddr met zíjn gepatchte RamSize =
	// appRAM — zelfde functie hier, dus de compiler bewaakt dat we op dezelfde
	// plek lezen als waar hij schreef.
	appRAM, err := appRAMSize(size)
	if err != nil {
		return err
	}
	if placeEntry != 0 {
		// Geen CleanInv en geen parse op core 0: het stubje veegt op zíjn
		// core eerst heel app-RAM (DC CIVAC — dezelfde coherentie-stap, maar
		// per-slot-parallel) en schuift dan de al-gevalideerde segmenten.
		if err := armSlot(i, base, size, placeEntry, memLimit, cores, envBlob, mtab, ports, job); err != nil {
			return err
		}
		staged = true
		fmt.Printf("slot %d: self-place dispatched in %s\n", i, time.Since(t0).Round(time.Millisecond))
		return nil
	}
	addr, _, fits := layout.StageAddr(base, appRAM, imgSize)
	if !fits {
		return fmt.Errorf("staged image %d bytes past niet in partitie %d MB (app-RAM %d MB)", imgSize, size>>20, appRAM>>20)
	}
	stageAddr := uintptr(addr)
	// Coherentie: de loader draaide cacheable; zijn dirty lines wegschrijven+
	// invalideren vóór we de echte app er ongecachet overheen plaatsen. De loader
	// flushte de staging zelf al (StageImage); dit dekt de rest van de partitie.
	// Dat dit vanaf de HOP-core kán, is ARM-eigen: cache-onderhoud is daar
	// broadcast over de inner-shareable domain, dus deze veeg raakt óók de L1 van
	// de loader-core. Op RISC-V is het hart-lokaal en dekt de reset uit
	// cageQuiesce de dirty lines (het hart verliest zijn cache); daar kost deze
	// call alleen HOP's eigen kopie — ~5ms per start, niet de moeite van een
	// architectuur-vertakking waard.
	dev.CleanInv(uintptr(base), uintptr(size))
	if err := placeFromStaging(i, base, size, stageAddr, imgSize, memLimit, cores, envBlob, mtab, ports, job); err != nil {
		return err
	}
	staged = true
	fmt.Printf("slot %d: staged app placed in %s\n", i, time.Since(t0).Round(time.Millisecond))
	return nil
}

var vectorsOnce sync.Once

// EnsureVectors zet de EL2-vectoren + parkeerlus + revoke-vectoren (idempotent
// via vectorsOnce, gedeeld met Start). De node roept dit vóór smp.ConfigureNode
// aan zodat opkomende node-cores hun VBAR_EL2 (= revoke-vectoren, net als core 0
// uit bootKernel) geldig aantreffen. Later in Start is de vectorsOnce een no-op.
func EnsureVectors() { vectorsOnce.Do(cageInit) }

// Stop beëindigt de app in slot i en wacht tot al zijn cores geparkeerd zijn.
// Eén pad voor één core én voor een SMP-app: de kill-flag geeft de app een
// coöperatieve kans (de kill-watcher exit't via HVC → de core parkeert netjes,
// met exit-status). Parkeert daarna nog een core niet — de secundaire cores van
// een SMP-app, of een hangende core — dan doet HOP de hard-kill via
// stage2.Revoke: het nult de stage-2-map van dit slot en doet één HVC→TLBI,
// waarna élke core van het slot (ze delen tabel én VMID) op zijn eerstvolgende
// vertaalde toegang naar de EL2-vectoren faultt en dáár parkeert (Status meldt
// dan layout.FaultSync). De cores gaan nóóit terug naar de firmware — HopOS
// bezit ze en herstart ze via hun mailbox.
func Stop(i int, timeout time.Duration) error {
	if err := checkSlot(i); err != nil {
		return err
	}
	// Eén lifecycle tegelijk, in een DMA-stil venster: ook de coöperatieve
	// kill parkeert een core — gemeten (13-07, torture): kill+park naast
	// lopende RX-DMA is dodelijk op C1. De kill-flag hoort BINNEN het venster:
	// een vroege write buiten het lock (probeersel 15-07) racete met een
	// parallelle Start op hetzelfde slot en landde dan op de nét geveegde
	// ctrl-page van de VOLGENDE huurder — die exitte braaf binnen 50ms
	// ("apploader exited before staging", de ronde-10-cascade). Sinds de
	// I$-fix gehoorzamen apps de kill in ~50ms, dus ook een delete-storm is
	// binnen het venster snel: ~200ms per stop i.p.v. de oude 10s-timeouts.
	defer lifecycleWindow()()
	ctrlWrite(i, layout.CtrlKill, 1)
	dev.MB()
	// Gedeelde core (er leeft nog een andere bewoner op de core van dit
	// slot): "core geparkeerd" bestaat daar niet — de buren draaien door. De
	// ctx-staat van dít slot is de waarheid: de coöperatieve exit (HVC) of de
	// fault na een Revoke zet hem op dead (switch.s), waarna de rotatie
	// gewoon verdergaat met de rest.
	if slotShares(i) {
		var stopErr error
		if !waitCtxDead(i, timeout) {
			cageRevoke(i)
			// De intrekking raakt een gesavede bewoner pas bij zijn
			// eerstvolgende hervatting (≤ een paar yield-tikken): dan faultt
			// hij op de genulde tabel en meldt de switch hem dood.
			if !waitCtxDead(i, time.Second) {
				stopErr = fmt.Errorf("slot %d: not dead after stage-2 revocation (shared core %d)", i, coreOf(i))
			}
		}
		// Partitie alleen terug de pool in als dit slot aantoonbaar dood is.
		releaseSlot(i, stopErr == nil)
		return stopErr
	}
	// Coöperatieve kans voor de app (de kill-watcher parkeert zijn eigen
	// core). Ook een niet-delend slot kan op een vreemde core wonen
	// (StartShared als eerste bewoner), dus alles hier is core-gebaseerd.
	core := coreOf(i)
	waitSlotQuiet(i, core, timeout)
	// Draait er nog iets? Hoeveel cores de app heeft komt uit HOP's eigen
	// smpCores (door Start gezet) — NIET uit de app-schrijfbare CtrlCores: een
	// verlaagde CtrlCores zou anders levende secundaire cores voor deze scan
	// verbergen en releaseSlot een nog-draaiende partitie laten vrijgeven.
	// (SMP-apps zijn altijd dedicated met slot = core; hun secundaire cores
	// zijn dan core+1..core+n-1, exact het oude bereik.)
	n := coreCount(i)
	stillOn := false
	for c := core; c < core+n; c++ {
		if coreRunning(c) {
			stillOn = true
			break
		}
	}
	var stopErr error
	if stillOn {
		// Eén intrekking velt álle cores van het slot (gedeelde tabel/VMID).
		cageRevoke(i)
		for c := core; c < core+n; c++ {
			if !coreRunning(c) {
				continue
			}
			if !waitStopped(c, time.Second) {
				stopErr = fmt.Errorf("slot %d: core %d did not park even after stage-2 revocation", i, c)
			}
		}
	}
	// Idem voor de dedicated tak: een core die na de intrekking niet parkeerde
	// kan nog bij dit geheugen — dat gaat niet terug de pool in.
	releaseSlot(i, stopErr == nil)
	return stopErr
}

// releaseSlot maakt een gestopt slot vrij: van de switch af, poorten in, en
// de partitie terug naar de pool (de cores zijn geparkeerd, dus niemand raakt
// het geheugen meer — pas bij een volgende Start worden ze her-gedispatcht).
//
// freePartition=false is de fail-closed-variant: alles losmaken behálve het
// geheugen. Dat is het geval waarin de intrekking niet bevestigd kon worden —
// dan kán er nog een core met een levende vertaling naar deze partitie zijn, en
// die partitie mag de pool niet in (first-fit deelt hem anders uit aan de
// volgende huurder, die dan het geheugen met een vreemde deelt). Het geheugen
// blijft dus in quarantaine bij die core; een volgende geslaagde Stop of een
// reconcile ruimt op. Zelfde beleid als slotmgr.Stop voor de core-reservering.
func releaseSlot(i int, freePartition bool) {
	// Post-mortem eerst: de status en het fault-rapport van dit slot staan op zijn
	// control-page, en die woont in de partitie die we hieronder teruggeven. Wie
	// ná een Stop vraagt "waaróm viel hij" (de regressie, `hop logs`, een
	// operator) moet dat antwoord nog krijgen — dus bewaart HOP de laatste woorden
	// in zijn eigen geheugen. Strikt beter dan vroeger: toen bleef het rapport in
	// een gedeelde regio staan tot de volgende start hem overschreef.
	// De servicer eerst netjes weg (dan zijn wíj de enige consumer, SPSC) en de
	// outbox leegdrinken: wat de app nog te zeggen had — zijn stervensreden —
	// staat vaak nog ín de ring, omdat niemand hem meer las.
	evictServicer(i)
	drainLastWords(i)
	snapshot(i)
	// Eén korte regel met wat de app als laatste zei — maar alléén als er iets
	// mís ging. Een app die netjes met 0 afsluit heeft geen post-mortem nodig en
	// zijn logs staan al bij de task; op de blije weg zou dit één console-regel
	// per slot-release zijn (24 in de swarm-golf), en op een board waar de UART
	// bytes verliest is elke overbodige regel er één te veel.
	if i >= 1 && i <= layout.MaxSlots {
		// Kwam de app überhaupt aan de beurt? Op een board met een kooi-stub
		// vertelt die het (cageWhy); waar dat niet speelt is het leeg.
		why := cageWhy(i)
		// Het post-mortem in één keer overnemen en de per-slot-staat meteen
		// wissen: de ident-cache hoort bij een partitie en niet bij een
		// slotnummer, dus een volgende huurder begint met een leeg blad.
		diagMu.Lock()
		st := lastWords[i]
		lastMsg := lastLog[i]
		lastLog[i] = ""
		cageIdentCache[i] = ""
		diagMu.Unlock()
		// StatusEmpty = geen momentopname (nooit gestart, of al vrijgegeven):
		// dan is er niets te melden, ook geen "onbekende" dood.
		bad := st.App != layout.StatusEmpty && (st.App != layout.StatusExited || st.ExitCode != 0)
		if bad || why != "" {
			if why != "" {
				why = " " + why
			}
			// Bij een switch-fault ook het spoor uit het ctx-blok: pc/ra/sp van
			// het moment van sterven (cageFaultRegs; leeg waar de arch dat niet
			// dumpt). Eén regel méér context, alleen op het pad waar al gejaagd
			// wordt.
			if st.FaultVec != 0 {
				why += cageFaultRegs(i)
			}
			fmt.Printf("slot %d: exit code=%d status=%#x last=%q%s\n",
				i, st.ExitCode, st.App, lastMsg, why)
		}
	}
	hopswitch.Detach(i)
	hopswitch.UnpublishSlot(i)
	grantRelease(i) // grant terug (fb: HOP-console weer op het glas)
	if freePartition {
		partRelease(i)
	} else {
		fmt.Printf("slot %d: partition quarantined — revocation unconfirmed, memory NOT returned to the pool HOPOS_PART_QUARANTINE\n", i)
	}
	if i >= 1 && i <= layout.MaxSlots {
		// Bewoners-boekhouding van de core-deling: uit de lijst van zijn
		// core, ctx-staat op Empty (het slot is écht weg — de rotatie slaat
		// gaten en Empty over), en de slot→core-koppeling los.
		residentRemove(coreOf(i), i)
		ctxWrite(i, layout.CtxState, layout.CtxEmpty)
		dev.MB()
		if hostCore != nil {
			hostCore[i] = 0
		}
		smpCores[i] = 0 // vertrouwde core-telling wissen (zie smp.go)
	}
}

// diagMu beschermt HOP's diagnose-staat: het post-mortem (lastWords, lastLog),
// de kooi-ident-cache en de eenmalige ident-melding. Die staat heeft ÉCHT
// meerdere goroutines: de lifecycle schrijft hem (releaseSlot → snapshot,
// drainLastWords), de status-kant leest hem uit elke /v1-request en elke
// heartbeat-ronde (Get), en elke servicer kan als eerste een ident melden. Het
// zijn strings en structs, dus zonder lock is dit geen "verouderde waarde" maar
// een gescheurde lezing — een string-header met de pointer van de een en de
// lengte van de ander leest buiten zijn backing array.
//
// Bewust een eigen, korte lock en niet lifecycleMu: Get mag nooit achter een
// lopende Start/Stop staan te wachten, en niets onder deze lock blokkeert of
// pakt een andere lock (device-reads en Sprintf). Print doen we er buiten.
var diagMu sync.Mutex

// lastWords bewaart per slot de status van vlak vóór het vrijgeven van zijn
// partitie — HOP's post-mortem. Lazy gedimensioneerd (poolInit). Onder diagMu.
var lastWords []Status

// drainLastWords leest wat er nog in de outbox van slot i staat en bewaart de
// laatste logregel. Alleen aanroepen ná evictServicer (SPSC: één consumer).
func drainLastWords(i int) {
	ram, ramSize, ok := abiOf(i)
	if !ok {
		return
	}
	out := ring.Open(layout.RingOutboxAt(ram, ramSize))
	buf := make([]byte, layout.RingDataCap)
	last := ""
	for range 256 { // begrensd: een post-mortem mag nooit blijven hangen
		typ, n, ok := out.ReadInto(buf)
		if !ok {
			break
		}
		if typ == ring.TypeLog && n > 0 {
			last = string(buf[:n])
		}
	}
	if last == "" {
		return
	}
	diagMu.Lock()
	lastLog[i] = last
	diagMu.Unlock()
}

// lastLog is de laatste logregel die de app stuurde, per slot (onder diagMu). Hij
// hoort bij het post-mortem: een app die zichzelf afsluit zégt waarom, en die
// regel mag niet verdampen omdat niemand op dat moment meelas. Gemeten 30-07: de
// apploader meldde netjes zijn reden en die was nergens terug te vinden — de ring
// werd door HOP's runner naar de task-logs gedreneerd en de console zag niets.
var lastLog [layout.SlotCap + 1]string

// snapshot legt de huidige status van slot i vast. Aanroepen zolang de partitie
// nog bestaat; daarna is de control-page weg.
func snapshot(i int) {
	if i < 1 || i > layout.MaxSlots {
		return
	}
	// De momentopname bouwen we BUITEN diagMu: liveStatus gaat via cageState de
	// ident-cache in, en die pakt de lock zelf — met hem vast zou dat een
	// self-deadlock zijn (sync.Mutex is niet reentrant).
	var st Status
	_, _, live := abiOf(i) // niets te bewaren als de partitie al weg is
	if live {
		st = liveStatus(i)
	}
	diagMu.Lock()
	defer diagMu.Unlock()
	if lastWords == nil {
		lastWords = make([]Status, layout.MaxSlots+1)
	}
	if live {
		lastWords[i] = st
	}
}

// Get geeft de actuele status van slot i (nulwaarde bij ongeldige index). Heeft
// het slot geen partitie meer, dan is dit HOP's bewaarde post-mortem: precies de
// laatste woorden van de app, inclusief het fault-rapport.
func Get(i int) Status {
	if checkSlot(i) != nil {
		return Status{}
	}
	if _, _, ok := abiOf(i); !ok {
		var st Status
		diagMu.Lock()
		if i < len(lastWords) {
			st = lastWords[i]
		}
		diagMu.Unlock()
		// CoreOn blijft wél live: het ctx-blok is HOP-privé en overleeft de
		// partitie, dus "leeft dit slot nog" hoort geen bewaarde waarde te zijn.
		st.CoreOn = ctxLive(ctxState(i))
		return st
	}
	return liveStatus(i)
}

// liveStatus leest de control-page van een slot dat nog een partitie heeft.
func liveStatus(i int) Status {
	return Status{
		// CoreOn = dít slot leeft, volgens zijn ctx-staat (het switch-
		// contextblok): boot-pending, gesaved of draaiend. Op een gedeelde
		// core zegt de core-mailbox niets over één bewoner; dedicated slots
		// lopen via exact dezelfde boekhouding (residentReset zet Running
		// vóór het startschot, de exit/fault-paden van switch.s zetten Dead,
		// releaseSlot zet Empty). Wie een CORE wil toetsen: CoreIdle.
		CoreOn:    ctxLive(ctxState(i)),
		App:       ctrlRead(i, layout.CtrlStatus),
		ExitCode:  ctrlRead(i, layout.CtrlExitCode),
		Heartbeat: ctrlRead(i, layout.CtrlHeartbeat),
		RAMSize:   ctrlRead(i, layout.CtrlRAMSize),
		MemSys:    ctrlRead(i, layout.CtrlMemSys),
		IdleTicks: ctrlRead(i, layout.CtrlIdle),
		Wakes:     ctrlRead(i, layout.CtrlWakes),
		Cores:     ctrlRead(i, layout.CtrlCores),
		Shared:    ctrlRead(i, layout.CtrlShared) != 0,
		FaultVec:  ctrlRead(i, layout.CtrlFaultVec),
		FaultESR:  ctrlRead(i, layout.CtrlFaultESR),
		FaultFAR:  ctrlRead(i, layout.CtrlFaultFAR),
		Cage:      cageState(i),
	}
}

// cageState is de kooi-regel voor Status.Cage: waaróm de stub niet doorkwam als
// er iets mis is, en anders wat voor hart eronder zit. Twee bronnen, één veld —
// een lezer wil "vertel me over de kooi van dit slot", niet twee functies kennen.
//
// Get wordt gepolld (elke heartbeat-ronde van de runner), dus dit pad mag niet
// per keer een string bouwen. Op de blije weg alloceert cageWhy niets (hij ziet
// stubCageOK en geeft leeg terug) en komt de ident uit de cache: die verandert
// niet, want het is de identiteit van het silicium.
func cageState(i int) string {
	if why := cageWhy(i); why != "" {
		return why
	}
	if i < 1 || i >= len(cageIdentCache) {
		return ""
	}
	diagMu.Lock()
	defer diagMu.Unlock()
	if cageIdentCache[i] == "" {
		cageIdentCache[i] = cageIdent(i)
	}
	return cageIdentCache[i]
}

// cageIdentCache: de kooi-identiteit per slot (onder diagMu). Het is de
// identiteit van het silicium, dus hij verandert niet — maar hij wordt gevuld
// door wie er als eerste naar vraagt, en dat kan elke lezer zijn.
var cageIdentCache [layout.SlotCap + 1]string

// identOnce meldt één keer per boot wat voor hart er onder een slot zit
// (cageIdent — leeg waar de architectuur niets achter de kooi-naad verstopt).
// Aangeroepen door de servicer op het moment dat een slot voor het eerst leeft:
// dan is de kooi-stub aantoonbaar gelopen en staat HOP er toch al, dus geen
// eigen poll. Eén regel, want het antwoord verandert niet en elke console-regel
// is er één te veel. WaitReady zou hier niet werken — dat gebruiken alleen de
// demo-mains, niet het agent-pad.
var identDone bool // onder diagMu

func identOnce(i int) {
	diagMu.Lock()
	s := ""
	if !identDone {
		if s = cageIdent(i); s != "" {
			identDone = true
		}
	}
	diagMu.Unlock()
	// Printen ná de unlock: op een board waar de console een 115200-UART is duurt
	// één regel milliseconden, en die mag Get niet ophouden.
	if s != "" {
		fmt.Println(s)
	}
}

// WaitReady wacht tot de app in slot i StatusReady meldt.
func WaitReady(i int, timeout time.Duration) error {
	if err := checkSlot(i); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ctrlRead(i, layout.CtrlStatus) == layout.StatusReady {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("slot %d not ready within %v", i, timeout)
}

// Logs geeft het logkanaal van de actieve servicer van slot i (gevuld uit de
// hop-ABI-outbox); het kanaal sluit zodra de core uit is, de ring corrupt
// blijkt, of een nieuwe Start de servicer verdringt. Dit voedt HOP's
// LogBroadcaster. Zonder actieve servicer: een gesloten kanaal.
func Logs(i int) <-chan string {
	svcMu.Lock()
	s := servicers[i]
	svcMu.Unlock()
	if s == nil {
		ch := make(chan string)
		close(ch)
		return ch
	}
	return s.logs
}

var (
	numSlotsOnce sync.Once
	numSlots     int
)

// NumSlots is het aantal bruikbare app-slots: cores 1..MaxSlots die PSCI
// herkent. Het layout reserveert MaxSlots plekken, maar een node kan minder
// cores hebben (QEMU -smp < MaxSlots+1, of een kleiner board). Zonder deze
// probe adverteert HOP slots zonder core: allocateSlot kiest er een, Start
// doet AFFINITY_INFO → PSCI INVALID_PARAMS → "core niet uit" → de job is
// permanent onplaatsbaar. We tellen de aaneengesloten bestaande cores, één
// keer (de topologie ligt vast na boot).
func NumSlots() int {
	numSlotsOnce.Do(func() {
		// Hier stond een veeg van álle control-pages: verse DRAM is geen nul
		// (QEMU verhulde dat — Pi-meting 2026-07-10) en een nooit-gestart slot
		// rapporteerde dan garbage als status. Dat kan niet meer: de
		// control-page van een slot woont in zijn partitie, dus een slot zónder
		// partitie heeft er geen (ctrlRead geeft 0 = StatusEmpty) en een slot
		// mét partitie krijgt hem vers geveegd in armSlot.
		// PSCI-telling: schuif de grens op zolang een core een écht power-woord
		// meldt; stop bij het eerste antwoord buiten {On,Off,OnPending} — dat is
		// een ontbrekende core (INVALID_PARAMS) óf een PSCI-fout/onimplementatie.
		// We onthouden of we op zo'n fout stopten (i.p.v. netjes MaxSlots te
		// halen), zodat de diagnose hieronder het verschil kan benoemen.
		probed := 0
		truncated := false
		for i := 1; i <= layout.NumAppCores(); i++ {
			if coreExists(i) {
				probed = i // geldige core: schuif de grens op
			} else {
				truncated = true // PSCI-fout/ontbrekende core: stop de telling
			}
			if truncated {
				break
			}
		}
		numSlots = probed

		// Board-hint: een board dat weet hoeveel app-cores het heeft (PSCI
		// AFFINITY_INFO onbetrouwbaar op sommige silicium) mag dat declareren via
		// board.CoreCountHinter. Boards met werkende AFFINITY_INFO (QEMU, Pi)
		// doen dat niet — dan blijft de PSCI-telling leidend.
		hint := 0
		if h, ok := board.Current().(board.CoreCountHinter); ok {
			hint = h.ExpectedAppCores()
		}
		switch {
		case hint > 0 && probed < hint:
			if hint > layout.NumAppCores() {
				hint = layout.NumAppCores() // niet meer dan de fysieke app-cores
			}
			fmt.Printf("HOPOS_NUMSLOTS_HINT: PSCI telde %d app-core(s) (getrunceerd=%v), board declareert er %d — de board-hint is leidend\n",
				probed, truncated, hint)
			numSlots = hint
		case probed == 0:
			// Geen enkele app-core én geen board-hint: HOP zou nul slots
			// adverteren en elke job permanent onplaatsbaar maken. Luid, niet stil.
			fmt.Println("HOPOS_NUMSLOTS_ZERO: geen enkele app-core via PSCI AFFINITY_INFO (core 1 gaf al een fout/INVALID_PARAMS) — HOP adverteert 0 slots; is AFFINITY_INFO op dit board geïmplementeerd? Een board.CoreCountHinter kan dit overbruggen")
		}
	})
	return numSlots
}

// hopReserved is het aantal EXTRA cores (naast core 0, dat altijd HOP is) dat
// de node-runtime voor zichzelf houdt: cores 1..hopReserved draaien HOP-Go-Ms
// (GOMAXPROCS), niet apps. Default 0 (alleen core 0 = HOP, alle andere cores
// zijn app-slots — het huidige gedrag). Gezet door de node uit de platform-
// config (main.go SetHopCores); HopOS leest de config, HOP-userspace blijft
// oblivious en krijgt via slotmgr simpelweg minder slots aangeboden.
var hopReserved = 0

// SetHopCores zet het aantal cores voor de HOP-runtime (≥1: core 0 telt mee).
// n=1 → hopReserved=0 (geen reservering). Aanroepen vóór de eerste NumSlots.
func SetHopCores(n int) {
	if n < 1 {
		n = 1
	}
	hopReserved = n - 1
}

// HopReserved is de core-offset tussen een HOP-slot (1-based, zoals HOP ze telt)
// en de interne slot/core-index: intern = HOP-slot + HopReserved. slotmgr past
// 'm toe zodat slots.* zelf onveranderd op slot=core=layout kan blijven werken.
func HopReserved() int { return hopReserved }

// CoreClass geeft de cluster-klasse van slot i. De indeling is board-kennis
// (de O6N-tri-clustertopologie), dus komt van het actieve board — slots kent
// hem niet zelf. Blijft hier als dunne doorgeef voor slotmgr.
func CoreClass(i int) string { return board.Current().CoreClass(i) }

// withDNS geeft een kopie van env met HOP_DNS gezet op de node-resolver,
// tenzij dns leeg is of de job de sleutel al koos. Kopie: de env-map is van de
// aanroeper (HOP's Job), die muteren we niet.
func withDNS(env map[string]string, dns string) map[string]string {
	if dns == "" {
		return env
	}
	if _, set := env["HOP_DNS"]; set {
		return env
	}
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	out["HOP_DNS"] = dns
	return out
}

// encodeEnv serialiseert een env-map tot "key=val\n"-bytes (stabiele volgorde
// niet nodig; de app leest per regel).
func encodeEnv(env map[string]string) []byte {
	if len(env) == 0 {
		return nil
	}
	var b []byte
	for k, v := range env {
		b = append(b, k...)
		b = append(b, '=')
		b = append(b, v...)
		b = append(b, '\n')
	}
	return b
}
