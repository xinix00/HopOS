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
	"net"
	"net/http"

	"hop-os/metal/app/applib"
	"hop-os/metal/app/applib/appnet"
)

func main() {
	app := applib.Init()

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
	resp, err := http.Get(url)
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

	app.Logf("apploader: streaming %d bytes into my partition staging", resp.ContentLength)
	// StageImage seint HOP en parkeert de core; keert bij succes niet terug.
	if err := app.StageImage(resp.Body, resp.ContentLength); err != nil {
		app.Logf("apploader: stage: %v", err)
		app.Exit(1)
	}
}
