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
	"fmt"
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
	// handshake-fase (TLS + gVisor), en StageImage scherpt het aan zodra de
	// image-maat — en daarmee de stagingbodem — bekend is (memlimit.ArmBelow).
	// Een zácht plafond — Go kent geen harde arena-grens — dus geen garantie
	// maar druk; wat er tóch misgaat is sinds de printk-haak
	// (appboard.PrintkSink) tenminste zichtbaar als panic in het task-log.
	//
	// DIT GETAL BEPAALT DE BOVENGRENS van wat er in een kleine partitie past:
	// wat de handshake hier aan arena mag pakken, kan de image niet meer
	// gebruiken. Verlagen is de knop als een image net niet past — maar niet
	// blind: 8MB is de ondergrens waar de TLS-keten het nog doet (minStageHeap
	// in applib), en dit pad is de enige startroute van élke job.
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
	// Een timeout op de STILTE, niet op de duur. Zonder deadline blijft een
	// stilgevallen verbinding eeuwig hangen (30-07: de server zag een broken pipe,
	// zijn FIN kwam nooit aan, HOP zag een task die "draait"). Maar hier stond
	// http.Client.Timeout, en die kapt ook een overdracht af die gewoon vordert:
	// 07-08 haalde de LicheeRV cloudflared (30MB) niet binnen vijf minuten — 7,
	// 15, 24MB binnen, elke keer stuk op de klok. Traag is gezond; stil is stuk.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = stallTimeout // dekt de fase vóór de eerste byte
	resp, err := (&http.Client{Transport: tr}).Get(url)
	if err != nil {
		app.Logf("apploader: GET: %v", err)
		app.Exit(1)
	}
	defer resp.Body.Close()
	// En de bodyfase: elke binnengekomen byte windt de timer opnieuw op; loopt hij
	// af, dan sluit hij de body en valt de lopende Read om.
	stall := time.AfterFunc(stallTimeout, func() { resp.Body.Close() })
	got := progress{tick: func() { stall.Reset(stallTimeout) }}
	if resp.StatusCode != 200 {
		app.Logf("apploader: HTTP %s", resp.Status)
		app.Exit(1)
	}
	if resp.ContentLength <= 0 {
		app.Logf("apploader: no Content-Length — cannot stage")
		app.Exit(1)
	}

	// Het geheugenplafond zetten we hier NIET meer zelf. Dat stond hier wel, en
	// het deed twee dingen fout — samen precies de "bad magic number
	// '[0 0 0 0]'" die op 06-08 de launcher op een Pi 5 sloopte:
	//
	//  1. het gold alleen als de nieuwe limiet ónder de helft van het raam lag.
	//     Bij 30MB app-RAM en een image van 5,5MB was de limiet 22,5MB en de
	//     drempel 15MB — dus hij werd juist NIET gezet, in exact het geval
	//     waarvoor hij bedoeld was;
	//  2. hij rekende vanaf adres 0, terwijl SetMemoryLimit runtime-geheugen
	//     telt vanaf het heapfundament. Dat is ~9MB image+bss te veel, dus zelfs
	//     als hij wél had gegolden was de heap nog steeds tot in de staging
	//     gekomen.
	//
	// De enige plek die de stagingbodem écht kent is StageImage, en die zet het
	// plafond nu daar (memlimit.ArmBelow) — met de rekensom die memlimit al
	// heeft, in de eenheid die de runtime gebruikt.

	app.Logf("apploader: streaming %d bytes into my partition staging", resp.ContentLength)
	// Meetellen, maar zwijgen: een stukgelopen overdracht moet kunnen zeggen
	// HOEVEEL er binnen was ("unexpected EOF" ≠ "niets gekregen"), en dat staat
	// hieronder in de foutregel. Tijdens het downloaden logt hij níet — elke
	// console-regel op dit board kost meer buffering dan de NIC-ring heeft, dus
	// een voortgangsbalk zou het probleem zijn dat hij meet.
	body := io.TeeReader(resp.Body, &got)
	// StageImage seint HOP en parkeert de core; keert bij succes niet terug.
	if err := app.StageImage(body, resp.ContentLength); err != nil {
		// Stop() is false als de timer al gelopen heeft: dan is de body door de
		// stilte gesloten en niet door een netwerkfout, en dat hoort er anders te
		// staan dan "read on closed response body".
		if !stall.Stop() {
			err = fmt.Errorf("no data for %s", stallTimeout)
		}
		app.Logf("apploader: staging failed after %d of %d bytes: %v", got.seen, resp.ContentLength, err)
		app.Exit(1)
	}
}

// stallTimeout is hoe lang de overdracht STIL mag zijn. Niet hoe lang hij mag
// duren: een node die 30MB over een gedeeld hart binnenhaalt heeft alle tijd
// nodig die hij nodig heeft, en zolang er bytes komen is er niets mis.
const stallTimeout = 60 * time.Second

// progress telt de binnengekomen bytes: het antwoord op "hoe ver kwam hij" in
// de foutregel van een mislukte overdracht, zonder er tijdens de overdracht
// iets over te zeggen. tick is de stilte-timer die bij elke byte opnieuw
// opgewonden wordt.
type progress struct {
	seen int64
	tick func()
}

func (p *progress) Write(b []byte) (int, error) {
	p.seen += int64(len(b))
	if p.tick != nil {
		p.tick()
	}
	return len(b), nil
}
