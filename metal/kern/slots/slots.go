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
	"hop-os/metal/kern/stage2"
	"hop-os/metal/net/hopswitch"
)

// Eén servicer per slot: de outbox is SPSC, dus er mag nooit meer dan één
// consumer leven. De servicer draint logs (TypeLog) én bedient hop-ABI-RPC's
// (TypeRPCReq → fs/fetch → TypeRPCResp op de inbox). Start verdringt de oude
// servicer synchroon vóór de ringen opnieuw geïnitialiseerd worden — anders
// kan bij een snelle Stop→Start een oude naast de nieuwe blijven lezen (twee
// schrijvers op tail). Alles draait op de HOP-kern: Go-synchronisatie volstaat.
type servicer struct {
	slot   int
	stop   chan struct{} // gesloten: servicer moet weg
	done   chan struct{} // gesloten zodra de servicer weg is
	logs   chan string   // logregels (drop bij trage lezer)
	root   string        // eigen (lege) hopfs-root van deze task
	mounts [][2]string   // {local, shared}, langste local eerst
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
	if i+cores-1 > layout.MaxSlots {
		return nil, nil, 0, fmt.Errorf("SMP: %d cores vanaf slot %d overschrijden MaxSlots %d", cores, i, layout.MaxSlots)
	}
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
		close(old.stop)
		<-old.done
	}
}

// registerServicer registreert de nieuwe servicer van slot i (nog niet
// gestart — placeFromStaging start hem ná de ring-init). Verdringen hoeft
// hier niet meer: evictServicer draait altijd eerder op hetzelfde pad, onder
// dezelfde lifecycleMu, en registratie gebeurt nérgens anders — er kán dus
// geen oude servicer meer staan.
func registerServicer(i int, root string, mounts [][2]string) *servicer {
	s := &servicer{
		slot:   i,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		logs:   make(chan string, 64),
		root:   root,
		mounts: mounts,
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
	out := ring.Open(layout.RingOutboxPA(s.slot))
	in := ring.Open(layout.RingInboxPA(s.slot))
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
			// Geen outbox-werk: stoppen zodra de core parkeerde (niet meer
			// draait) of de ring corrupt is.
			if out.Corrupt() || !coreRunning(s.slot) {
				return
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
			select {
			case s.logs <- string(p):
			default: // trage lezer: drop i.p.v. de app blokkeren
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
	// Vertrouwde core-telling uit HOP-geheugen (slotCores), NOOIT ctrlRead
	// (CtrlCores) — die page is app-schrijfbaar; een opgehoogde CtrlCores zou
	// anders een app buurcores in zijn kooi laten trekken. Zie smp.go.
	cores := coreCount(s.slot)
	if c <= s.slot || c > s.slot+cores-1 || c > layout.MaxSlots {
		// Buiten het toegewezen core-bereik: weiger (de app hoort dit niet te
		// vragen). Verzoek intrekken zodat de app niet eeuwig wacht.
		fmt.Printf("HOPOS_SMP_REJECT slot %d: core %d outside [%d,%d]\n", s.slot, c, s.slot+1, s.slot+cores-1)
		ctrlWrite(s.slot, layout.CtrlSMPReq, 0)
		dev.MB()
		return
	}
	ctrlWrite(s.slot, layout.CtrlSMPMbox, uint64(layout.ParkMboxPA(c)))
	dev.MB()
	if err := dispatchCore(c, board.Current().S2SMPTrampPC(), uint64(layout.CtrlPagePA(s.slot))); err != nil {
		fmt.Printf("HOPOS_SMP_DISPATCH_FAIL slot %d core %d: %v\n", s.slot, c, err)
	}
	ctrlWrite(s.slot, layout.CtrlSMPReq, 0) // app-handshake: verzoek afgehandeld
	dev.MB()
}

// ctrlRead/ctrlWrite: 64-bit velden op een control-page (device-gemapt).
// HOP-kant: fysiek adres uit het board-plan (de app leest dezelfde page via
// zijn IPA; de stage-2 verbindt de twee).
func ctrlRead(slot int, off uintptr) uint64 {
	return dev.Read64(layout.CtrlPagePA(slot) + off)
}

func ctrlWrite(slot int, off uintptr, v uint64) {
	dev.Write64(layout.CtrlPagePA(slot)+off, v)
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
const (
	mboxCold   = 0
	mboxParked = 1
)

func mboxWord0(core int) uint64 { return dev.Read64(layout.ParkMboxPA(core)) }

// coreRunning is true zolang de app op deze core draait (nog niet geparkeerd).
func coreRunning(core int) bool { return mboxWord0(core) >= 2 }

// dispatchCore start een core op entry met ctx in x0. Cold (nooit geparkeerd):
// PSCI CPU_ON — de éénmalige bring-up per core. Anders (geparkeerd): schrijf
// {ctx, entry} in de mailbox en wek de WFE-lus met SEV; die springt de
// (idempotente) trampoline in. Zet word0 sowieso op ctx zodat coreRunning klopt.
func dispatchCore(core int, entry, ctx uint64) error {
	// Nooit een core dispatchen die al draait: dat zou een app (of een tweede
	// Start) een core midden in de uitvoering laten kapen. Start's pad checkt dit
	// al vóór de aanroep (coreRunning-lus), maar de lazy-SMP-weg (dispatchSMP,
	// met een app-beïnvloed core-nummer uit CtrlSMPReq) komt hier ook langs — dus
	// de guard hoort hier, op het gedeelde punt.
	if coreRunning(core) {
		return fmt.Errorf("core %d draait al (mailbox word0 >= 2) — dispatch geweigerd", core)
	}
	mbox := layout.ParkMboxPA(core)
	cold := dev.Read64(mbox) == mboxCold
	dev.Write64(mbox+8, entry) // word1 = doel-PC
	dev.Write64(mbox+0, ctx)   // word0 = ctx (= "running")
	dev.MB()
	if cold {
		if ret := board.Current().CPUOn(uint64(core), entry, ctx); ret != board.PSCISuccess {
			return fmt.Errorf("PSCI CPU_ON core %d: %d", core, ret)
		}
		return nil
	}
	dev.SEV() // geparkeerde core: wek de WFE-lus
	return nil
}

// waitStopped polt tot core geparkeerd is (mailbox word0 == mboxParked).
func waitStopped(core int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if mboxWord0(core) == mboxParked {
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

	// Door de EL2-vectoren gerapporteerd bij een onvrijwillig einde:
	// FaultVec = layout.FaultSync (stage-2-fault; ESR/FAR geldig) — zowel bij
	// een spontane kooi-overtreding als bij HOP's hard-kill (stage2.Revoke).
	// layout.FaultNone = geen fault gezien.
	FaultVec uint64
	FaultESR uint64
	FaultFAR uint64
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
func Start(i int, image []byte, memLimit uint64, cores int, env map[string]string, mounts map[string]string, ports map[string]int) error {
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
	vectorsOnce.Do(stage2.InitVectors)
	if err := coresFree(i, cores, "not parked/cold"); err != nil {
		return err
	}

	// De fysieke partitie éérst alloceren (partAlloc heeft alleen i+memLimit
	// nodig, niet het linkadres): we kopiëren de image erin vóór we hem
	// parsen. started markeert een geslaagde start: valt Start eerder
	// uit, dan geeft de defer de gealloceerde partitie terug.
	base, err := partAlloc(i, memLimit)
	if err != nil {
		return err
	}
	// Eén regel per plaatsing: op een headless node is dít hoe je ziet wáár
	// een slot fysiek landt (sinds 15-07 ook boven de 512GB-grens).
	fmt.Printf("slot %d: partition %d MB @ %#x\n", i, size>>20, base)
	var started bool
	defer func() {
		if !started {
			partRelease(i)
		}
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

	if err := placeFromStaging(i, base, size, stageAddr, imgSize, memLimit, cores, envBlob, mtab, ports); err != nil {
		return err
	}
	started = true // partitie blijft van deze task tot Stop
	fmt.Printf("slot %d: image placed in %s\n", i, time.Since(t0).Round(time.Millisecond))
	return nil
}

// StartLoader laadt de universele apploader (metal/app/apploader, ingebakken via
// apploaderblob) in slot i op één core. Die downloadt op zíjn eigen core+netstack
// de echte app-image (env HOP_IMAGE_URL) zijn eigen partitie in en seint
// "staged"; HOP plaatst 'm dan met StartStaged. De partitie wordt op memLimit
// gealloceerd — die de echte app in fase 2 hergebruikt. De loader is een gedeelde
// ingebakken blob (geen fetch, geen per-start-kopie). Vereist -tags embedloader.
func StartLoader(i int, memLimit uint64, env map[string]string) error {
	img := apploaderblob.Loader()
	if len(img) == 0 {
		return fmt.Errorf("apploader niet ingebakken of uitpakken faalde (bouw de node met -tags embedloader)")
	}
	return Start(i, img, memLimit, 1, env, nil, nil)
}

// appRAMSize is het deel van de partitie dat de app als RAM ziet: de bovenste
// NetRingStride is zijn net-ring ("512MB → 510 Go + 2 netbuffer"). Zo komt het
// ring-geheugen uit de eigen memLimit van de job — er draait geen statische
// SlotCap-reservering meer in het board-plan — en blijft de coherentie gratis:
// de app declareert de staart niet als RAM (zijn stage-1 mapt hem nooit
// cacheable), HOP raakt hem alleen device-side, en de bestaande CleanInv over
// de hele partitie veegt de dirty lines van de vórige huurder.
func appRAMSize(size uint64) (uint64, error) {
	if size < 2*layout.NetRingStride {
		return 0, fmt.Errorf("memLimit te klein: partitie %d MB laat geen app-RAM over naast de %d MB net-ring",
			size>>20, uint64(layout.NetRingStride)>>20)
	}
	return size - layout.NetRingStride, nil
}

// placeFromStaging is de tweede helft van een slot-start: de image staat al in
// de staging bovenin de partitie — óf door Start (een ingebakken blob, zoals de
// apploader), óf door de apploader vanaf zíjn eigen download (StartStaged). Van
// hieraf is alles geprivilegieerd HOP-werk: ELF parsen, segmenten plaatsen,
// RAM-symbolen patchen, stage-2 bouwen en de core (her)dispatchen. Eén bron van
// waarheid voor beide startpaden.
func placeFromStaging(i int, base, size uint64, stageAddr uintptr, imgSize int64, memLimit uint64, cores int, envBlob []byte, mtab [][2]string, ports map[string]int) error {
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
	// linkvenster is het canonieke contract (slot-1-basis, de stage-2 is de
	// relocatie) en het plafond de staging-onderkant: segmenten mogen hun
	// eigen kopieerbron niet raken.
	linkBase := uint64(layout.SlotBase(1))
	plan, err := place.Build(devReaderAt{base: stageAddr, size: imgSize}, imgSize,
		linkBase, appRAM, uint64(stageAddr)-base, i)
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

	return armSlot(i, base, size, plan.Entry, memLimit, cores, envBlob, mtab, ports)
}

// armSlot is de gedeelde slotstart-staart: servicer/switch-hygiëne, verse
// control-page + ringen, stage-2-kooi, en de (her)dispatch van de core op
// entry. Twee aanroepers: placeFromStaging (HOP plaatste de bytes zelf, entry
// = de app-entry uit de ELF) en het zelfplaats-pad van StartStaged (de loader
// plaatste voor, entry = het stubje dat op de eigen core de segmenten schuift
// en dan de app inspringt — zie applib/selfplace.go).
func armSlot(i int, base, size uint64, entry, memLimit uint64, cores int, envBlob []byte, mtab [][2]string, ports map[string]int) error {
	appRAM, err := appRAMSize(size)
	if err != nil {
		return err
	}
	netPA := base + appRAM
	// Entry bepaalt het canonieke IPA-venster (zelfde afleiding als de
	// ELF-route); voor het zelfplaats-pad is dit tevens de hygiëne-check op
	// het onvertrouwde CtrlPlaceEntry — buiten de kooi wijzen kán niet (de
	// stage-2 vertaalt alleen het eigen venster), maar gericht falen is netter.
	if entry < layout.SlotsBase || entry >= layout.SlotsBase+uint64(layout.MaxSlots)*uint64(layout.SlotStride) {
		return fmt.Errorf("entry %#x outside every slot range", entry)
	}
	linked := int((entry-layout.SlotsBase)/layout.SlotStride) + 1
	linkBase := layout.SlotBase(linked)
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
	dev.Clear(layout.CtrlPagePA(i), layout.CtrlStride)
	if len(envBlob) > 0 {
		dev.Copy(layout.CtrlPagePA(i)+layout.CtrlEnvData, envBlob)
	}
	ctrlWrite(i, layout.CtrlEnvLen, uint64(len(envBlob)))
	// Klok doorgeven: de teller is gedeeld, dus HOP's offset geldt 1-op-1.
	ctrlWrite(i, layout.CtrlWallOff, uint64(board.Current().TimerOffset()))
	// Geen net-config meer op de control-page: elke task heeft altijd een adres
	// op het interne net en leidt IP/gateway/MAC deterministisch af uit zijn
	// slotnummer (layout-net-plan, gedeeld met de switch); de app initieert een
	// stack pas als hij appnet.Up aanroept.
	ring.Init(layout.RingOutboxPA(i), layout.RingDataCap)
	ring.Init(layout.RingInboxPA(i), layout.RingDataCap)
	ring.Init(uintptr(netPA)+layout.NetTXOff, layout.NetRingDataCap)
	ring.Init(uintptr(netPA)+layout.NetRXOff, layout.NetRingDataCap)

	// De core krijgt stage-2-isolatie: de EL2-trampoline activeert de hier
	// gebouwde tabel en dropt pas dan naar de app-entry (een canoniek IPA — de
	// stage-2 vertaalt hem naar deze partitie). De app-image draait nooit op
	// EL2. De trampoline is data-gedreven: alles staat op deze control-page.
	l1, err := stage2.Build(i, linkBase, base, size, netPA)
	if err != nil {
		return fmt.Errorf("stage-2 slot %d: %w", i, err)
	}
	ctrlWrite(i, layout.CtrlEntry, entry)
	ctrlWrite(i, layout.CtrlS2Table, l1)
	ctrlWrite(i, layout.CtrlVecPA, uint64(layout.VecBasePA()))
	ctrlWrite(i, layout.CtrlSlot, uint64(i))
	ctrlWrite(i, layout.CtrlMboxPA, uint64(layout.ParkMboxPA(i))) // → TPIDR_EL2
	// Het aantal cores op de control-page; de app-OS-laag leest 'm en vraagt bij
	// cores > 1 de extra cores lazy op (CtrlSMPReq → HOP dispatcht). Altijd
	// zetten (ook 1 = gewone app), zodat de app-kant niet hoeft te weten of dit
	// SMP is. LET OP: dit is HOP → app-informatie; HOP vertrouwt de readback
	// NOOIT (de app kan de page herschrijven). De vertrouwde bron voor HOP's
	// eigen beslissingen is slotCores, hier uit het al-gevalideerde `cores` gezet.
	slotCores[i] = cores
	ctrlWrite(i, layout.CtrlCores, uint64(cores))
	if cores > 1 {
		// Fysiek adres van de EL2 SMP-trampoline publiceren (op ditzelfde slot
		// z'n partitie/stage-2 → gedeelde heap).
		ctrlWrite(i, layout.CtrlSMPTramp, board.Current().S2SMPTrampPC())
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

	// Startschot: cold → PSCI CPU_ON (eerste bring-up), geparkeerd → mailbox +
	// SEV. Ctx = de fysieke control-page; de trampoline leest er alles van.
	if err := dispatchCore(i, board.Current().S2TrampPC(), uint64(layout.CtrlPagePA(i))); err != nil {
		return err
	}

	go registerServicer(i, root, mtab).run()

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
func StartStaged(i int, memLimit uint64, cores int, env map[string]string, mounts map[string]string, ports map[string]int) error {
	// Pure invoervalidatie vóór het venster — gedeeld met Start (prepStart).
	mtab, envBlob, cores, err := prepStart(i, memLimit, cores, env, mounts, ports)
	if err != nil {
		return err
	}
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
	vectorsOnce.Do(stage2.InitVectors)
	// De apploader parkeerde na het seinen; zijn core (en de secundaire cores
	// van een SMP-app) mogen niet meer draaien vóór we eroverheen plaatsen.
	if err := coresFree(i, cores, "loader not parked?"); err != nil {
		return err
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
		if err := armSlot(i, base, size, placeEntry, memLimit, cores, envBlob, mtab, ports); err != nil {
			return err
		}
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
	dev.CleanInv(uintptr(base), uintptr(size))
	if err := placeFromStaging(i, base, size, stageAddr, imgSize, memLimit, cores, envBlob, mtab, ports); err != nil {
		return err
	}
	fmt.Printf("slot %d: staged app placed in %s\n", i, time.Since(t0).Round(time.Millisecond))
	return nil
}

var vectorsOnce sync.Once

// EnsureVectors zet de EL2-vectoren + parkeerlus + revoke-vectoren (idempotent
// via vectorsOnce, gedeeld met Start). De node roept dit vóór smp.ConfigureNode
// aan zodat opkomende node-cores hun VBAR_EL2 (= revoke-vectoren, net als core 0
// uit bootKernel) geldig aantreffen. Later in Start is de vectorsOnce een no-op.
func EnsureVectors() { vectorsOnce.Do(stage2.InitVectors) }

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
	// Coöperatieve kans voor de app (de kill-watcher parkeert zijn eigen core).
	waitStopped(i, timeout)
	// Draait er nog iets? Hoeveel cores de app heeft komt uit HOP's eigen
	// slotCores (door Start gezet) — NIET uit de app-schrijfbare CtrlCores: een
	// verlaagde CtrlCores zou anders levende secundaire cores voor deze scan
	// verbergen en releaseSlot een nog-draaiende partitie laten vrijgeven.
	n := coreCount(i)
	stillOn := false
	for c := i; c < i+n; c++ {
		if coreRunning(c) {
			stillOn = true
			break
		}
	}
	var stopErr error
	if stillOn {
		// Eén intrekking velt álle cores van het slot (gedeelde tabel/VMID).
		stage2.Revoke(i)
		for c := i; c < i+n; c++ {
			if !coreRunning(c) {
				continue
			}
			if !waitStopped(c, time.Second) {
				stopErr = fmt.Errorf("slot %d: core %d did not park even after stage-2 revocation", i, c)
			}
		}
	}
	releaseSlot(i)
	return stopErr
}

// releaseSlot maakt een gestopt slot vrij: van de switch af, poorten in, en
// de partitie terug naar de pool (de cores zijn geparkeerd, dus niemand raakt
// het geheugen meer — pas bij een volgende Start worden ze her-gedispatcht).
func releaseSlot(i int) {
	hopswitch.Detach(i)
	hopswitch.UnpublishSlot(i)
	partRelease(i)
	if i >= 1 && i <= layout.MaxSlots {
		slotCores[i] = 0 // vertrouwde core-telling wissen (zie smp.go)
	}
}

// Get geeft de actuele status van slot i (nulwaarde bij ongeldige index).
func Get(i int) Status {
	if checkSlot(i) != nil {
		return Status{}
	}
	return Status{
		// CoreOn = de app draait (mailbox word0 draagt de ctx). Een geparkeerde
		// (gestopte) of cold core telt als niet-aan. Dit is nu de mailbox, niet
		// PSCI AFFINITY_INFO: geparkeerde cores staan PSCI-gezien nog "on" (ze
		// deden nooit CPU_OFF), maar draaien geen app meer.
		CoreOn:    coreRunning(i),
		App:       ctrlRead(i, layout.CtrlStatus),
		ExitCode:  ctrlRead(i, layout.CtrlExitCode),
		Heartbeat: ctrlRead(i, layout.CtrlHeartbeat),
		RAMSize:   ctrlRead(i, layout.CtrlRAMSize),
		MemSys:    ctrlRead(i, layout.CtrlMemSys),
		FaultVec:  ctrlRead(i, layout.CtrlFaultVec),
		FaultESR:  ctrlRead(i, layout.CtrlFaultESR),
		FaultFAR:  ctrlRead(i, layout.CtrlFaultFAR),
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
		// Verse DRAM is geen nul (QEMU verhulde dat — Pi-meting 2026-07-10):
		// de control-page van een nooit-gestart slot zou anders garbage als
		// status/fault rapporteren. Eén keer vegen, vóór de eerste Get/Start.
		for i := 1; i <= layout.MaxSlots; i++ {
			dev.Clear(layout.CtrlPagePA(i), layout.CtrlStride)
		}
		// PSCI-telling: schuif de grens op zolang een core een écht power-woord
		// meldt; stop bij het eerste antwoord buiten {On,Off,OnPending} — dat is
		// een ontbrekende core (INVALID_PARAMS) óf een PSCI-fout/onimplementatie.
		// We onthouden of we op zo'n fout stopten (i.p.v. netjes MaxSlots te
		// halen), zodat de diagnose hieronder het verschil kan benoemen.
		probed := 0
		truncated := false
		for i := 1; i <= layout.MaxSlots; i++ {
			switch board.Current().AffinityInfo(uint64(i)) {
			case board.PowerOn, board.PowerOff, board.PowerOnPending:
				probed = i // geldige core: schuif de grens op
			default:
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
			if hint > layout.MaxSlots {
				hint = layout.MaxSlots // het layout reserveert niet meer dan dit
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

// AppSlotCount is het aantal slots dat HOP mag gebruiken: de ontdekte app-cores
// minus de voor de HOP-runtime gereserveerde. Dit voedt slotmgr.NumSlots.
func AppSlotCount() int {
	if c := NumSlots() - hopReserved; c > 0 {
		return c
	}
	return 0
}

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
