// Package applib is de app-kant van HopOS' slot-protocol: elke app-image
// linkt dit pakket en roept Init() aan als eerste regel van main. Daarmee:
//
//   - meldt de app zich READY op zijn control-page;
//   - loopt er automatisch een heartbeat (hang-detectie door HOP);
//   - wordt de kill-flag van HOP gehoorzaamd: status EXITED + PSCI CPU_OFF;
//   - is de hop-ABI beschikbaar: Logf en de fs-laag (Stat/ReadFile/
//     WriteFile/List/Remove/Fetch) over de eigen mailbox-ringen. De app ziet
//     een eigen lege root plus de volumes die HOP bij de start mountte.
package applib

import (
	"fmt"
	"io"
	"runtime"
	"runtime/goos"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xinix00/HopOS/metal/abi/hopabi"
	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/abi/ring"
	"github.com/xinix00/HopOS/metal/board/appboard"
	"github.com/xinix00/HopOS/metal/cpu/idle"
	"github.com/xinix00/HopOS/metal/cpu/memattr"
	"github.com/xinix00/HopOS/metal/cpu/memlimit"
	"github.com/xinix00/HopOS/metal/cpu/smp"
	"github.com/xinix00/HopOS/metal/dev"
)

// App is het handle dat Init teruggeeft.
type App struct {
	Slot     int    // slot-index (= core-index)
	RAMStart uint64 // eigen partitiebasis
	RAMSize  uint64 // eigen (door HOP gepatchte) RAM-declaratie

	env  map[string]string // door HOP meegegeven bij start
	mu   sync.Mutex        // outbox is SPSC: één producer tegelijk (logs + RPC)
	seq  uint32
	out  *ring.Ring // hop-ABI outbox (app → HOP)
	in   *ring.Ring // hop-ABI inbox (HOP → app)
	rbuf []byte     // hergebruikte leesbuffer (onder mu, zoals seq)

	// printk-regelbuffer (appboard.PrintkSink): runtime-output komt per byte
	// binnen en gaat per regel de log-ring op. Vast formaat, want de schrijver
	// zit mogelijk midden in een panic en mag dan niet alloceren.
	pkBuf [224]byte
	pkN   int
}

// abiVersion is de slot-ABI waartegen dít image gelinkt is (abi/place.SymABI).
// Geen variabele die iemand op runtime leest maar een stempel in het binary:
// HOP leest 'm bij plaatsing uit de symboltabel en weigert een image met een
// andere versie — zo kan een app nooit stil op het verkeerde adres zijn control
// page of ringen zoeken. Een symbool dat nergens gelezen wordt veegt de linker
// weg (gemeten: dan weigert HOP élk image), dus houdt Init het stempel met één
// load in leven — goedkoper dan het op de control page echoën, waar niemand het
// las.
var abiVersion uint64 = layout.ABIVersion

// ctrlGet/ctrlSet zijn de ABI-lees en -schrijf: met het cache-onderhoud dat de
// architectuur nodig heeft (dev.Pull/Push — no-ops waar de control page
// device-gemapt is, echt werk waar HOP en dit hart niet coherent zijn). Alle
// ABI-verkeer van de app loopt hierlangs; een rauwe deref zou op zo'n board stil
// in de eigen D-cache blijven staan en voor HOP niet bestaan.
func (a *App) ctrlGet(off uintptr) uint64 {
	p := layout.CtrlPageAt(a.RAMStart, a.RAMSize) + off
	dev.Pull(p, 8)
	return dev.Read64(p)
}

func (a *App) ctrlSet(off uintptr, v uint64) {
	p := layout.CtrlPageAt(a.RAMStart, a.RAMSize) + off
	dev.Write64(p, v)
	dev.Push(p, 8)
}

// Init leidt de eigen slot-index af uit de core-identiteit (MPIDR: slot =
// core), meldt READY en start de heartbeat- en kill-watchers. Niet uit de
// RAM-declaratie: die is canoniek (zelfde linkadres voor elk slot; de
// stage-2-vertaling legt de image in de echte partitie).
func Init() *App {
	// Eerst het geheugenplafond: de partitie is de hele wereld van deze app
	// en Go's default GC-beleid kent geen muur (cpu/memlimit — de stille
	// OOM-dood van 02-08). Vóór al het andere, zodat ook de init-allocaties
	// hieronder al onder het plafond vallen.
	memlimit.Arm()

	start, end := runtime.MemRegion()
	a := &App{
		Slot:     appboard.Current().CoreID(),
		RAMStart: uint64(start),
		RAMSize:  uint64(end - start),
	}

	a.out = ring.Open(layout.RingOutboxAt(a.RAMStart, a.RAMSize))
	a.in = ring.Open(layout.RingInboxAt(a.RAMStart, a.RAMSize))
	a.rbuf = make([]byte, layout.RingDataCap)
	a.env = a.readEnv()

	// Kreeg deze app het glas (gui/fbgrant zette FB_*), dan is dat venster DRAM
	// en hoort het als write-combine gemapt te worden. De stage-2 van de kooi
	// zegt dat al (Normal-NC), maar bij ARM wint de STRENGSTE van de twee lagen
	// en onze eigen stage-1 mapt alles buiten de partitie als Device-nGnRnE —
	// dus zonder deze regel drukt de app-kant de grant plat naar device en gaat
	// élk beeldje als miljoenen losse ongatherbare stores het fabric in.
	// OS-laag-werk: de app merkt er niets van en hoeft er niets voor te doen.
	a.mapGrantedFB()

	// Fatale runtime-exits (panic, os.Exit) onderscheppen: tamago's default
	// halt is DAIFSet+WFI — een lijk dat geen vertaalde toegang meer doet,
	// dus zelfs de stage-2-revoke niet meer voelt (verbrande core, gemeten
	// 19-07: browser-panic → dedicated core weg tot powercycle). Via de
	// goos.Exit-hook parkeert de core in plaats daarvan netjes bij HopOS —
	// kaal (Exit): na een fatalpanic draait de scheduler niet meer, dus
	// géén hooks/goroutines hier. Peers horen de dood via de switch-RST bij
	// de teardown.
	goos.Exit = func(code int32) { a.Exit(uint64(uint32(code))) }

	// Runtime-output (per byte, via het board) de log-ring op. Het enige dat
	// daar nog langskomt is een panic — en juist die MOET het task-log halen:
	// zonder deze haak is een panic een exit-code 2 zonder één regel reden
	// (gemeten 31-07: de apploader-OOM stierf vijfmaal onzichtbaar).
	appboard.PrintkSink = a.printk

	// Klok overnemen van HOP (die synct via SNTP): zonder dit begint elke
	// app-runtime op 1970. De teller is gedeeld, de offset dus ook.
	if off := a.ctrlGet(layout.CtrlWallOff); off != 0 {
		appboard.Current().SetTimerOffset(int64(off))
	}

	a.ctrlSet(layout.CtrlRAMSize, a.RAMSize)

	// SMP (fase 5): de OS-laag brengt de door HOP toegewezen extra cores
	// transparant op (goos.Task) en zet GOMAXPROCS=N. De app krijgt zo N cores
	// "as is" — parallelle goroutines op een gedeelde heap — zonder dat app-code
	// er iets van merkt of aan hoeft te doen. Configure is een no-op bij één
	// core, dus hier geen SMP-vertakking. Vóór READY, zodat wie op READY wacht
	// meteen de volledige machine ziet.
	smp.Configure(a.Slot, int(a.ctrlGet(layout.CtrlCores)), layout.CtrlPageAt(a.RAMStart, a.RAMSize))

	// Idle-tik-teller publiceren (metal/cpu/idle → CtrlIdle): het klok-signaal
	// voor de wachter op de HOP-core. OS-laag-werk — de app merkt er niets
	// van, net als bij SMP.
	idle.Publish(layout.CtrlPageAt(a.RAMStart, a.RAMSize) + layout.CtrlIdle)
	idle.PublishWakes(layout.CtrlPageAt(a.RAMStart, a.RAMSize) + layout.CtrlWakes)

	// Core-deling (fase 6): laat de governor het CtrlShared-woord volgen. Zet
	// HOP het (dit slot deelt zijn core), dan yieldt de governor coöperatief
	// via HVC i.p.v. WFE zodat de mede-bewoner draait. OS-laag-werk — de app
	// merkt er niets van, net als bij SMP en de idle-teller.
	idle.WatchShared(layout.CtrlPageAt(a.RAMStart, a.RAMSize) + layout.CtrlShared)

	// Het ABI-stempel aanraken zodat het in het image blijft staan (zie
	// abiVersion): HOP leest het bij plaatsing uit de symboltabel.
	if abiVersion == 0 {
		panic("applib: ABI stamp missing")
	}

	a.ctrlSet(layout.CtrlStatus, layout.StatusReady)

	go a.watch()
	return a
}

// Env geeft een door HOP meegegeven omgevingsvariabele (leeg = afwezig).
// De ER_PORT_*/ER_ATTR_*-conventie van HOP werkt hier ongewijzigd.
func (a *App) Env(key string) string { return a.env[key] }

// readEnv leest de env-blob die HOP op de control-page schreef.
func (a *App) readEnv() map[string]string {
	n := a.ctrlGet(layout.CtrlEnvLen)
	env := make(map[string]string)
	if n == 0 || n > layout.CtrlEnvMax {
		return env
	}
	blob := make([]byte, n)
	env0 := layout.CtrlPageAt(a.RAMStart, a.RAMSize) + layout.CtrlEnvData
	dev.Pull(env0, uintptr(n))
	dev.CopyOut(blob, env0)
	for _, line := range strings.Split(string(blob), "\n") {
		if eq := strings.IndexByte(line, '='); eq > 0 {
			env[line[:eq]] = line[eq+1:]
		}
	}
	return env
}

// mapGrantedFB zet het geglaste venster (FB_BASE/FB_STRIDE/FB_HEIGHT uit
// gui/fbgrant) op write-combine in de eigen stage-1-map — zie cpu/memattr voor
// het waarom en wat er wel en niet bewezen is. Wél melden en niet stil: een
// optimalisatie waarvan niemand weet of hij aanstaat, is precies de val waarin
// de freeze-jacht van 04-08 twee keer liep.
func (a *App) mapGrantedFB() {
	base, err1 := strconv.ParseUint(a.env["FB_BASE"], 0, 64)
	stride, err2 := strconv.Atoi(a.env["FB_STRIDE"])
	height, err3 := strconv.Atoi(a.env["FB_HEIGHT"])
	if err1 != nil || err2 != nil || err3 != nil || base == 0 || stride <= 0 || height <= 0 {
		return // geen (complete) grant: headless is de normale situatie
	}
	span := uintptr(stride) * uintptr(height)
	if err := memattr.NormalNC(uintptr(base), span); err != nil {
		a.Logf("fb: window %#x stays device-mapped: %v", base, err)
	} else {
		a.Logf("fb: window %#x..%#x mapped write-combine", base, base+uint64(span))
	}
}

// printk buffert runtime-output tot een regel en zet die op de log-ring. De
// aanroeper zit mogelijk midden in een panic, dus de regels zijn hier strenger
// dan bij Logf: geen allocatie (vaste buffer), geen blokkerende lock (TryLock —
// de mutex kan van een gestorven goroutine zijn), en bij een volle ring meteen
// droppen. Torn output bij gelijktijdige runtime-prints is geaccepteerd:
// runtime-output ís de uitzondering, en een halve panic-regel is oneindig veel
// meer dan de nul regels van hiervoor.
func (a *App) printk(c byte) {
	if c != '\n' {
		if a.pkN < len(a.pkBuf) {
			a.pkBuf[a.pkN] = c
			a.pkN++
			return
		}
		// Regel te lang: flushen wat er ligt, dan de byte alsnog bufferen.
	}
	n := a.pkN
	a.pkN = 0
	if n == 0 {
		return
	}
	if a.mu.TryLock() {
		a.out.Write(ring.TypeLog, a.pkBuf[:n])
		a.mu.Unlock()
	}
	if c != '\n' {
		a.pkBuf[0] = c
		a.pkN = 1
	}
}

// Logf stuurt een logregel naar HOP via de hop-ABI-outbox. Bij een volle ring
// wordt kort gewacht en anders gedropt (logs mogen het werk nooit blokkeren).
func (a *App) Logf(format string, args ...any) {
	msg := []byte(fmt.Sprintf(format, args...))
	a.mu.Lock()
	defer a.mu.Unlock()
	for range 100 {
		if a.out.Write(ring.TypeLog, msg) {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// rpc doet één hop-ABI-call: request de outbox op, response van de inbox.
// Eén in flight tegelijk (mutex); responses met een vreemde seq — van een
// eerdere, verlopen call — worden overgeslagen.
func (a *App) rpc(req hopabi.Req, timeout time.Duration) (hopabi.Resp, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	req.Seq = a.seq
	payload := hopabi.EncodeReq(req)
	// Spiegel van de servicer-kant (slots.go): een request die nóóit in de
	// outbox past zou de Write-lus hieronder de volle timeout laten spinnen en
	// dan misleidend "outbox blijft vol" geven. Meteen een grootte-fout.
	if !a.out.Fits(len(payload)) {
		return hopabi.Resp{}, fmt.Errorf("hop-ABI: request %d bytes past niet in de outbox", len(payload))
	}
	deadline := time.Now().Add(timeout)
	for !a.out.Write(ring.TypeRPCReq, payload) {
		if time.Now().After(deadline) {
			return hopabi.Resp{}, fmt.Errorf("hop-ABI: outbox blijft vol")
		}
		time.Sleep(time.Millisecond)
	}
	for {
		typ, n, ok := a.in.ReadInto(a.rbuf)
		if !ok {
			if time.Now().After(deadline) {
				return hopabi.Resp{}, fmt.Errorf("hop-ABI: geen antwoord op op %d", req.Op)
			}
			time.Sleep(500 * time.Microsecond)
			continue
		}
		if typ != ring.TypeRPCResp {
			continue
		}
		resp, err := hopabi.DecodeResp(a.rbuf[:n])
		if err != nil {
			return hopabi.Resp{}, err
		}
		if resp.Seq != req.Seq {
			continue
		}
		// resp.Data wijst in de hergebruikte leesbuffer: kopiëren vóór hij
		// de mutex (en dus de volgende ReadInto) overleeft.
		resp.Data = append([]byte(nil), resp.Data...)
		if resp.Status != hopabi.StatusOK {
			return resp, fmt.Errorf("hop-ABI op %d: status %d: %s", req.Op, resp.Status, resp.Data)
		}
		return resp, nil
	}
}

const rpcTimeout = 10 * time.Second

// Stat geeft de grootte van een bestand (of 0 voor een dir).
func (a *App) Stat(path string) (uint64, error) {
	resp, err := a.rpc(hopabi.Req{Op: hopabi.OpStat, Path: path}, rpcTimeout)
	return resp.Size, err
}

// ReadAt leest maximaal n bytes vanaf off (n ≤ hopabi.MaxChunk per call).
func (a *App) ReadAt(path string, off uint64, n int) ([]byte, error) {
	resp, err := a.rpc(hopabi.Req{Op: hopabi.OpRead, Path: path, Off: off, N: uint64(n)}, rpcTimeout)
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// ReadFile leest een heel bestand (gechunkt over de ring).
func (a *App) ReadFile(path string) ([]byte, error) {
	size, err := a.Stat(path)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 0, size)
	for off := uint64(0); off < size; {
		chunk, err := a.ReadAt(path, off, hopabi.MaxChunk)
		if err != nil {
			return nil, err
		}
		if len(chunk) == 0 {
			return nil, fmt.Errorf("hop-ABI: lege read op %d/%d", off, size)
		}
		buf = append(buf, chunk...)
		off += uint64(len(chunk))
	}
	return buf, nil
}

// WriteFile VERVANGT de inhoud van path door data (gechunkt; maakt bestand +
// ouder-dirs).
//
// De truncate vooraf is de replace-semantiek, en die is er niet gratis: de
// schrijf-op alleen kan een bestand niet korter maken, dus zonder dit bleef bij
// kortere nieuwe inhoud de oude STAART staan en liet een lege write het oude
// bestand volledig intact. Een lezer zag dan een bestand dat nooit geschreven is.
// Eerst op nul zetten (O_TRUNC-gedrag) maakt van een halve schrijf een KORT
// bestand in plaats van een gemengd bestand — en dat is de fout die je ziet.
func (a *App) WriteFile(path string, data []byte) error {
	if _, err := a.rpc(hopabi.Req{Op: hopabi.OpTruncate, Path: path, N: 0}, rpcTimeout); err != nil {
		return err
	}
	for off := 0; off < len(data); off += hopabi.MaxChunk {
		end := off + hopabi.MaxChunk
		if end > len(data) {
			end = len(data)
		}
		_, err := a.rpc(hopabi.Req{
			Op: hopabi.OpWrite, Path: path, Off: uint64(off), Data: data[off:end],
		}, rpcTimeout)
		if err != nil {
			return err
		}
	}
	return nil
}

// List geeft de namen in een dir ("naam/" = subdir).
func (a *App) List(path string) ([]string, error) {
	resp, err := a.rpc(hopabi.Req{Op: hopabi.OpList, Path: path}, rpcTimeout)
	if err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, nil
	}
	// HOP joint de namen met "\n" (geen trailing), dus Split geeft precies de
	// namen zonder lege staart.
	return strings.Split(string(resp.Data), "\n"), nil
}

// Remove verwijdert een bestand of lege dir.
func (a *App) Remove(path string) error {
	_, err := a.rpc(hopabi.Req{Op: hopabi.OpRemove, Path: path}, rpcTimeout)
	return err
}

// storeTimeout is de RPC-timeout van de store-ops: een pull/push duurt zo
// lang als het object groot is (HOP streamt hem van/naar de bucket), dus
// véél ruimer dan de fs-ops. Eén RPC in flight per app (mutex): een lange
// transfer blokkeert dus ook de eigen Logf/fs-ops — de app koos zelf voor
// een synchrone kopie.
const storeTimeout = 15 * time.Minute

// Pull haalt object <path> uit de eigen S3-map van deze job
// (apps/<cluster>/<job>/, HOP bewaakt de grens) en VERVANGT er het lokale
// bestand op hetzelfde pad mee (maakt bestand + ouder-dirs). Bestaat het
// object niet, dan blijft het lokale bestand onaangeraakt en komt er een
// fout terug. Geeft de objectgrootte in bytes terug.
//
// Persistentie is een daad: pull bij de start wat je vorige leven pushte —
// de eigen root is bij elke start immers leeg.
func (a *App) Pull(path string) (uint64, error) {
	resp, err := a.rpc(hopabi.Req{Op: hopabi.OpStorePull, Path: path}, storeTimeout)
	return resp.Size, err
}

// Push uploadt het lokale bestand <path> als object <path> naar de eigen
// S3-map (vervangend). De inhoud is die van het moment van de call: schrijf
// het bestand af vóór je pusht — een push terwijl je zelf nog schrijft is
// een luide fout (hash-mismatch), nooit stil een corrupt object. Geeft de
// verstuurde grootte in bytes terug.
func (a *App) Push(path string) (uint64, error) {
	resp, err := a.rpc(hopabi.Req{Op: hopabi.OpStorePush, Path: path}, storeTimeout)
	return resp.Size, err
}

// StoreList geeft de objectnamen in de eigen S3-map onder prefix (""= alles),
// relatief aan de map — een naam uit StoreList kan rechtstreeks naar Pull.
// De match is de tekstuele prefix-match van een object-store (géén dir-boom):
// "db" matcht ook "dbx.json"; sluit af met "/" voor map-gedrag.
func (a *App) StoreList(prefix string) ([]string, error) {
	resp, err := a.rpc(hopabi.Req{Op: hopabi.OpStoreList, Path: prefix}, storeTimeout)
	if err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, nil
	}
	return strings.Split(string(resp.Data), "\n"), nil
}

// StoreDrop verwijdert object <path> uit de eigen S3-map (idempotent: een
// object dat er niet is, is geen fout).
func (a *App) StoreDrop(path string) error {
	_, err := a.rpc(hopabi.Req{Op: hopabi.OpStoreDrop, Path: path}, storeTimeout)
	return err
}

// watch verstuurt heartbeats, gehoorzaamt de kill-flag en rapporteert elke
// ~2s de eigen geheugen-draw (MemStats.Sys → CtrlMemSys), zodat HOP per task
// weet wat hij gebrúíkt naast wat hij mág. ReadMemStats is een korte
// stop-the-world — op deze cadans verwaarloosbaar.
func (a *App) watch() {
	var ms runtime.MemStats
	var beat uint64
	for tick := 0; ; tick++ {
		beat++
		a.ctrlSet(layout.CtrlHeartbeat, beat)
		if a.ctrlGet(layout.CtrlKill) != 0 {
			a.Exit(0)
		}
		if tick%40 == 0 { // 40 × 50ms = 2s
			runtime.ReadMemStats(&ms)
			a.ctrlSet(layout.CtrlMemSys, ms.Sys)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Exit meldt de exitcode en geeft de eigen core via HVC aan HopOS terug.
// Keert nooit terug. Doet niets meer dan dat — geen hooks, goroutines of
// timers — en is daardoor óók veilig vanuit goos.Exit (na een fatale panic
// draait de scheduler niet meer, dus alles wat een goroutine nodig heeft zou
// daar stil blijven liggen).
//
// Bewust GEEN net-afscheid (geen exit-hook die verbindingen sluit, en de
// switch verzint ook geen RST's meer): een peer merkt de dood via zijn eigen
// heartbeat/deadline. Dat moet hij toch al kunnen — een switch of router kan
// een verbinding op elk moment stil doodmaken, en daar is geen signaal voor.
// Zie appnet/up_gvisor.go.
func (a *App) Exit(code uint64) {
	a.ctrlSet(layout.CtrlExitCode, code)
	a.ctrlSet(layout.CtrlStatus, layout.StatusExited)
	dev.MB() // status zichtbaar vóór we de core aan HopOS teruggeven
	// Coöperatief stoppen: de core aan HopOS teruggeven. Hoe dat gaat is
	// arch-eigen (park_<arch>.s) — op ARM een HVC naar de EL2-parkeerlus, op
	// RISC-V wachten tot HOP het hart reset. HopOS bezit zijn cores; ze gaan
	// nooit terug naar de firmware.
	parkExit()
	for {
	} // onbereikbaar
}

// StageImage is de kern van "de app downloadt zijn eigen image": stream r (de
// gedownloade image, imgSize bytes) de STAGING bovenin de eigen partitie in —
// precies waar HOP 'm bij het plaatsen verwacht (slots.StartStaged) — en sein
// HOP dan "staged". De core parkeert daarna; HOP her-dispatcht 'm op de echte
// app. StageImage keert dus niet terug bij succes.
//
// De hele download draait op DEZE core, DEZE netstack, in DEZE partitie: één
// node-netstack draagt nooit 127 verbindingen, en een te grote/kapotte image
// raakt hooguit dit ene slot ("crasht hooguit daar").
// minStageHeap is de werkruimte die de runtime tijdens een download minstens
// moet houden. Niet gekozen maar gemeten: een HTTPS-fetch draagt TLS-records,
// de x509-keten en de http-response, en dat is de piek van de hele apploader.
// Onder deze grens is de partitie simpelweg te klein voor dit image, en dat is
// een nette startfout — geen OOM halverwege en zeker geen stille corruptie.
const minStageHeap = 8 << 20

func (a *App) StageImage(r io.Reader, imgSize int64) error {
	if imgSize <= 0 {
		return fmt.Errorf("StageImage: onbekende image-grootte (Content-Length vereist)")
	}
	// Bovenin de eigen partitie — waar straks de stack/heap-top komt, maar die
	// bestaat nog niet: de echte app draait pas ná het plaatsen. layout.StageAddr
	// is het gedeelde contract: HOP rekent bij StartStaged met dezelfde functie
	// (dáár in PA, hier in IPA — de stage-2 vertaalt naar dezelfde fysieke plek).
	addr, staged, fits := layout.StageAddr(a.RAMStart, a.RAMSize, imgSize)
	if !fits {
		return fmt.Errorf("StageImage: image %d bytes past niet in partitie %d MB", imgSize, a.RAMSize>>20)
	}
	stageAddr := uintptr(addr)

	// DE HEAP MAG HIER NOOIT KOMEN. Vanaf nu deelt deze runtime zijn raam met de
	// image, en de enige die dat weet is deze functie: memlimit.Arm() rekende
	// zijn muur op de bovenkant van het RAM (RamStackOffset is 0x100), dus zonder
	// deze regel mag de heap dwars door de staging groeien. Dat is geen theorie —
	// het is wat er op de Pi 5 gebeurde (06-08): de loader groeide erdoorheen, Go
	// nulde de verse span, en HOP las `bad magic number '[0 0 0 0]'`. Een
	// genulde ELF-header ziet er precies zo uit als een kapotte download, en dat
	// kostte twee bordjes aan verdenkingen.
	//
	// De grens is dus de stagingbodem, niet de bovenkant van het raam. Wat er
	// daarna nog over is voor de runtime IS de vraag of dit image in deze
	// partitie past — vandaar dat te weinig ruimte hier luid faalt en niet
	// stilletjes doorgaat.
	limit, ok := memlimit.ArmBelow(stageAddr)
	if !ok || limit < minStageHeap {
		return fmt.Errorf("StageImage: image van %d MB laat de runtime %d MB over in een partitie van %d MB — "+
			"minstens %d MB nodig voor de download; verhoog memory_limit",
			imgSize>>20, limit>>20, a.RAMSize>>20, uint64(minStageHeap)>>20)
	}
	var buf [64 << 10]byte
	var got int64
	for got < imgSize {
		n, rerr := r.Read(buf[:])
		if n > 0 {
			if got+int64(n) > imgSize {
				return fmt.Errorf("StageImage: image groter dan aangekondigd")
			}
			dev.Copy(stageAddr+uintptr(got), buf[:n])
			got += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("StageImage: download: %w", rerr)
		}
	}
	if got != imgSize {
		return fmt.Errorf("StageImage: image incompleet: %d van %d bytes", got, imgSize)
	}
	// De ELF-magic terugkijken. Vier bytes, en ze beantwoorden een vraag die ons
	// twee bordjes aan verdenkingen kostte: HOP las hier `bad magic number
	// '[0 0 0 0]'` en dat ziet er identiek uit voor een kapotte download en voor
	// een genulde span die er overheen groeide. Wij weten dat we hem net
	// geschreven hebben — dus als hij nu weg is, is de download NIET de dader en
	// heeft iets in ons eigen raam eroverheen gelopen.
	//
	// De MemStats erbij maken van de volgende boot een meting in plaats van een
	// vermoeden: staat Sys tegen de stagingbodem aan, dan is het de heap.
	var magic [4]byte
	dev.CopyOut(magic[:], stageAddr)
	if magic != [4]byte{0x7F, 'E', 'L', 'F'} {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return fmt.Errorf("StageImage: ELF-magic weg ná %d bytes schrijven (las %v op %#x) — "+
			"iets in dit raam liep eroverheen; runtime Sys %d KB, heap %d KB, raam %#x..%#x",
			got, magic, stageAddr, m.Sys>>10, m.HeapSys>>10, a.RAMStart, a.RAMStart+a.RAMSize)
	}
	// Onze cacheable writes naar de staging naar RAM duwen: HOP (legacy-pad)
	// leest die regio ongecachet bij het plaatsen — zonder deze flush ziet hij
	// stale RAM. Ook het zelfplaats-stubje leest de staging ongecachet.
	dev.CleanInv(stageAddr, uintptr(staged))
	// Zelfplaatsing (zie selfplace.go): parseer en valideer de image hier, op
	// eigen core en cacheable, en genereer het plaatsings-stubje. Lukt dat
	// niet (exotische image, symbolen zoek), dan blijft CtrlPlaceEntry 0 en
	// plaatst HOP legacy vanaf de staging — met zijn eigen nette fout als de
	// image echt kapot is.
	if stub, err := a.selfPlace(stageAddr, imgSize); err == nil {
		a.ctrlSet(layout.CtrlPlaceEntry, stub)
	} else {
		a.Logf("apploader: self-place unavailable (%v) — HOP will place from staging", err)
	}
	// Seinen: eerst de maat, dan de status (HOP leest de maat pas ná StatusStaged).
	a.ctrlSet(layout.CtrlStagedSize, uint64(imgSize))
	dev.MB()
	a.ctrlSet(layout.CtrlStatus, layout.StatusStaged)
	dev.MB()
	// De core aan HopOS teruggeven (park, net als Exit) — maar met StatusStaged,
	// dus HOP plaatst de echte app en her-dispatcht deze core i.p.v. het slot
	// vrij te geven. Keert nooit terug.
	parkExit()
	for {
	} // onbereikbaar
}

// parkExit geeft de core aan HopOS terug; keert nooit terug. De vorm is
// arch-eigen: zie park_arm64.s (HVC naar de EL2-parkeerlus) en
// park_riscv64.go (wachten op de hart-reset).
