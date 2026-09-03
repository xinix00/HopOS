// Package hopswitch is HOP's interne L2-frame-switch (per-slot netwerk):
// elke app-core draait een eigen netstack (applib/appnet) over rauwe
// Ethernet-frames door twee per-slot descriptorqueues; de payload blijft in
// app-RAM en HOP kopieert op de dst-MAC — app↔app-verkeer raakt nooit
// een TCP-stack op core 0. "Apps rekenen, HOP sjouwt data."
//
// HOP is poort 0 op hetzelfde LAN. Het system-callcontract bereikt daar HOP's
// gewone TCP-stack; gateway-ARP en verkeer naar buiten handelt de switch zelf:
//   - ARP voor de gateway (10.100.0.1) beantwoorden (arpReplyGateway);
//   - uitgaand verkeer naar buiten masqueraden en de antwoorden rechtstreeks
//     in de slot-ring terugleggen (nat.go/deliverLocked) — geen tunnel,
//     alleen header-herschrijving.
//
// Adressering is deterministisch, geen tabellen die leren: het net-plan
// (subnet, per-slot IP/MAC, gateway) leeft in metal/abi/layout, zodat de switch
// en de app-stacks nooit uiteenlopen. HOP is de gateway op .1 (MAC ..:00).
package hopswitch

import (
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/xinix00/HopOS/metal/v2/abi/frameq"
	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/abi/ring"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/net/netdev"
)

const (
	// maxBurst begrenst het aantal frames per poort per switch-ronde, zodat
	// één drukke poort de rest niet verhongert.
	maxBurst = 64

	// maxFrameLen is het grootste frame dat de stacks aan beide kanten
	// aanbieden. De ABI-ring kan technisch bijna een halve megabyte per record
	// dragen, maar TypeFrame is Ethernet: een groter record vóór elke flood,
	// gateway-kopie of NAT-route weigeren. Anders kan één slot de MTU-buffer
	// van een buurslot permanent corrupt verklaren of de LAN-ringen met
	// reuzenrecords vullen.
	maxFrameLen = netdev.MTU + netdev.EthernetMaximumSize

	// Een lokale producer mag een korte TCP-burst groter dan zijn ring maken.
	// Wacht dan op de switch in plaats van de staart stil te droppen en pas na
	// een retransmissietimer terug te zien. De timeout bewaakt isolatie.
	txBackpressure = 10 * time.Millisecond
)

// uit het net-plan in layout, als string voor de mains (layout.IP4Str: de
// string-vorm woont bij de bron van het plan).

func SlotIP(i int) string { return layout.IP4Str(layout.SlotIP4(i)) }

// GatewayIP is "mijn node" zoals elke app hem ziet (10.100.0.1): het adres waar
// de node-diensten binnen te bereiken zijn, op élke node hetzelfde. Zelfde
// waarde als SlotIP(0), maar dít is de naam waaronder een aanroeper hem bedoelt.
func GatewayIP() string { return SlotIP(0) }

// hostMAC is HOP's MAC op het interne net (slot 0 → ..:00).
var hostMAC = layout.SlotMAC(0)

// port is één switch-poort. Productie-apps delen alleen twee kleine
// descriptorpagina's; tx/rx bestaan nog voor HOP's lokale poort en hosttests.
// De switch is per richting de enige tegenhanger (SPSC): consumer op TX,
// producer op RX.
type port struct {
	tx           *ring.Ring // app → switch
	rx           *ring.Ring // switch → app
	txq          *frameq.Queue
	rxq          *frameq.Queue
	partBase     uint64
	partSize     uint64
	pendingHead  uint32
	pendingTail  uint32
	pendingCount uint32

	// Na één begrensde wacht op een volle lokale RX-ring droppen volgende
	// frames meteen tot de consumer aantoonbaar ruimte heeft gemaakt. Zo kan
	// een niet-lezende app de hele switch nooit 10ms per frame ophouden.
	rxBlocked bool

	// txWarned: de corrupt-verklaring van de TX-ring is al gemeld. Eén regel
	// per leven van de poort — de vlag zelf is permanent (ring.Corrupt), dus
	// elke ronde melden zou de console verzuipen. De melding bestaat omdat een
	// corrupte TX-ring van buiten identiek oogt aan een lege (boot 9, 17-08:
	// slot-net dood na ~30s, app gezond, en niets op de console).
	txWarned bool
}

var (
	mu       sync.Mutex
	ports    []*port // [0..MaxSlots]; 0 is HOP, 1.. zijn geïsoleerde apps
	host     *hostDevice
	rxWake   func(slot int)
	hostWake func()
	pumpHook func(status func() bool, wake func())
	pool     framePool // one dynamic payload pool for every app port
	up       bool
)

// hostDevice is HOP's poort 0. Ook de vertrouwde kant gebruikt dus exact
// dezelfde SPSC-ringnaad als een app; er is geen gateway-queue naast het LAN.
type hostDevice struct {
	txMu sync.Mutex
	tx   *ring.Ring // HOP → switch
	rx   *ring.Ring // switch → HOP
}

func (d *hostDevice) Receive(buf []byte) (int, error) {
	typ, n, ok := d.rx.ReadInto(buf)
	if !ok || typ != ring.TypeFrame {
		return 0, nil
	}
	return n, nil
}

func (d *hostDevice) Transmit(buf []byte) error {
	d.txMu.Lock()
	defer d.txMu.Unlock()
	deadline := time.Now().Add(txBackpressure)
	for {
		ok, notify := d.tx.WriteNotify(ring.TypeFrame, buf)
		if ok {
			if notify {
				// Deze producer draait al op HOP; maak de switchlus zonder polling
				// runnable. De app-kant gebruikt dezelfde semantiek via dev.Notify.
				notifySwitch()
			}
			return nil
		}
		notifySwitch()
		if time.Now().After(deadline) {
			return fmt.Errorf("hopswitch: host TX ring bleef vol")
		}
		runtime.Gosched()
	}
}

// HostDevice geeft HOP's poort 0. Up moet eerst zijn aangeroepen.
func HostDevice() (netdev.Device, error) {
	mu.Lock()
	defer mu.Unlock()
	if !up || host == nil {
		return nil, fmt.Errorf("hopswitch: host port is not up")
	}
	return host, nil
}

// SetRXWake registreert de architectuur-onafhankelijke slot-wakegrens. De
// callback wordt alleen op een succesvolle leeg→niet-leeg overgang gedaan.
func SetRXWake(f func(slot int)) {
	mu.Lock()
	rxWake = f
	mu.Unlock()
}

// SetHostWake registreert de wekbel voor HOP's eigen RX-pomp.
func SetHostWake(f func()) {
	mu.Lock()
	hostWake = f
	mu.Unlock()
}

// UsePump bedraadt de switch met de scheduler/idle-laag zonder de
// importrichting om te keren. De hook wordt op de switchgoroutine uitgevoerd
// en krijgt uitsluitend de level-triggered pending-vraag.
func UsePump(hook func(status func() bool, wake func())) {
	mu.Lock()
	pumpHook = hook
	mu.Unlock()
}

// Up start de switch-lus; idempotent. Aanroepen vóór de eerste slots.Start.
func Up() error {
	mu.Lock()
	defer mu.Unlock()
	if up {
		return nil
	}
	ports = make([]*port, layout.MaxSlots+1) // MaxSlots staat vast na board-init
	if pool.base == 0 {
		if err := pool.configureLocal(4 << 20); err != nil {
			return err
		}
	}
	host = &hostDevice{tx: ring.New(layout.NetRingDataCap), rx: ring.New(layout.NetRingDataCap)}
	ports[0] = &port{tx: host.tx, rx: host.rx}
	go loop()
	go flowExpiryLoop()
	up = true
	return nil
}

// Attach koppelt slot i aan de switch (door slots.Start, ná queue-init).
// txPA/rxPA zijn de twee fysieke descriptorpagina's van dit slot. De app ziet
// ze op vaste IPA's; framepayload wordt met gevalideerde offsets in de eigen
// partitie benoemd en is dus nooit onderdeel van deze mapping.
// No-op zolang de switch niet Up() is: ports is dan nog nil (lazy op de
// runtime-MaxSlots gedimensioneerd), en een board dat geen switch draait
// (de Pi-mains starten slots zonder hopswitch.Up) mag hier niet crashen —
// vóór de array→slice-wissel was dit een onschuldige no-op.
func Attach(i int, txPA, rxPA uintptr, partBase, partSize uint64) {
	if i < 1 || i > layout.MaxSlots {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if !up {
		return
	}
	ports[i] = &port{
		txq:      frameq.Open(txPA),
		rxq:      frameq.Open(rxPA),
		partBase: partBase,
		partSize: partSize,
	}
}

// Detach ontkoppelt slot i. Keert pas terug als de switch-lus de ringen
// gegarandeerd niet meer aanraakt — aanroepen vóór een ring-herinit.
func Detach(i int) {
	if i < 1 || i > layout.MaxSlots {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if !up {
		return
	}
	if pt := ports[i]; pt != nil {
		freePendingLocked(pt)
	}
	ports[i] = nil
}

// loop is dé switch: drain alle poorten, bezorg per frame op dst-MAC. Ringen
// worden uitsluitend onder mu beschreven (deze lus én het NAT-bezorgpad,
// deliverLocked) — daarmee is de mu-houder vanzelf de enige producer per
// RX-ring en de enige consumer per TX-ring (SPSC zonder verdere sloten).
func loop() {
	// Eén hergebruikte leesbuffer voor alle TX-ringen (de switch-lus is één
	// goroutine): geen allocatie per frame op de netwerk-hot-path. forward
	// kopieert het frame synchroon in de dst-ring(en) of via nat de uplink.
	buf := make([]byte, maxFrameLen)
	if pumpHook != nil {
		pumpHook(switchPending, notifySwitch)
	}
	// De bel is level-triggered via de idle-governor (switchPending), maar die
	// governor draait alleen als HOP níets te doen heeft. Is HOP bezig — een
	// 1MiB-write naar de NVMe, een S3-transfer — dan mist de SEV van een app
	// zijn WFE en ligt het frame tot deze failsafe. 1ms is dan de klok, zoals
	// de waker (kern/slots) die al heeft; korter zou pollen zijn, 10ms een
	// zichtbare hik in elk request. Weg zodra een app-core HOP een IPI kan
	// sturen. Eén hergebruikte timer: time.After alloceerde er één per lege
	// ronde (Go ≥1.23: Reset laat nooit een oude waarde op het kanaal).
	const failsafe = time.Millisecond
	t := time.NewTimer(failsafe)
	for {
		if switchPass(buf) {
			// Maximaal maxBurst frames zijn nu naar een RX-ring gekopieerd. Laat
			// vooral HOP's eigen RX-pomp ze consumeren vóór de volgende burst;
			// anders kan de switch op dezelfde core de volledige ring vullen en
			// de staart van een 1MiB-system-frame alsnog droppen.
			runtime.Gosched()
			continue
		}
		t.Reset(failsafe)
		select {
		case <-switchDoor:
		case <-t.C:
		}
	}
}

func switchPending() bool {
	if !mu.TryLock() {
		// Conservatief bellen. De switch kan de lock precies tussen zijn laatste
		// lege check en zijn kanaal-wacht vasthouden; false zou dan het event
		// verliezen en de failsafe van loop nodig maken. Een overbodig kanaaltoken is
		// goedkoop en de status blijft level-triggered.
		return true
	}
	defer mu.Unlock()
	for _, pt := range ports {
		if pt != nil {
			if pt.tx != nil {
				_, pending := pt.tx.HeadPending()
				if pending {
					return true
				}
			}
			if pt.txq != nil && pt.txq.SubmitPending() {
				return true
			}
			if pt.pendingHead != 0 && pt.rxq != nil && pt.rxq.SubmitPending() {
				return true
			}
		}
	}
	return false
}

var switchDoor = make(chan struct{}, 1)

func notifySwitch() {
	select {
	case switchDoor <- struct{}{}:
	default:
	}
}

// switchPass draint alle poorten één ronde onder mu. (De uplink-inbound-kant
// — DNAT en masquerade-antwoorden — loopt hier niet meer doorheen: natInbound
// bezorgt onder ditzelfde mu rechtstreeks in de slot-ring, deliverLocked; de
// oude inject-queue kostte een allocatie + kopie + wachtbeurt per frame.)
// Diepteverdediging: een panic (een bug, of frame-inhoud die tot in nat
// reikt) mag core 0 — en dus álle slots — niet vellen. De defer ontgrendelt
// mu (ook bij een panic, anders deadlockt de volgende ronde) en recovert: het
// frame wordt gedropt en de switch draait door.
// HOP is tegenwoordig gewoon poort 0; ook dat pad is dus een ringwrite onder
// dezelfde SPSC-regels en roept nooit synchroon een netstack aan met mu vast.
func switchPass(buf []byte) (worked bool) {
	worked = switchPassLocked(buf)
	return worked
}

func switchPassLocked(buf []byte) (worked bool) {
	mu.Lock()
	defer func() {
		mu.Unlock()
		if r := recover(); r != nil {
			fmt.Printf("HOPOS_SWITCH_PANIC: %v — frame gedropt, switch draait door\n", r)
		}
	}()
	for i := 0; i <= layout.MaxSlots; i++ {
		pt := ports[i]
		if pt == nil {
			continue
		}
		if pt.rxq != nil && flushPendingLocked(i, pt) {
			worked = true
		}
		for range maxBurst {
			var n int
			var ok bool
			if pt.tx != nil {
				var typ uint32
				typ, n, ok = pt.tx.ReadInto(buf)
				if ok && typ != ring.TypeFrame {
					continue
				}
			} else {
				n, ok = readAppTXLocked(pt, buf)
			}
			if !ok {
				break
			}
			forward(i, buf[:n])
			worked = true
		}
		if pt.tx != nil && !pt.txWarned && pt.tx.Corrupt() {
			pt.txWarned = true
			fmt.Printf("HOPOS_NETRING_TX_CORRUPT: slot %d: %s\n", i, pt.tx.CorruptWhy())
		}
		if pt.txq != nil && !pt.txWarned && pt.txq.CorruptWhy() != "" {
			pt.txWarned = true
			fmt.Printf("HOPOS_FRAMEQ_TX_CORRUPT: slot %d: %s\n", i, pt.txq.CorruptWhy())
		}
	}
	return worked
}

func descriptorPA(pt *port, d frameq.Desc, need uint32) (uintptr, uint32) {
	if d.Length < need {
		return 0, frameq.StatusTooSmall
	}
	if d.Offset > pt.partSize || uint64(d.Length) > pt.partSize-d.Offset {
		return 0, frameq.StatusBounds
	}
	return uintptr(pt.partBase + d.Offset), frameq.StatusOK
}

func readAppTXLocked(pt *port, dst []byte) (int, bool) {
	if pt.txq == nil {
		return 0, false
	}
	// Een kapotte descriptor is één mislukt frame, niet het einde van deze
	// switchronde. Consumeer en complete maximaal één hele queue voordat we de
	// volgende poort laten lopen; zo kan een kwaad slot geen globale spin maken.
	for range frameq.Entries {
		d, ok := pt.txq.Take()
		if !ok {
			return 0, false
		}
		status := uint32(frameq.StatusOK)
		if d.Length > maxFrameLen || d.Length < 14 {
			status = frameq.StatusTooSmall
		}
		pa, bounds := descriptorPA(pt, d, d.Length)
		if bounds != frameq.StatusOK {
			status = bounds
		}
		if status == frameq.StatusOK {
			dev.Pull(pa, uintptr(d.Length))
			dev.CopyOut(dst[:d.Length], pa)
		}
		pt.txq.Complete(d.Token, d.Length, status)
		if status == frameq.StatusOK {
			return int(d.Length), true
		}
	}
	return 0, false
}

// deliverLocked legt een (door de NAT al herschreven) inbound frame
// rechtstreeks in de RX-ring van slot i — zonder tussenstop: élke
// RX-ring-write gebeurt onder mu (de switch-lus én dit pad), dus de
// SPSC-invariant (één producer) staat al; de oude inject-queue voegde alleen
// een allocatie, een kopie en een wachtbeurt op de switch-ronde toe (~2 van
// de ~5 ongecachte passes per frame — netdoorvoer-analyse 17-07). Vol of
// niet aangesloten = drop (zoals echt Ethernet; TCP herstelt). Aanroepen met
// mu vast (vanuit natInbound).
func deliverLocked(i int, p []byte) {
	if len(p) > maxFrameLen || i < 1 || i >= len(ports) || ports[i] == nil {
		return
	}
	writeRXLocked(i, p)
}

// writeRXLocked is the only producer boundary for LAN RX. App ports first use
// an offered app-owned buffer. If none is available the frame borrows one
// chunk from the global pool; no per-app payload capacity is reserved.
func writeRXLocked(i int, p []byte) {
	if i < 0 || i >= len(ports) || ports[i] == nil {
		return
	}
	pt := ports[i]
	if pt.rxq != nil {
		// Preserve wire order: once a port has backlog, append new frames behind
		// it even if a fresh offer appeared in the meantime.
		if pt.pendingHead == 0 && deliverBytesLocked(i, pt, p) {
			return
		}
		enqueuePendingLocked(pt, p)
		notifySwitch()
		return
	}

	// HOP's own port and legacy host tests still use a local byte ring.
	deadline := time.Now().Add(txBackpressure)
	woken := false
	for {
		ok, notify := pt.rx.WriteNotify(ring.TypeFrame, p)
		if ok {
			pt.rxBlocked = false
			if notify {
				wakeRXLocked(i)
			}
			return
		}
		if pt.rxBlocked {
			return
		}
		// Vol impliceert niet dat de consumer nú runnable is. Eén extra kick
		// maakt de full→space-race level-triggered zonder 10ms lang te hameren.
		if !woken {
			wakeRXLocked(i)
			woken = true
		}
		if time.Now().After(deadline) {
			pt.rxBlocked = true
			return
		}
		runtime.Gosched()
	}
}

func takeRXBufferLocked(i int, pt *port, need uint32) (frameq.Desc, uintptr, bool) {
	for range frameq.Entries {
		d, ok := pt.rxq.Take()
		if !ok {
			return frameq.Desc{}, 0, false
		}
		pa, status := descriptorPA(pt, d, need)
		if status == frameq.StatusOK {
			return d, pa, true
		}
		pt.rxq.Complete(d.Token, 0, status)
		wakeRXLocked(i)
	}
	return frameq.Desc{}, 0, false
}

func deliverBytesLocked(i int, pt *port, p []byte) bool {
	d, pa, ok := takeRXBufferLocked(i, pt, uint32(len(p)))
	if !ok {
		return false
	}
	dev.Copy(pa, p)
	dev.Push(pa, uintptr(len(p)))
	pt.rxq.Complete(d.Token, uint32(len(p)), frameq.StatusOK)
	wakeRXLocked(i)
	return true
}

func enqueuePendingLocked(pt *port, p []byte) bool {
	if !mayBorrowLocked(pt) {
		return false
	}
	id := pool.alloc(p)
	if id == 0 {
		return false
	}
	if pt.pendingTail == 0 {
		pt.pendingHead = id
	} else {
		pool.chunks[pt.pendingTail].next = id
	}
	pt.pendingTail = id
	pt.pendingCount++
	return true
}

// mayBorrowLocked houdt een kleine bodem beschikbaar voor iedere andere
// aangesloten app, zonder ook maar één chunk vooraf aan een slot toe te wijzen.
// Met één ontvanger mag die de hele pool gebruiken; bij meerdere ontvangers
// leent iedereen uit hetzelfde vrije deel en groeit de werkelijke verdeling met
// de vraag. Zodra alle bodems in gebruik zijn is ook de rest weer vrij voor
// wie hem het eerst nodig heeft. Vol = Ethernet-drop; TCP regelt herstel.
func mayBorrowLocked(want *port) bool {
	if pool.freeCount == 0 {
		return false
	}
	var attached uint32
	for i, pt := range ports {
		if i > 0 && pt != nil && pt.rxq != nil {
			attached++
		}
	}
	if attached <= 1 {
		return true
	}
	total := uint32(len(pool.chunks) - 1)
	reserveEach := total / (2 * attached)
	if reserveEach > frameq.Entries {
		reserveEach = frameq.Entries
	}
	var reserved uint32
	for i, pt := range ports {
		if i == 0 || pt == nil || pt == want || pt.rxq == nil || pt.pendingCount >= reserveEach {
			continue
		}
		reserved += reserveEach - pt.pendingCount
	}
	return pool.freeCount > reserved
}

func flushPendingLocked(i int, pt *port) (worked bool) {
	for pt.pendingHead != 0 {
		id := pt.pendingHead
		need := uint32(pool.chunks[id].length)
		d, pa, ok := takeRXBufferLocked(i, pt, need)
		if !ok {
			return worked
		}
		next := pool.chunks[id].next
		n, valid := pool.copyTo(id, pa)
		status := uint32(frameq.StatusOK)
		if !valid {
			status = frameq.StatusBounds
			n = 0
		} else {
			dev.Push(pa, uintptr(n))
		}
		pt.rxq.Complete(d.Token, uint32(n), status)
		pool.release(id)
		pt.pendingHead = next
		pt.pendingCount--
		if next == 0 {
			pt.pendingTail = 0
		}
		wakeRXLocked(i)
		worked = true
	}
	return worked
}

func freePendingLocked(pt *port) {
	for pt.pendingHead != 0 {
		id := pt.pendingHead
		pt.pendingHead = pool.chunks[id].next
		pool.release(id)
		pt.pendingCount--
	}
	pt.pendingTail = 0
}

func wakeRXLocked(i int) {
	// Dedicated ARM-apps kunnen zelf in WFE staan; hetzelfde generieke event
	// wekt hen zonder dat de switch hun architectuur hoeft te kennen. Een app
	// die naar EL2/M-mode yieldde krijgt hieronder bovendien de doelkick.
	dev.Notify()
	if i == 0 {
		if hostWake != nil {
			hostWake()
		}
		return
	}
	if rxWake != nil {
		rxWake(i)
	}
}

// forward bezorgt één frame op grond van de dst-MAC — meer switch is er
// niet. Onbekende bestemming of volle ring = drop (zoals echt Ethernet).
// Aanroepen met mu vast (vanuit switchPass).
func forward(src int, p []byte) {
	if len(p) < 14 || len(p) > maxFrameLen {
		return
	}
	// Bron-MAC-controle: een slot mag alleen zijn ÉIGEN MAC gebruiken. De
	// switch weet uit welke ring hij dit frame las (src) en de nummering is
	// deterministisch (layout.SlotMAC), dus dit is een gratis feit — geen
	// leertabel die te vergiftigen valt.
	//
	// Zonder deze regel kan een slot zich als een ander slot voordoen op laag
	// 2: frames sturen met andermans bron-MAC, en daarmee ARP-antwoorden geven
	// namens een adres dat niet van hem is. Dat is precies de aanval die je
	// niet wilt op een node waar HOP toetsaanslagen rondstuurt — dan is
	// meeluisteren met wat de gebruiker typt een kwestie van sneller
	// antwoorden dan de buurman. src 0 is HOP zelf (de gateway) en valt buiten
	// deze grens: dat is de vertrouwde kant.
	if src >= 1 && (p[6] != 0x02 || p[7]|p[8]|p[9]|p[10] != 0 || int(p[11]) != src) {
		return
	}
	if src >= 1 && !validSourceIP(src, p) {
		return
	}
	if p[0]&1 != 0 { // broadcast/multicast (ARP): iedereen behalve de bron
		if arpReplyGateway(src, p) { // who-has de gateway? HOP antwoordt zelf
			return
		}
		for i := 1; i <= layout.MaxSlots; i++ {
			if i != src && ports[i] != nil {
				writeRXLocked(i, p)
			}
		}
		// IP-multicast (mDNS, matter, NDP) gaat óók het LAN op — de scope is
		// de hele link, niet alleen deze node. 01:00:5e (IPv4) en 33:33
		// (IPv6); broadcast en het interne ARP-verkeer blijven binnen (dat
		// is node-privé; het op het LAN zetten lekt de 10.100-namen en
		// verwart niemand ten goede).
		if (p[0] == 0x01 && p[1] == 0x00 && p[2] == 0x5e) ||
			(p[0] == 0x33 && p[1] == 0x33) {
			uplinkMulticastTx(p)
		}
		return
	}
	if p[0] != 0x02 || p[1]|p[2]|p[3]|p[4] != 0 {
		// Geen switch-MAC. IPv6-unicast van een slot naar een LAN-buur (een
		// matter-apparaat, de border router) gaat als écht L2-frame de NIC
		// op, mét de slot-MAC als bron — v6 kent geen NAT-pad en hoort dat
		// ook niet te krijgen: NDP heeft de slot al als buur geadverteerd.
		// De terugweg staat in Uplink.Receive (unicast op een slot-MAC).
		if p[12] == 0x86 && p[13] == 0xdd {
			uplinkMulticastTx(p)
		}
		return // v4-unicast kent alleen de switch-MAC's (NAT is de uitweg)
	}
	dst := int(p[5])
	if dst == 0 { // naar HOP toe (de gateway-MAC)
		if src != 0 {
			// Volgorde: eerst de interne NIC (10.100.0.1 = "mijn node" —
			// agent/leader; plus ARP-replies voor die NIC), dan het antwoord
			// van een gepubliceerde poort (SNAT de externe NIC uit), anders
			// uitgaand verkeer masqueraden.
			if gatewayClaimLocked(p) {
				return
			}
			if natFromSlot(src, p) {
				return
			}
			natOutbound(src, p)
		}
		return
	}
	if dst != src && dst <= layout.MaxSlots && ports[dst] != nil {
		writeRXLocked(dst, p)
	}
}

// validSourceIP bindt L3 aan dezelfde slotidentiteit als de reeds afgedwongen
// bron-MAC. Daardoor is het remote IP van een verbinding met 10.100.0.1 een
// betrouwbare capability voor precies dat slot; een app kan niet de volumes
// of credentials van een buur lenen door diens adres als bron te schrijven.
func validSourceIP(src int, p []byte) bool {
	want := layout.SlotIP4(src)
	if len(p) >= ethLen+20 && p[12] == 0x08 && p[13] == 0x00 {
		return uint32(p[26])<<24|uint32(p[27])<<16|uint32(p[28])<<8|uint32(p[29]) == want
	}
	if len(p) >= ethLen+28 && p[12] == 0x08 && p[13] == 0x06 {
		a := p[ethLen:]
		return [6]byte(a[8:14]) == layout.SlotMAC(src) &&
			uint32(a[14])<<24|uint32(a[15])<<16|uint32(a[16])<<8|uint32(a[17]) == want
	}
	return true
}

// arpReplyGateway beantwoordt een ARP-request voor de gateway (10.100.0.1)
// namens HOP en schrijft het antwoord in de RX-ring van de vragende slot;
// true = afgehandeld. Andere ARP's (slot ↔ slot) worden gewoon geflood, die
// beantwoordt de doel-slot zelf. Aanroepen met mu vast.
//
// ARP-payload (RFC 826, na de 14-byte Ethernet-kop): htype(2) ptype(2)
// hlen(1) plen(1) oper(2) sha(6) spa(4) tha(6) tpa(4) = 28 bytes.
func arpReplyGateway(src int, p []byte) bool {
	if src < 1 || src > layout.MaxSlots || ports[src] == nil {
		return false
	}
	if len(p) < ethLen+28 || p[12] != 0x08 || p[13] != 0x06 {
		return false // geen ARP
	}
	a := p[ethLen:]
	// Ethernet/IPv4-request (oper=1) naar het gateway-IP?
	if a[0] != 0x00 || a[1] != 0x01 || a[2] != 0x08 || a[3] != 0x00 || a[6] != 0x00 || a[7] != 0x01 {
		return false
	}
	tpa := uint32(a[24])<<24 | uint32(a[25])<<16 | uint32(a[26])<<8 | uint32(a[27])
	if tpa != layout.HostIP4() {
		return false // niet voor de gateway: laat het floodpad het doen
	}
	sha := a[8:14]  // vragende MAC
	spa := a[14:18] // vragende IP

	var r [ethLen + 28]byte
	copy(r[0:6], sha)         // dst = de vrager
	copy(r[6:12], hostMAC[:]) // src = HOP
	r[12], r[13] = 0x08, 0x06 // ARP
	b := r[ethLen:]
	b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7] = 0x00, 0x01, 0x08, 0x00, 6, 4, 0x00, 0x02 // reply
	copy(b[8:14], hostMAC[:])                                                                 // sha = HOP
	gw := layout.HostIP4()
	b[14], b[15], b[16], b[17] = byte(gw>>24), byte(gw>>16), byte(gw>>8), byte(gw) // spa = gateway
	copy(b[18:24], sha)                                                            // tha = de vrager
	copy(b[24:28], spa)                                                            // tpa = zijn IP
	writeRXLocked(src, r[:])
	return true
}
