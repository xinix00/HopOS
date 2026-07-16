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
	"strings"
	"sync"
	"time"
	"unsafe"

	"hop-os/metal/abi/hopabi"
	"hop-os/metal/abi/layout"
	"hop-os/metal/abi/ring"
	"hop-os/metal/board/appboard"
	"hop-os/metal/cpu/idle"
	"hop-os/metal/cpu/smp"
	"hop-os/metal/dev"
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
}

func (a *App) ctrl(off uintptr) *uint64 {
	return (*uint64)(unsafe.Pointer(layout.CtrlPage(a.Slot) + off))
}

// Init leidt de eigen slot-index af uit de core-identiteit (MPIDR: slot =
// core), meldt READY en start de heartbeat- en kill-watchers. Niet uit de
// RAM-declaratie: die is canoniek (zelfde linkadres voor elk slot; de
// stage-2-vertaling legt de image in de echte partitie).
func Init() *App {
	start, end := runtime.MemRegion()
	a := &App{
		Slot:     appboard.Current().CoreID(),
		RAMStart: uint64(start),
		RAMSize:  uint64(end - start),
	}

	a.out = ring.Open(layout.RingOutbox(a.Slot))
	a.in = ring.Open(layout.RingInbox(a.Slot))
	a.rbuf = make([]byte, layout.RingDataCap)
	a.env = a.readEnv()

	// Klok overnemen van HOP (die synct via SNTP): zonder dit begint elke
	// app-runtime op 1970. De teller is gedeeld, de offset dus ook.
	if off := *a.ctrl(layout.CtrlWallOff); off != 0 {
		appboard.Current().SetTimerOffset(int64(off))
	}

	*a.ctrl(layout.CtrlRAMSize) = a.RAMSize

	// SMP (fase 5): de OS-laag brengt de door HOP toegewezen extra cores
	// transparant op (goos.Task) en zet GOMAXPROCS=N. De app krijgt zo N cores
	// "as is" — parallelle goroutines op een gedeelde heap — zonder dat app-code
	// er iets van merkt of aan hoeft te doen. Configure is een no-op bij één
	// core, dus hier geen SMP-vertakking. Vóór READY, zodat wie op READY wacht
	// meteen de volledige machine ziet.
	smp.Configure(a.Slot, int(*a.ctrl(layout.CtrlCores)))

	// Idle-tik-teller publiceren (metal/cpu/idle → CtrlIdle): het klok-signaal
	// voor de wachter op de HOP-core. OS-laag-werk — de app merkt er niets
	// van, net als bij SMP.
	idle.Publish(layout.CtrlPage(a.Slot) + layout.CtrlIdle)

	*a.ctrl(layout.CtrlStatus) = layout.StatusReady

	go a.watch()
	return a
}

// Env geeft een door HOP meegegeven omgevingsvariabele (leeg = afwezig).
// De ER_PORT_*/ER_ATTR_*-conventie van HOP werkt hier ongewijzigd.
func (a *App) Env(key string) string { return a.env[key] }

// readEnv leest de env-blob die HOP op de control-page schreef.
func (a *App) readEnv() map[string]string {
	n := *a.ctrl(layout.CtrlEnvLen)
	env := make(map[string]string)
	if n == 0 || n > layout.CtrlEnvMax {
		return env
	}
	blob := make([]byte, n)
	dev.CopyOut(blob, layout.CtrlPage(a.Slot)+layout.CtrlEnvData)
	for _, line := range strings.Split(string(blob), "\n") {
		if eq := strings.IndexByte(line, '='); eq > 0 {
			env[line[:eq]] = line[eq+1:]
		}
	}
	return env
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

// WriteFile schrijft data naar path (gechunkt; maakt bestand + ouder-dirs).
func (a *App) WriteFile(path string, data []byte) error {
	if len(data) == 0 {
		_, err := a.rpc(hopabi.Req{Op: hopabi.OpWrite, Path: path}, rpcTimeout)
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

// Fetch laat HOP url downloaden naar path (binnen het zicht van deze task)
// en geeft het aantal bytes terug — de bulk gaat buiten de ring om.
func (a *App) Fetch(url, path string) (uint64, error) {
	resp, err := a.rpc(hopabi.Req{Op: hopabi.OpFetch, Path: url, Data: []byte(path)}, 60*time.Second)
	return resp.Size, err
}

// watch verstuurt heartbeats, gehoorzaamt de kill-flag en rapporteert elke
// ~2s de eigen geheugen-draw (MemStats.Sys → CtrlMemSys), zodat HOP per task
// weet wat hij gebrúíkt naast wat hij mág. ReadMemStats is een korte
// stop-the-world — op deze cadans verwaarloosbaar.
func (a *App) watch() {
	hb := a.ctrl(layout.CtrlHeartbeat)
	kill := a.ctrl(layout.CtrlKill)
	mem := a.ctrl(layout.CtrlMemSys)
	var ms runtime.MemStats
	for tick := 0; ; tick++ {
		*hb++
		if *kill != 0 {
			a.Exit(0)
		}
		if tick%40 == 0 { // 40 × 50ms = 2s
			runtime.ReadMemStats(&ms)
			*mem = ms.Sys
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Exit meldt de exitcode en zet de eigen core uit. Keert nooit terug.
func (a *App) Exit(code uint64) {
	*a.ctrl(layout.CtrlExitCode) = code
	*a.ctrl(layout.CtrlStatus) = layout.StatusExited
	dev.MB() // status zichtbaar vóór we de core aan HopOS teruggeven
	// Coöperatief stoppen via HVC → HopOS parkeert de core op EL2 (geen PSCI
	// CPU_OFF: dat geeft de core aan de firmware terug en op de Pi 5-stock
	// komt hij dan nooit terug). HopOS bezit zijn cores.
	hvcExit()
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
		*a.ctrl(layout.CtrlPlaceEntry) = stub
	} else {
		a.Logf("apploader: self-place unavailable (%v) — HOP will place from staging", err)
	}
	// Seinen: eerst de maat, dan de status (HOP leest de maat pas ná StatusStaged).
	*a.ctrl(layout.CtrlStagedSize) = uint64(imgSize)
	dev.MB()
	*a.ctrl(layout.CtrlStatus) = layout.StatusStaged
	dev.MB()
	// De core aan HopOS teruggeven (park, net als Exit) — maar met StatusStaged,
	// dus HOP plaatst de echte app en her-dispatcht deze core i.p.v. het slot
	// vrij te geven. Keert nooit terug.
	hvcExit()
	for {
	} // onbereikbaar
}

// hvcExit trapt naar HopOS' EL2-parkeerpad (zie park_arm64.s).
func hvcExit()
