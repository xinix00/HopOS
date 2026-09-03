// netmeter is de meetbank voor de netstack van de kern (QEMU virt). Hij was
// twee keer een A/B-bank — gVisor vs lneto (09-08), lneto vs leannet (12-08) —
// en dat is ook zijn rol bij een volgende wissel: splits stack.go achter een
// build-tag en laat de nieuwe kant het verdienen. De cijfers van de vorige
// kanten staan in git history en in TODO.md. Wat blijft is de bank zelf:
// RX/TX-plafonds, allocaties per fase, en de storm-workloads — ook bruikbaar
// op ijzer (de LicheeRV-RX-jacht).
//
// Wat QEMU hier wél zuiver meet: het pakket-pad (CPU-plafond, allocaties,
// GC-druk) en de correctheid (SHA256 van elke transfer). Wat QEMU NIET kan
// meten: WAN-venstergedrag — slirp termineert TCP op de host, dus de
// guest-RTT is altijd microseconden. Die kanttekening hoort bij elk getal
// uit deze bank. Op ijzer (de LicheeRV-RX-jacht) vervalt die kanttekening:
// daar is de RTT echt en meet pull-local het hele pad NIC→DMA→stack.
//
// Bouwen (handmatig, buiten de gate):
//
//	QEMU virt:  -tags linkcpuinit  (GOARCH=arm64)
//	LicheeRV:   PAYLOAD=netmeter image/licheerv-agent.sh — de bank wordt dan
//	            zélf de monitor-payload in de FIP: de hele node is het
//	            meetinstrument, er draait geen agent ertussen.
//
// De host-helft van de bank (/small, /blob64 op :8099) is hostsrv.py in deze
// map — die moet draaien op de machine waar hostBase heen wijst.
//
// Fasen (markers op de console, Engels zoals alle console-output):
//
//	pull-local   GET <host>/blob64 — het RX-plafond van het hele pad
//	storm-keep   300 GETs /small over één verbinding — request-pad/allocaties
//	storm-conn   100 GETs /small, verbinding per request — SYN/accept-churn
//	pull-github  echte HTTPS-download (TLS + redirect naar CDN) — correctheid
//
// De :80-server staat er vanaf het begin (serveUp, vóór de fasen), niet als
// laatste fase: op een board zonder console is hij de enige weg naar de
// cijfers. /report = alle meetregels (live groeiend), /diag = NIC-ringen +
// runtime nú, /blob = de TX-kant om van buitenaf te klokken.
package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	// TLS-wortels: zonde deze bundel faalt elke https-fetch (geen OS, geen
	// system-CA-store) — zelfde regel als in cmd/hopos.
	_ "golang.org/x/crypto/x509roots/fallback"

	"github.com/xinix00/HopOS/metal/v2/net/netdev"

	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/cpu/memlimit"
)

const githubURL = "https://github.com/xinix00/hop-os-surf/releases/download/rolling-release/display.elf"

// De board-kant (registratie-import, RAM-declaratie, serveSize) staat in
// board_qemu.go / board_licheerv.go — zelfde snit als cmd/hopos.

// boardWarn laat een board bij het opkomen over zichzelf vertellen wat de
// code niet kan weten (LicheeRV: jitter-DRBG, CLINT-uitslag, temperatuurlog).
// QEMU heeft niets te melden — de default is dus leeg.
var boardWarn = func() {}

// hostBase is waar de host-helft van de bank draait (hostsrv.py, :8099).
// Default: de gateway uit het IP-plan — op QEMU is dat 10.0.2.2 (slirp = de
// Mac), op het Mac-sharing-net 192.168.99.1 (óók de Mac). Alleen op een LAN
// waar de gateway een router is wijst dat mis; dan bakt de build het adres
// in: HOSTSRV=http://192.168.1.208:8099 (→ -X main.hostOverride).
var (
	hostBase     string
	hostOverride string
)

// --- rapportage over het net ----------------------------------------------
//
// Een headless board heeft geen console: de LicheeRV en de Radxa hebben geen
// framebuffer, en een UART-dongle hangt er niet altijd aan (10-08 gemeten:
// hij hing er niet, en dán is elke fmt.Println een meting die in het niets
// verdwijnt). Daarom gaat élke meetregel óók in deze buffer, en serveert de
// bank hem op :80/report. De HTTP-server start VÓÓR de fasen, zodat je live
// kunt meekijken terwijl er gemeten wordt — bij een traag board is dat het
// verschil tussen weten en zes minuten wachten.
//
// De ringbuffer is klein (de bank print tientallen regels, geen duizenden) en
// bewust simpel: dit is een meetinstrument, geen logging-framework.
// Twee lijsten, en dat is de les van de eerste run op ijzer: de meetregels
// stonden in dezelfde ringbuffer als de netstack-meldingen, en een stack die
// honderden keren "reject segment" roept duwt precies de regels weg waar je
// het voor deed. Meetregels (NETMETER…) zijn er tientallen en worden nooit
// weggegooid; de ruis leeft in een ring.
var (
	logMu   sync.Mutex
	results []string // meetregels — compleet, in volgorde
	noise   []string // al het andere — ring, laatste 128
)

func logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	fmt.Println(line) // de console, áls er een dongle hangt
	logMu.Lock()
	if strings.HasPrefix(line, "NETMETER") {
		results = append(results, line)
	} else {
		noise = append(noise, line)
		if len(noise) > 128 {
			noise = noise[len(noise)-128:]
		}
	}
	logMu.Unlock()
}

func report() string {
	logMu.Lock()
	defer logMu.Unlock()
	out := strings.Join(results, "\n")
	if len(noise) > 0 {
		out += "\n\n--- stack messages (last " + fmt.Sprint(len(noise)) + ") ---\n" + strings.Join(noise, "\n")
	}
	return out + "\n"
}

// diagnoser is wat een NIC-driver optioneel biedt: het rauwe zicht op zijn
// ringen en DMA-status (dwmac/gem/genet hebben het alle drie). Bij een
// RX-jacht is dát de bron van waarheid — hoeveel frames de driver zag, hoeveel
// hij afkeurde, en waar de hardware-pointer staat t.o.v. de onze.
type diagnoser interface{ Diag() string }

// Het RX-tijdbudget, per frame opgeteld in de ontvangstlus. Zonder deze
// verdeling is "RX haalt de link niet" een observatie zonder aangrijpingspunt:
// het ophalen (driver) en het verwerken (stack) vragen tegengestelde
// maatregelen, en de idle-teller zegt of de lus wacht of achterloopt.
var (
	rxDriverNs, rxStackNs, rxBytes, rxCount, rxIdle atomic.Int64

	// De poll-slaap van de RX-lus, runtime verstelbaar via /set?rxsleep=N.
	// Een boot-cyclus op dit board kost een kaart uit een bordje halen en een
	// dd, dus is elke parameter die je wilt variëren beter een knop dan een
	// constante: 0µs, 50µs en 300µs naast elkaar in één boot zegt meer dan drie
	// builds, en de vergelijking is zuiver omdat niets anders verschilt.
	rxSleepUs atomic.Int64
)

func resetProfile() {
	rxDriverNs.Store(0)
	rxStackNs.Store(0)
	rxBytes.Store(0)
	rxCount.Store(0)
	rxIdle.Store(0)
}

// rxProfile is het tijdbudget als leesbare regel: ns per frame per laag, plus
// wat dat betekent tegen de ~120µs die een 1500-byte frame op 100Mbit kost.
func rxProfile() string {
	frames, drv, stk := rxCount.Load(), rxDriverNs.Load(), rxStackNs.Load()
	if frames == 0 {
		return "rx-profile: no frames yet"
	}
	perFrame := float64(drv+stk) / float64(frames)
	return fmt.Sprintf("rx-profile: frames=%d bytes=%d idle-polls=%d driver=%.1fus/frame stack=%.1fus/frame total=%.1fus/frame (100Mbit budget ~120us at 1500B)",
		frames, rxBytes.Load(), rxIdle.Load(),
		float64(drv)/float64(frames)/1000, float64(stk)/float64(frames)/1000, perFrame/1000)
}

func main() {
	memlimit.Arm()
	logf("NETMETER %s %s %s/%s", stackName, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	boardWarn()

	nic, hw, err := board.Current().ProbeNIC()

	if err != nil || nic == nil {
		logf("NETMETER_FAIL nic: %v", err)
		park()
	}
	nc := board.Current().Net()

	// De stack (stack.go): die hangt ook net.SocketFunc; wat terugkomt is de
	// RX-voeding.
	recv, err := benchStackUp(nic, nc, hw.String())
	if err != nil {
		logf("NETMETER_FAIL netstack init: %v", err)
		park()
	}

	net.SetDefaultNS([]string{nc.DNS})

	rxSleepUs.Store(300) // de hopnet-default; /set?rxsleep=N verstelt hem live

	// RX-lus in de hopnet-vorm: pollen met microslaap. De klokjes eromheen zijn
	// de kern van de RX-jacht: op 100Mbit komt er elke ~120µs een frame van
	// 1500 bytes binnen, dus als de lus er langer over doet loopt hij structureel
	// achter en helpt een diepere ring niets. Twee tellers scheiden de twee
	// verdachten: het ophalen (driver: cache-onderhoud + kopie uit DMA-geheugen)
	// en het verwerken (stack: checksums, sequence-ruimte, ringbuffer).
	go func() {
		buf := make([]byte, netdev.MTU+netdev.EthernetMaximumSize)
		for {
			t0 := time.Now()
			n, err := nic.Receive(buf)
			t1 := time.Now()
			if n == 0 || err != nil {
				rxIdle.Add(1)
				if us := rxSleepUs.Load(); us > 0 {
					time.Sleep(time.Duration(us) * time.Microsecond)
				} else {
					runtime.Gosched() // 0 = niet slapen, wel afgeven
				}
				continue
			}
			recv(buf[:n]) // drops zijn tellers op de stack, geen fout per frame
			rxDriverNs.Add(t1.Sub(t0).Nanoseconds())
			rxStackNs.Add(time.Since(t1).Nanoseconds())
			rxBytes.Add(int64(n))
			rxCount.Add(1)
		}
	}()
	hostBase = "http://" + nc.GW + ":8099"
	if hostOverride != "" {
		hostBase = hostOverride
	}
	logf("NETMETER net up: %s (gw %s, host %s)", nc.IP, nc.GW, hostBase)

	// De server eerst, dán meten: op een headless board is dit de enige weg
	// naar de cijfers, en hij moet dus al openstaan terwijl de fasen lopen.
	// http://<node>/report = alle meetregels, /diag = driver + runtime nú.
	serveUp(nic)

	// Wandtijd uit de Date-header van de host: zonder klok faalt elke
	// TLS-validatie (epoch 0 < notBefore). Seconde-grof is zat voor x509.
	if resp, err := http.Get(hostBase + "/small"); err == nil {
		if t, perr := http.ParseTime(resp.Header.Get("Date")); perr == nil {
			board.Current().SetWallTime(t.UnixNano())
			logf("NETMETER clock set: %s", t.Format(time.RFC3339))
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	} else {
		logf("NETMETER clock: no Date from %s (%v) — is hostsrv.py running? TLS phases will fail", hostBase, err)
	}

	// Vanaf hier draagt elke fase zijn eigen NIC-stand en tijdbudget mee: een
	// MB/s-getal zonder de drops en de µs/frame ernaast is niet te ontleden.
	phaseNIC = func() string {
		s := rxProfile()
		if d, ok := nic.(diagnoser); ok {
			s += " | " + d.Diag()
		}
		return s
	}

	// clock-bench: de kostprijs van de klok zélf. time.Now() ís nanotime ís
	// rdtime (riscv64), en Go klokt élke scheduling-beslissing — een dure
	// time-CSR vertraagt dus alles. De verdenking (18-08, LicheeRV): de
	// C906L-profielen meten driver=788µs/frame waar 2 profiler-klok-reads +
	// 1,4× het big-werk exact op uitkomen als één rdtime ~390µs kost — het
	// zieke c900-CLINT-blok (mtime-MMIO was al een bus-fout) zou dan ook de
	// time-CSR traag voeden. Zelf-meting is hier geldig: rdtime is traag,
	// niet fout, dus de fase-duur klopt op de wandklok. Verwacht: big ~2ms,
	// little ~8s als de hypothese klopt.
	phase("clock-bench", func() (int64, string, error) {
		const n = 20_000
		for i := 0; i < n; i++ {
			_ = time.Now()
		}
		return n, "clock-calls", nil
	})
	phase("pull-local", func() (int64, string, error) { return pull(hostBase+"/blob64", 180*time.Second) })
	phase("storm-keep", func() (int64, string, error) { return storm(300, true) })
	phase("storm-conn", func() (int64, string, error) { return storm(400, false) })
	phase("pull-github", func() (int64, string, error) { return pull(githubURL, 300*time.Second) })
	logf("NETMETER_DONE all phases — /blob stays up for the TX side")
	park()
}

// park houdt de bank in leven na een fatale fout, zodat de console leesbaar
// blijft (zelfde reden als cmd/hopos: geen shell om op terug te vallen).
func park() {
	for {
		time.Sleep(time.Hour)
	}
}

// snap is de memstats-foto vóór een fase; het verschil erna is de prijs van
// die fase in allocaties/GC — dát is het "lean"-getal naast de MB/s.
type snap struct {
	alloc, mallocs, pause uint64
	gc                    uint32
}

func take() snap {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return snap{alloc: ms.TotalAlloc, mallocs: ms.Mallocs, pause: ms.PauseTotalNs, gc: ms.NumGC}
}

// phaseNIC levert de NIC-stand + het RX-tijdbudget dat achter elke fase-regel
// komt. Een func-variabele omdat main de nic pas kent na ProbeNIC; leeg zolang
// die er niet is (dan is er ook geen fase gelopen).
var phaseNIC = func() string { return "" }

func phase(name string, f func() (int64, string, error)) {
	// De startregel is er voor /report: zonder hem zie je een trage fase pas
	// als hij klaar is, en dat is precies de fase waar je live bij wil kijken.
	logf("NETMETER_BEGIN phase=%s", name)
	before := take()
	t0 := time.Now()
	n, sha, err := f()
	dur := time.Since(t0)
	after := take()
	if err != nil {
		// Mét bytes en tempo: een fase die op zijn timeout strandt heeft nog
		// steeds gemeten hoe snel het ging (12 MiB in 180s is een getal,
		// "mislukt" is er geen).
		mb := float64(n) / (1 << 20)
		logf("NETMETER_FAIL phase=%s bytes=%d after=%v MBps=%.3f err=%v",
			name, n, dur, mb/dur.Seconds(), err)
		return
	}
	mb := float64(n) / (1 << 20)
	logf("NETMETER phase=%s bytes=%d ms=%d MBps=%.2f sha=%s allocMB=%.1f mallocs=%d gc=%d pauseMs=%.1f goroutines=%d",
		name, n, dur.Milliseconds(), mb/dur.Seconds(), sha,
		float64(after.alloc-before.alloc)/(1<<20), after.mallocs-before.mallocs,
		after.gc-before.gc, float64(after.pause-before.pause)/1e6, runtime.NumGoroutine())
	if s := phaseNIC(); s != "" {
		logf("NETMETER phase=%s %s", name, s)
	}
}

// pull haalt url binnen en hasht onderweg: het MB/s-getal en het bewijs van
// byte-correctheid komen uit dezelfde lus.
func pull(url string, timeout time.Duration) (int64, string, error) {
	cl := &http.Client{Timeout: timeout}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, "", fmt.Errorf("status %s", resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	if err != nil {
		return n, "", err
	}
	return n, fmt.Sprintf("%x", h.Sum(nil))[:16], nil
}

// storm doet n kleine GETs; keepalive=false forceert een verse verbinding per
// request (SYN/accept/FIN-churn — poort-recycling is hier deel van de meting).
// Faalt een request, dan is de vervolgvraag nét zo belangrijk als het
// faalpunt: herstelt de stack als de churn stopt? De probe wacht 30s en doet
// er dan nog vijf — "failed at N, recovered" vs "wedged" scheelt een categorie.
func storm(n int, keepalive bool) (int64, string, error) {
	tr := &http.Transport{DisableKeepAlives: !keepalive}
	cl := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	var total int64
	one := func(i int) error {
		resp, err := cl.Get(hostBase + "/small")
		if err != nil {
			return fmt.Errorf("request %d: %w", i, err)
		}
		m, err := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("request %d body: %w", i, err)
		}
		total += m
		return nil
	}
	for i := 0; i < n; i++ {
		if err := one(i); err != nil {
			logf("NETMETER storm stalled: %v — probing recovery in 30s", err)
			time.Sleep(30 * time.Second)
			for j := 0; j < 5; j++ {
				if perr := one(n + j); perr != nil {
					return total, "", fmt.Errorf("failed at %d, NOT recovered: %w", i, perr)
				}
			}
			return total, fmt.Sprintf("failed at %d, recovered after 30s", i), nil
		}
	}
	return total, fmt.Sprintf("%d reqs", n), nil
}

// serveUp brengt de :80-kant op en keert meteen terug: de bank meet daarna
// verder terwijl de server openstaat. Drie luiken:
//
//	/blob    deterministische bytes — de TX-kant, van buitenaf geklokt
//	         (curl -o /dev/null http://<node>/blob)
//	/report  alle meetregels tot nu toe — de console voor een board zonder
//	         console; tijdens een trage fase zie je hem hier groeien
//	/diag    het rauwe nú: NIC-ringen/DMA-status (als de driver Diag heeft),
//	         plus runtime-cijfers. Bij een RX-jacht is dit het instrument:
//	         rx=frames/errors en de afstand tussen onze ring-index en de
//	         hardware-pointer vertellen of er gedropt wordt.
func serveUp(nic netdev.Device) {
	buf := make([]byte, serveSize)
	x := uint64(0x48504f53) // "HPOS" — vaste seed: elke build serveert dezelfde bytes
	for i := range buf {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		buf[i] = byte(x)
	}
	sum := sha256.Sum256(buf)
	logf("NETMETER serve sha=%x bytes=%d", sum[:8], len(buf))

	http.HandleFunc("/blob", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(buf)))
		w.Write(buf)
	})
	http.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, report())
	})
	http.HandleFunc("/diag", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		if d, ok := nic.(diagnoser); ok {
			io.WriteString(w, "nic: "+d.Diag()+"\n")
		} else {
			io.WriteString(w, "nic: driver has no Diag()\n")
		}
		io.WriteString(w, rxProfile()+"\n")
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		fmt.Fprintf(w, "runtime: goroutines=%d heap=%.1fMB sys=%.1fMB mallocs=%d gc=%d pauseMs=%.1f\n",
			runtime.NumGoroutine(), float64(ms.HeapAlloc)/(1<<20), float64(ms.Sys)/(1<<20),
			ms.Mallocs, ms.NumGC, float64(ms.PauseTotalNs)/1e6)
		fmt.Fprintf(w, "board: die temp %d mC\n", board.TempMilliC())
	})

	// De experimenteerknoppen. Hiermee is een variant meten geen boot-cyclus
	// meer maar een curl: zet een parameter, nul de tellers, trek een blob.
	http.HandleFunc("/set", func(w http.ResponseWriter, r *http.Request) {
		if v := r.URL.Query().Get("rxsleep"); v != "" {
			us, err := strconv.Atoi(v)
			if err != nil || us < 0 || us > 100000 {
				http.Error(w, "rxsleep must be 0..100000 (microseconds)", 400)
				return
			}
			rxSleepUs.Store(int64(us))
			logf("NETMETER set rxsleep=%dus", us)
		}
		fmt.Fprintf(w, "rxsleep=%dus\n", rxSleepUs.Load())
	})
	http.HandleFunc("/reset", func(w http.ResponseWriter, r *http.Request) {
		resetProfile()
		io.WriteString(w, "profile counters cleared\n")
	})
	// /pull herhaalt de pull-local-fase op verzoek, zodat een variant gemeten
	// wordt onder exact dezelfde omstandigheden als de vorige.
	http.HandleFunc("/pull", func(w http.ResponseWriter, r *http.Request) {
		resetProfile()
		t0 := time.Now()
		n, sha, err := pull(hostBase+"/blob64", 180*time.Second)
		dur := time.Since(t0)
		if err != nil {
			fmt.Fprintf(w, "FAIL bytes=%d after=%v err=%v\n", n, dur, err)
			return
		}
		fmt.Fprintf(w, "bytes=%d ms=%d MBps=%.2f sha=%s rxsleep=%dus\n%s\n",
			n, dur.Milliseconds(), float64(n)/(1<<20)/dur.Seconds(), sha,
			rxSleepUs.Load(), phaseNIC())
	})

	go func() {
		if err := http.ListenAndServe(":80", nil); err != nil {
			logf("NETMETER_FAIL serve: %v", err)
		}
	}()
	logf("NETMETER_SERVE_READY :80 (/blob /report /diag /pull /set /reset)")
}
