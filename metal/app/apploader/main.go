// apploader is de universele mini-loader die HOP in élk slot als eerste laadt
// (lokaal, uit een gecachete kopie — nooit 127× over het net). Zijn enige taak:
// op ZIJN eigen core, over ZIJN eigen netstack, de échte app-image downloaden
// zijn EIGEN partitie in, en HOP dan seinen "staged". HOP plaatst de app en
// her-dispatcht de core (slots.StartStaged) — de apploader draait dan niet meer.
//
// Waarom: zou HOP zelf alle images fetchen, dan lopen 127 gelijktijdige
// downloads door één node-netstack → 127 verbindings-buffers in de 256MB
// kern-heap → OOM (gemeten 14-07). Door het downloaden naar de app te verhuizen
// verdeelt het zich over 127 app-netstacks en raakt een te grote/kapotte image
// hooguit dat ene slot. Alleen het downloaden verhuist; het geprivilegieerde
// plaatsen (stage-2, dispatch) blijft bij HOP.
//
// Canoniek gelinkt als een gewone app-image; bouwen met dezelfde tags als de
// echte app (uefi/rpi5/…). De echte image-URL komt via env (HOP_IMAGE_URL),
// door HOP bij de start meegegeven.
package main

import (
	"io"
	"net"
	"net/http"
	"runtime/debug"
	"time"

	// TLS-wortels: tamago heeft geen OS en dus geen system-CA-store — zonder
	// deze fallback-bundel (de Mozilla-roots die Go meelevert) faalt élke
	// https-artifact-URL op certificaatvalidatie. Sinds het downloaden van
	// core 0 naar de apploader verhuisde moet de bundel dus híer zitten
	// (gemeten 20-07: GitHub-release-assets → x509-fout in QEMU).
	_ "golang.org/x/crypto/x509roots/fallback"

	"github.com/xinix00/HopOS/metal/app/applib"
	"github.com/xinix00/HopOS/metal/app/applib/appnet"
)

func main() {
	app := applib.Init()

	// Heap-plafond, VÓÓR de netstack. De runtime kent de staging bovenin deze
	// partitie niet: zijn arena groeit gewoon door tot RAMStart+RAMSize, en de
	// gestreamde image landt daar dwars doorheen (dev.Copy, buiten de heap om).
	// Op een 64MB-partitie kwam de heap nooit zo hoog; op 24MB wél — gemeten
	// 31-07: vijfmaal exit-code 2 middenin "streaming ... into staging".
	// Het plafond stuurt de GC zó dat de arena laag blijft; de helft is voor de
	// handshake-fase (TLS + gVisor), hieronder wordt het aangescherpt zodra de
	// image-maat bekend is. Een zácht plafond — Go kent geen harde arena-grens —
	// dus geen garantie maar druk; wat er tóch misgaat is sinds de printk-haak
	// (appboard.PrintkSink) tenminste zichtbaar als panic in het task-log.
	debug.SetMemoryLimit(int64(app.RAMSize) / 2)

	url := app.Env("HOP_IMAGE_URL")
	if url == "" {
		app.Logf("apploader: HOP_IMAGE_URL missing")
		app.Exit(1)
	}

	ip, err := appnet.Up(app)
	if err != nil {
		app.Logf("apploader: netstack: %v", err)
		app.Exit(1)
	}
	if d := app.Env("HOP_DNS"); d != "" {
		net.SetDefaultNS([]string{d})
	}

	app.Logf("apploader: %s up — fetching image on my own core+netstack from %s", ip, url)
	// Met een deadline, en dat is geen luxe: zonder timeout blijft een stilgevallen
	// verbinding EEUWIG hangen (Go's default client heeft er geen). Gemeten 30-07 op
	// de LicheeRV: de server zag een broken pipe, zijn FIN kwam nooit aan, en de
	// loader wachtte oneindig op bytes — HOP zag een task die "draait" met een
	// tikkende hartslag en kon dus niks herstarten. Liever luid falen: HOP's
	// restart-beleid is precies de plek waar een mislukte download hoort te landen.
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		app.Logf("apploader: GET: %v", err)
		app.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		app.Logf("apploader: HTTP %s", resp.Status)
		app.Exit(1)
	}
	if resp.ContentLength <= 0 {
		app.Logf("apploader: no Content-Length — cannot stage")
		app.Exit(1)
	}

	// Nu de maat bekend is: de heap alles geven wat de staging niet nodig heeft
	// (of juist minder, op een kleine partitie). De 2MB marge dekt wat buiten de
	// heap om leeft (stacks, runtime-metadata).
	if lim := int64(app.RAMSize) - (resp.ContentLength+7)&^7 - (2 << 20); lim < int64(app.RAMSize)/2 {
		if lim < 8<<20 {
			app.Logf("apploader: %d MB partition minus %d MB image leaves the runtime %d MB — expect OOM",
				app.RAMSize>>20, resp.ContentLength>>20, max(lim, 0)>>20)
		}
		debug.SetMemoryLimit(max(lim, 8<<20))
	}

	app.Logf("apploader: streaming %d bytes into my partition staging", resp.ContentLength)
	// Meetellen, maar zwijgen: een stukgelopen overdracht moet kunnen zeggen
	// HOEVEEL er binnen was ("unexpected EOF" ≠ "niets gekregen"), en dat staat
	// hieronder in de foutregel. Tijdens het downloaden logt hij níet — elke
	// console-regel op dit board kost meer buffering dan de NIC-ring heeft, dus
	// een voortgangsbalk zou het probleem zijn dat hij meet.
	var got progress
	body := io.TeeReader(resp.Body, &got)
	// StageImage seint HOP en parkeert de core; keert bij succes niet terug.
	if err := app.StageImage(body, resp.ContentLength); err != nil {
		app.Logf("apploader: stage na %d van %d bytes: %v", got.seen, resp.ContentLength, err)
		app.Exit(1)
	}
}

// progress telt de binnengekomen bytes: het antwoord op "hoe ver kwam hij" in
// de foutregel van een mislukte overdracht, zonder er tijdens de overdracht
// iets over te zeggen.
type progress struct{ seen int64 }

func (p *progress) Write(b []byte) (int, error) {
	p.seen += int64(len(b))
	return len(b), nil
}
