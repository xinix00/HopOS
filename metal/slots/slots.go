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
	"bytes"
	"debug/elf"
	"fmt"
	"io"
	"sync"
	"time"

	"hop-os/metal/board"
	"hop-os/metal/dev"
	"hop-os/metal/hopswitch"
	"hop-os/metal/layout"
	"hop-os/metal/ring"
	"hop-os/metal/stage2"
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

// claimServicer verdringt de oude servicer van slot i en registreert een
// nieuwe (nog niet gestart — Start doet dat na de ring-init).
func claimServicer(i int, root string, mounts [][2]string) *servicer {
	s := &servicer{
		slot:   i,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		logs:   make(chan string, 64),
		root:   root,
		mounts: mounts,
	}
	svcMu.Lock()
	old := servicers[i]
	servicers[i] = s
	svcMu.Unlock()
	if old != nil {
		close(old.stop)
		<-old.done
	}
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

// Namen van de runtime-symbolen die bij het laden gepatcht worden. Zo wordt
// HOP's job.MemoryLimit letterlijk de RAM-declaratie van de app-runtime —
// een harde fysieke grens, nul handhavingscode.
const (
	symRAMStart = "runtime/goos.RamStart"
	symRAMSize  = "runtime/goos.RamSize"
	// Slot-identiteit voor app-images op UEFI/ACPI-servers (zie de patch in
	// Start; alleen gepatcht als het symbool bestaat).
	symSlotHint = "hop-os/metal/board/uefi.slotHint"
)

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

// Start laadt image in slot i (1-based, = core-index), patcht de
// RAM-declaratie naar memLimit en wekt de core. De image is een gewone
// tamago-ELF, canoniek gelinkt (TEXT_START = SlotBase(1)+0x10000): de
// stage-2-map legt het canonieke bereik op de partitie van dít slot, dus
// één artifact draait op elk slot.
//
// mounts is de volume-tabel van de task (shared path → local path, de vorm
// van HOP's Job.Volumes): de task ziet zijn eigen lege root plus uitsluitend
// de gemounte shared dirs — de toegangsgrens op storage, zoals de
// stage-2-kooi dat op geheugen is. Een shared dir ontstaat bij de eerste
// mount. Vereist een storage-laag (UseFS); zonder mounts is fs optioneel.
//
// ports (naam → poort, HOP's Task.Ports) worden na de start gepubliceerd:
// node-IP:poort → dit slot (stateloze DNAT bij de switch), zelfde nummer
// aan beide kanten — de app leest hem uit ER_PORT_* en bindt hem zelf.
func Start(i int, image []byte, memLimit uint64, cores int, env map[string]string, mounts map[string]string, ports map[string]int) error {
	return StartStream(i, bytes.NewReader(image), int64(len(image)), memLimit, cores, env, mounts, ports)
}

// devReaderAt is een io.ReaderAt over een stuk device-geheugen (de partitie-
// staging waar StartStream de image in streamt) — zo parseert debug/elf de
// ELF zonder dat het bestand ooit volledig in de kern-RAM staat.
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

// StartStream is Start's streamende kern: het leest de image uit src (een
// io.Reader — een download-body of een bytes.Reader) rechtstreeks de
// slot-partitie in, en parseert+plaatst de ELF vanuit dát device-geheugen.
// Zo is core 0 nooit de download-trechter: de kern houdt per fetch alleen een
// kleine stream-buffer vast (dev.Move/CopyOut werken op vaste stack-buffers),
// niet de hele image — 127 gelijktijdige fetches passen zo probleemloos, en
// een te grote/kapotte image raakt hooguit zijn eigen partitie. size is de
// verwachte bytegrootte (Content-Length; de []byte-wrapper geeft len).
func StartStream(i int, src io.Reader, imgSize int64, memLimit uint64, cores int, env map[string]string, mounts map[string]string, ports map[string]int) error {
	if err := checkSlot(i); err != nil {
		return err
	}
	if imgSize <= 0 {
		return fmt.Errorf("StartStream: onbekende/lege image-grootte %d (Content-Length vereist)", imgSize)
	}
	// Eén lifecycle tegelijk, in een DMA-stil venster (zie lifecycleMu):
	// gewone defers dekken álle paden — het venster sluit pas na de
	// WaitReady onderaan, zodat ook de app-boot (heap-zeroing) erbinnen valt.
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	pace()
	quiesce(true)
	drain()
	defer quiesce(false)
	for name, p := range ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("poort %q: %d ongeldig", name, p)
		}
	}
	mtab, err := mountTable(mounts)
	if err != nil {
		return err
	}
	if memLimit == 0 {
		return fmt.Errorf("memLimit 0 ongeldig")
	}
	// De EL2-vectoren + parkeerlus + mailboxen moeten klaar zijn vóór de
	// eerste dispatch (mailbox cold-detectie leest een geveegde mailbox).
	vectorsOnce.Do(stage2.InitVectors)

	// SMP (fase 5): cores ≥ 1. cores > 1 = één app over meerdere cores met een
	// gedeelde heap op de partitie van dít slot; de OS-laag vraagt de extra
	// cores lazy op (goos.Task → CtrlSMPReq → HOP dispatcht). De cores moeten
	// binnen bereik en beschikbaar zijn — d.w.z. niet draaiend (geparkeerd of
	// cold mag: dat is precies een core die HOP kan (her)starten).
	if cores < 1 {
		cores = 1
	}
	if i+cores-1 > layout.MaxSlots {
		return fmt.Errorf("SMP: %d cores vanaf slot %d overschrijden MaxSlots %d", cores, i, layout.MaxSlots)
	}
	for c := i; c < i+cores; c++ {
		if coreRunning(c) {
			return fmt.Errorf("core %d still running (not parked/cold)", c)
		}
	}
	// DNS-resolver van de node meegeven, zodat een app die naar buiten praat
	// (cloudflared, servers) namen kan opzoeken — de query loopt als gewoon
	// UDP door de masquerade. HOP zet 'm als env (net als ER_PORT_*), tenzij
	// de job 'm al expliciet koos. Leeg (Pi vóór P2) = geen HOP_DNS.
	envBlob := encodeEnv(withDNS(env, board.Current().Net().DNS))
	if len(envBlob) > layout.CtrlEnvMax {
		return fmt.Errorf("env te groot: %d > %d bytes", len(envBlob), layout.CtrlEnvMax)
	}

	// De fysieke partitie éérst alloceren (partAlloc heeft alleen i+memLimit
	// nodig, niet het linkadres): we streamen de image erin vóór we hem
	// parsen. started markeert een geslaagde start: valt StartStream eerder
	// uit, dan geeft de defer de gealloceerde partitie terug.
	base, err := partAlloc(i, memLimit)
	if err != nil {
		return err
	}
	size := align2M(memLimit)
	var started bool
	defer func() {
		if !started {
			partRelease(i)
		}
	}()

	// Coherentie vóór de ongecachte writes: de vórige huurder draaide
	// cacheable (hele heap); zijn dirty lines eerst wegschrijven+invalideren,
	// anders clobberen ze straks onze verse image (QEMU verhult dit — geen
	// caches; op de A76 echt, gemeten 2026-07-10). Hele partitie in één keer.
	dev.CleanInv(uintptr(base), uintptr(size))

	// De image de partitie in STREAMEN — bovenaan (staging), zodat de laag
	// geplaatste segmenten er niet mee botsen en core 0 nooit de hele image
	// vasthoudt (download-in-app-memory: een te grote/kapotte image raakt
	// hooguit deze partitie). 8-uitgelijnd zodat de device-reads netjes vallen.
	staged := (uint64(imgSize) + 7) &^ 7
	if staged >= size {
		return fmt.Errorf("image %d bytes past niet in partitie %d MB", imgSize, size>>20)
	}
	stageAddr := uintptr(base + size - staged)
	var buf [64 << 10]byte
	var got int64
	for got < imgSize {
		n, rerr := src.Read(buf[:])
		if n > 0 {
			if got+int64(n) > imgSize {
				return fmt.Errorf("image groter dan aangekondigd (%d > %d)", got+int64(n), imgSize)
			}
			dev.Copy(stageAddr+uintptr(got), buf[:n])
			got += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("image streamen: %w", rerr)
		}
	}
	if got != imgSize {
		return fmt.Errorf("image incompleet: %d van %d bytes", got, imgSize)
	}

	// Parsen vanuit de gestreamde device-kopie (geen kern-RAM-kopie).
	f, err := elf.NewFile(devReaderAt{base: stageAddr, size: imgSize})
	if err != nil {
		return fmt.Errorf("elf parse: %w", err)
	}

	// Het linkadres van de image bepaalt zijn IPA-bereik; de stage-2-map
	// legt dat op de fysieke partitie van dít slot. Images zijn canoniek
	// gelinkt (slot-1-bereik) en draaien zo op elk slot — de MMU is de
	// relocatie.
	if f.Entry < layout.SlotsBase || f.Entry >= layout.SlotsBase+uint64(layout.MaxSlots)*uint64(layout.SlotStride) {
		return fmt.Errorf("entry %#x outside every slot range", f.Entry)
	}
	linked := int((f.Entry-layout.SlotsBase)/layout.SlotStride) + 1
	linkBase := layout.SlotBase(linked)
	if max := maxLimitFor(linkBase); memLimit > max {
		return fmt.Errorf("memLimit %d MB > %d MB slot-cap (één GB-blok vanaf linkadres %#x, geklemd onder CtrlBase; groter vergt vensteruitbreiding — zie slots/partmem.go)", memLimit>>20, max>>20, linkBase)
	}
	delta := base - linkBase // PA = linkadres + delta (identiek slot: 0)

	// Segmenten naar de partitie, device→device (dev.Move, kleine stack-buffer
	// — geen kern-RAM voor de hele image). Headervelden zijn input:
	// overflow-veilig begrenzen, én het segment mag de staging bovenin niet
	// raken (anders overschrijven we de bron tijdens het kopiëren).
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if p.Filesz > p.Memsz || p.Memsz > size ||
			p.Paddr < linkBase || p.Paddr > linkBase+size-p.Memsz {
			return fmt.Errorf("segment %#x+%#x (file %#x) outside link range of slot %d (%#x+%#x)",
				p.Paddr, p.Memsz, p.Filesz, linked, linkBase, size)
		}
		if p.Paddr+delta+p.Memsz > uint64(stageAddr) {
			return fmt.Errorf("segment %#x+%#x botst met de image-staging (partitie te klein)", p.Paddr, p.Memsz)
		}
		dev.Move(uintptr(p.Paddr+delta), stageAddr+uintptr(p.Off), p.Filesz)
		dev.Clear(uintptr(p.Paddr+delta)+uintptr(p.Filesz), p.Memsz-p.Filesz)
	}

	// job.MemoryLimit → RAM-declaratie van de app-runtime patchen. RamStart
	// blijft het línkadres: dat is wat de app ziet (de stage-2 vertaalt).
	// (Vereist een niet-gestripte symboltabel: app-images bouwen met -w,
	// zonder -s.)
	syms, err := f.Symbols()
	if err != nil {
		return fmt.Errorf("symbols (app-image met -s gebouwd?): %w", err)
	}
	patched := 0
	for _, s := range syms {
		if s.Name != symRAMStart && s.Name != symRAMSize {
			continue
		}
		if s.Value%8 != 0 || s.Value < linkBase || s.Value > linkBase+size-8 {
			return fmt.Errorf("symbol %s (%#x) outside link range of slot %d", s.Name, s.Value, linked)
		}
		v := linkBase
		if s.Name == symRAMSize {
			v = memLimit
		}
		dev.Write64(uintptr(s.Value+delta), v)
		patched++
	}
	if patched != 2 {
		return fmt.Errorf("RAM symbols not found (%d/2 patched)", patched)
	}

	// Optioneel slot-hint-symbool (uefi-app-images): op servers is MPIDR
	// géén slotnummer (Altra: aff0 altijd 0), dus HOP vertelt de app bij
	// Start zíjn slot. Additief: Pi/virt-images hebben het symbool niet en
	// merken hier niets van.
	for _, s := range syms {
		if s.Name != symSlotHint {
			continue
		}
		if s.Value%8 == 0 && s.Value >= linkBase && s.Value <= linkBase+size-8 {
			dev.Write64(uintptr(s.Value+delta), uint64(i))
		}
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
	ring.Init(layout.NetRingTXPA(i), layout.NetRingDataCap)
	ring.Init(layout.NetRingRXPA(i), layout.NetRingDataCap)

	// De core krijgt stage-2-isolatie: de EL2-trampoline activeert de hier
	// gebouwde tabel en dropt pas dan naar de app-entry (een canoniek IPA — de
	// stage-2 vertaalt hem naar deze partitie). De app-image draait nooit op
	// EL2. De trampoline is data-gedreven: alles staat op deze control-page.
	l1, err := stage2.Build(i, linkBase, base, size)
	if err != nil {
		return fmt.Errorf("stage-2 slot %d: %w", i, err)
	}
	ctrlWrite(i, layout.CtrlEntry, f.Entry)
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
	hopswitch.Attach(i)
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

	started = true // partitie blijft van deze task tot Stop
	go claimServicer(i, root, mtab).run()

	// Synchroon wachten tot de app READY meldt (runtime-boot met heap-zeroing
	// en TLBI's klaar) — dan pas sluiten de defers het DMA-stille venster.
	// Best-effort deadline: een app die láng doet over z'n init houdt de
	// lifecycle niet eeuwig vast.
	_ = WaitReady(i, 3*time.Second)
	return nil
}

var vectorsOnce sync.Once

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
	// Eén lifecycle tegelijk, in een DMA-stil venster (zie lifecycleMu): ook
	// de coöperatieve kill parkeert een core — gemeten (13-07, torture):
	// kill+park naast lopende RX-DMA is dodelijk op C1.
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	pace()
	quiesce(true)
	drain()
	defer quiesce(false)
	ctrlWrite(i, layout.CtrlKill, 1)
	dev.MB()
	// Coöperatieve kans voor de app (de kill-watcher parkeert zijn eigen core).
	waitStopped(i, timeout)
	// Draait er nog iets? Hoeveel cores de app heeft komt uit HOP's eigen
	// slotCores (door Start gezet) — NIET uit de app-schrijfbare CtrlCores: een
	// verlaagde CtrlCores zou anders levende secundaire cores voor deze scan
	// verbergen en releaseSlot een nog-draaiende partitie laten vrijgeven.
	cores := appCores(i)
	stillOn := false
	for _, c := range cores {
		if coreRunning(c) {
			stillOn = true
			break
		}
	}
	var stopErr error
	if stillOn {
		// Eén intrekking velt álle cores van het slot (gedeelde tabel/VMID).
		stage2.Revoke(i)
		for _, c := range cores {
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
