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
	out := ring.Open(layout.RingOutbox(s.slot))
	in := ring.Open(layout.RingInbox(s.slot))
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
		typ, n, ok := out.ReadInto(buf)
		if !ok {
			if out.Corrupt() || board.Current().AffinityInfo(uint64(s.slot)) == board.PowerOff {
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

// Namen van de runtime-symbolen die bij het laden gepatcht worden. Zo wordt
// HOP's job.MemoryLimit letterlijk de RAM-declaratie van de app-runtime —
// een harde fysieke grens, nul handhavingscode.
const (
	symRAMStart = "runtime/goos.RamStart"
	symRAMSize  = "runtime/goos.RamSize"
)

// ctrlRead/ctrlWrite: 64-bit velden op een control-page (device-gemapt).
func ctrlRead(slot int, off uintptr) uint64 {
	return dev.Read64(layout.CtrlPage(slot) + off)
}

func ctrlWrite(slot int, off uintptr, v uint64) {
	dev.Write64(layout.CtrlPage(slot)+off, v)
}

// Status van een slot zoals HOP het ziet.
type Status struct {
	CoreOn    bool
	App       uint64 // layout.Status*-waarde
	ExitCode  uint64
	Heartbeat uint64
	RAMSize   uint64 // door de app gerapporteerde (gepatchte) RAM-maat

	// Door de EL2-vectoren gerapporteerd bij een onvrijwillig einde:
	// FaultVec = layout.FaultSync (stage-2-fault; ESR/FAR geldig) of
	// layout.FaultIRQ (hard-kill-SGI). layout.FaultNone = geen fault.
	FaultVec uint64
	FaultESR uint64
	FaultFAR uint64
}

// checkSlot valideert een slot-index; elke publieke functie begint hiermee —
// de control-page- en ringadressen worden er rechtstreeks uit berekend.
func checkSlot(i int) error {
	if i < 1 || i > layout.MaxSlots {
		return fmt.Errorf("slot %d buiten bereik 1..%d", i, layout.MaxSlots)
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
func Start(i int, image []byte, memLimit uint64, env map[string]string, mounts map[string]string, ports map[string]int) error {
	if err := checkSlot(i); err != nil {
		return err
	}
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
	// DNS-resolver van de node meegeven, zodat een app die naar buiten praat
	// (cloudflared, servers) namen kan opzoeken — de query loopt als gewoon
	// UDP door de masquerade. HOP zet 'm als env (net als ER_PORT_*), tenzij
	// de job 'm al expliciet koos. Leeg (Pi vóór P2) = geen HOP_DNS.
	envBlob := encodeEnv(withDNS(env, board.Current().Net().DNS))
	if len(envBlob) > layout.CtrlEnvMax {
		return fmt.Errorf("env te groot: %d > %d bytes", len(envBlob), layout.CtrlEnvMax)
	}
	if on := board.Current().AffinityInfo(uint64(i)); on != board.PowerOff {
		return fmt.Errorf("core %d is niet uit (AFFINITY_INFO=%d)", i, on)
	}

	f, err := elf.NewFile(bytes.NewReader(image))
	if err != nil {
		return fmt.Errorf("elf parse: %w", err)
	}

	// Het linkadres van de image bepaalt zijn IPA-bereik; de stage-2-map
	// legt dat op de fysieke partitie van dít slot. Images zijn canoniek
	// gelinkt (slot-1-bereik) en draaien zo op elk slot — de MMU is de
	// relocatie.
	if f.Entry < layout.SlotsBase || f.Entry >= layout.SlotsBase+layout.MaxSlots*uint64(layout.SlotStride) {
		return fmt.Errorf("entry %#x valt buiten elk slotbereik", f.Entry)
	}
	linked := int((f.Entry-layout.SlotsBase)/layout.SlotStride) + 1
	linkBase := layout.SlotBase(linked)

	// base/size = de fysieke partitie: dynamisch uit de pool gealloceerd op
	// precies memLimit (de een 128MB, de ander 640MB). started markeert een
	// geslaagde start: valt Start eerder uit, dan geeft de defer de
	// gealloceerde partitie terug.
	if max := maxLimitFor(linkBase); memLimit > max {
		return fmt.Errorf("memLimit %#x > %#x (één GB vanaf linkadres %#x; groter vergt vensteruitbreiding)", memLimit, max, linkBase)
	}
	base, err := partAlloc(i, memLimit)
	if err != nil {
		return err
	}
	size := align2M(memLimit)
	var started bool
	// Vanaf hier faalt niets zonder de partitie terug te geven.
	defer func() {
		if !started {
			partRelease(i)
		}
	}()
	delta := base - linkBase // PA = linkadres + delta (identiek slot: 0)

	// Segmenten naar de partitie. Headervelden zijn input — overflow-veilig
	// rekenen, anders wordt een corrupte image een geheugenveger. Bounds
	// gelden in het link-bereik; geschreven wordt op linkadres+delta.
	for _, p := range f.Progs {
		if p.Type != elf.PT_LOAD {
			continue
		}
		if p.Filesz > p.Memsz || p.Memsz > size ||
			p.Paddr < linkBase || p.Paddr > linkBase+size-p.Memsz {
			return fmt.Errorf("segment %#x+%#x (file %#x) valt buiten linkbereik slot %d (%#x+%#x)",
				p.Paddr, p.Memsz, p.Filesz, linked, linkBase, size)
		}
		buf := make([]byte, p.Filesz)
		if n, err := p.ReadAt(buf, 0); err != nil || uint64(n) != p.Filesz {
			return fmt.Errorf("segment lezen: %d/%d, %v", n, p.Filesz, err)
		}
		dev.Copy(uintptr(p.Paddr+delta), buf)
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
			return fmt.Errorf("symbool %s (%#x) valt buiten linkbereik slot %d", s.Name, s.Value, linked)
		}
		v := linkBase
		if s.Name == symRAMSize {
			v = memLimit
		}
		dev.Write64(uintptr(s.Value+delta), v)
		patched++
	}
	if patched != 2 {
		return fmt.Errorf("RAM-symbolen niet gevonden (%d/2 gepatcht)", patched)
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
		return fmt.Errorf("mounts gevraagd maar geen storage-laag (UseFS)")
	}

	// Control-page vegen, env-blob schrijven, hop-ABI-ringen klaarzetten,
	// BOOTING, core wekken.
	dev.Clear(layout.CtrlPage(i), layout.CtrlStride)
	if len(envBlob) > 0 {
		dev.Copy(layout.CtrlPage(i)+layout.CtrlEnvData, envBlob)
	}
	ctrlWrite(i, layout.CtrlEnvLen, uint64(len(envBlob)))
	// Klok doorgeven: de teller is gedeeld, dus HOP's offset geldt 1-op-1.
	ctrlWrite(i, layout.CtrlWallOff, uint64(board.Current().TimerOffset()))
	// Geen net-config meer op de control-page: elke task heeft altijd een adres
	// op het interne net en leidt IP/gateway/MAC deterministisch af uit zijn
	// slotnummer (layout-net-plan, gedeeld met de switch); de app initieert een
	// stack pas als hij appnet.Up aanroept.
	ring.Init(layout.RingOutbox(i), layout.RingDataCap)
	ring.Init(layout.RingInbox(i), layout.RingDataCap)
	ring.Init(layout.NetRingTX(i), layout.NetRingDataCap)
	ring.Init(layout.NetRingRX(i), layout.NetRingDataCap)

	// De core krijgt stage-2-isolatie: CPU_ON wijst naar HOP's EL2-trampoline
	// (ctx = slot) die de hier gebouwde tabel activeert en pas dan naar de
	// app-entry dropt (een canoniek IPA — de stage-2 vertaalt hem naar deze
	// partitie). De app-image draait nooit op EL2.
	vectorsOnce.Do(stage2.InitVectors)
	l1, err := stage2.Build(i, linkBase, base, size)
	if err != nil {
		return fmt.Errorf("stage-2 slot %d: %w", i, err)
	}
	ctrlWrite(i, layout.CtrlEntry, f.Entry)
	ctrlWrite(i, layout.CtrlS2Table, l1)
	// Een pending kill-SGI van een eerdere hard-kill zou de verse app
	// direct zijn core kosten.
	board.Current().SGIClearPending(uint64(i))
	ctrlWrite(i, layout.CtrlStatus, layout.StatusBooting)

	if ret := board.Current().CPUOn(uint64(i), board.Current().S2TrampPC(), uint64(i)); ret != board.PSCISuccess {
		return fmt.Errorf("PSCI CPU_ON slot %d: %d", i, ret)
	}

	hopswitch.Attach(i)
	for name, p := range ports {
		if err := hopswitch.Publish("tcp", uint16(p), i, uint16(p)); err != nil {
			return fmt.Errorf("poort %q: %w", name, err)
		}
	}
	started = true // partitie blijft van deze task tot Stop
	go claimServicer(i, root, mtab).run()
	return nil
}

var vectorsOnce sync.Once

// Stop vraagt de app in slot i zichzelf te beëindigen (kill-flag; de app-lib
// zet de core uit via CPU_OFF) en wacht tot de core uit is. Negeert de app de
// kill-flag (hang), dan volgt de hard-kill: een SGI die naar de EL2-vectoren
// trapt (HCR.IMO, door de app niet te maskeren) en de core via CPU_OFF
// uitzet — Status meldt dan layout.FaultIRQ.
func Stop(i int, timeout time.Duration) error {
	if err := checkSlot(i); err != nil {
		return err
	}
	ctrlWrite(i, layout.CtrlKill, 1)
	if waitOff(i, timeout) {
		releaseSlot(i)
		return nil
	}
	board.Current().SGIKill(uint64(i))
	if waitOff(i, time.Second) {
		releaseSlot(i)
		return nil
	}
	return fmt.Errorf("slot %d is ook na de hard-kill-SGI niet uit", i)
}

// releaseSlot maakt een gestopt slot vrij: van de switch af, poorten in, en
// de partitie terug naar de pool (de core is uit, dus niemand raakt hem meer).
func releaseSlot(i int) {
	hopswitch.Detach(i)
	hopswitch.UnpublishSlot(i)
	partRelease(i)
}

// waitOff polt tot de core van slot i uit is.
func waitOff(i int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if board.Current().AffinityInfo(uint64(i)) == board.PowerOff {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// Get geeft de actuele status van slot i (nulwaarde bij ongeldige index).
func Get(i int) Status {
	if checkSlot(i) != nil {
		return Status{}
	}
	return Status{
		// ON_PENDING telt als aan: vlak na CPU_ON is de core nog onderweg —
		// wie dat als "uit" rapporteert laat HOP een bootende app als crash
		// zien (en het restart-beleid onnodig ingrijpen).
		CoreOn:    board.Current().AffinityInfo(uint64(i)) != board.PowerOff,
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
	return fmt.Errorf("slot %d niet ready binnen %v", i, timeout)
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
		for i := 1; i <= layout.MaxSlots; i++ {
			switch board.Current().AffinityInfo(uint64(i)) {
			case board.PowerOn, board.PowerOff, board.PowerOnPending:
				numSlots = i // geldige core: schuif de grens op
			default:
				return // eerste ontbrekende core (INVALID_PARAMS): stop
			}
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
