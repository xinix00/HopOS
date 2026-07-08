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
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"hop-os/metal/board"
	"hop-os/metal/dev"
	"hop-os/metal/hopabi"
	"hop-os/metal/layout"
	"hop-os/metal/ring"
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
		Slot:     board.Current().CoreID(),
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
		board.Current().SetTimerOffset(int64(off))
	}

	*a.ctrl(layout.CtrlRAMSize) = a.RAMSize
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

// watch verstuurt heartbeats en gehoorzaamt de kill-flag.
func (a *App) watch() {
	hb := a.ctrl(layout.CtrlHeartbeat)
	kill := a.ctrl(layout.CtrlKill)
	for {
		*hb++
		if *kill != 0 {
			a.Exit(0)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Exit meldt de exitcode en zet de eigen core uit. Keert nooit terug.
func (a *App) Exit(code uint64) {
	*a.ctrl(layout.CtrlExitCode) = code
	*a.ctrl(layout.CtrlStatus) = layout.StatusExited
	board.Current().CPUOff()
	for {
	} // onbereikbaar
}
