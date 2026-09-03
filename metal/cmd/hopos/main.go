// HopOS met de echte HOP-agent aan boord (QEMU virt, fase 1): core 0 boot,
// brengt het netwerk op en start hop's agent + leader (pkg/agentboot) met de
// slot-manager als runner-backend. Jobs met driver "hop" komen binnen via de
// leader-API (:9080), de agent (:8080) downloadt de app-image en start hem
// op een vrije core — dezelfde HOP-bytes als op Linux/macOS, zonder Linux.
//
// Steiger (fase 1): standalone-cluster (deze node is z'n eigen leader);
// gaat eruit zodra hoplockserver-over-netwerk (fase 2) er is. App-images
// zijn canoniek gelinkt (slot-1-bereik): één artifact draait op elk slot,
// de stage-2-map is de relocatie.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	// TLS-wortels: tamago heeft geen OS en dus geen system-CA-store — zonder
	// deze fallback-bundel (de Mozilla-roots die Go zelf meelevert) faalt
	// élke https-fetch op certificaatvalidatie. Nodig voor het S3-artifact-
	// pad (P2b, gemeten 2026-07-11: lege x509-pool op de node).
	_ "golang.org/x/crypto/x509roots/fallback"

	"github.com/xinix00/hop/pkg/agentboot"
	"github.com/xinix00/hop/pkg/config"
	"github.com/xinix00/hop/pkg/hopos"

	"github.com/xinix00/HopOS/metal/abi/layout"
	"github.com/xinix00/HopOS/metal/board"
	"github.com/xinix00/HopOS/metal/cpu/idle"
	"github.com/xinix00/HopOS/metal/cpu/memlimit"
	"github.com/xinix00/HopOS/metal/cpu/smp"
	"github.com/xinix00/HopOS/metal/driver/fb"
	"github.com/xinix00/HopOS/metal/driver/nvme"
	"github.com/xinix00/HopOS/metal/kern/conport"
	"github.com/xinix00/HopOS/metal/kern/hopfs"
	"github.com/xinix00/HopOS/metal/kern/kernflip"
	"github.com/xinix00/HopOS/metal/kern/slotmgr"
	"github.com/xinix00/HopOS/metal/kern/slots"
	"github.com/xinix00/HopOS/metal/net/hopnet"
	"github.com/xinix00/HopOS/metal/net/hopswitch"
)

// park houdt de node in leven zónder verder werk, en keert nooit terug: HopOS
// heeft geen shell om op terug te vallen, dus een node die niet verder kán
// blijft liever bestaan (een watchdog-reboot of een latere link herstelt) dan
// verdwijnen. De reden gaat mee en wordt hier gelogd — een stille park is niet
// te diagnosticeren op een headless node.
func park(reden string) {
	fmt.Println(reden)
	for {
		time.Sleep(time.Hour)
	}
}

func fail(what string, err error) {
	park(fmt.Sprintf("FAIL %s: %v\nHOPOS_AGENT_FAIL", what, err))
}

// hopBudget/hopUsage zijn HOP's eigen geheugengetallen: wat het board hem gaf
// (de RAM-declaratie van HopSize op HopBase) en wat de
// Go-runtime daar werkelijk van vasthoudt. Ze staan hier los omdat élke MB die
// HOP niet nodig heeft naar de app-pool hoort — met deze twee regels is het
// krimpen van HopBase een meting in plaats van een gok. Zelfde bron als
// screenStatus.
func hopBudget() string {
	start, end := runtime.MemRegion()
	return fmt.Sprintf("%d MB (%#x..%#x)", (end-start)>>20, start, end)
}

func hopUsage() string {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return fmt.Sprintf("Go runtime holds %d MB, heap %d MB in use", ms.Sys>>20, ms.HeapAlloc>>20)
}

// screenStatus ververst de meetregels rechts naast de bunny (fb.HeaderStatus,
// de bovenste drie header-regels): kern-mem als percentage van de eigen
// RAM-declaratie, datum en tijd mét seconden — elke seconde, dus een bevroren
// klok verraadt een hangende kern meteen. ReadMemStats is een korte
// stop-the-world; op 1Hz verwaarloosbaar (zelfde afweging als applib's watch).
func screenStatus() {
	start, end := runtime.MemRegion()
	total := uint64(end - start)
	var ms runtime.MemStats
	for {
		runtime.ReadMemStats(&ms)
		fb.HeaderStatus(0, fmt.Sprintf("mem %d%% (%d/%dMB)",
			ms.Sys*100/total, ms.Sys>>20, total>>20))
		now := time.Now()
		fb.HeaderStatus(1, now.Format("02-01-2006"))
		fb.HeaderStatus(2, now.Format("15:04:05"))
		time.Sleep(time.Second)
	}
}

// boardExtra: optioneel board-specifiek nawerk (gezet door board_*.go in
// zijn init) — de Pi's starten er het klokbeleid mee.
var boardExtra func()

// bootParamAll leest de platform-config — HopOS leest die, want HOP-userspace
// kan er niet bij: op de Pi's cmdline.txt (via de DTB), op UEFI-boards
// hopos.cfg op de stick (door de stub via de firmware-FAT gelezen). Eén hook,
// per board gezet (board_*.go); default leeg (QEMU-embed heeft geen
// platform-config). Geeft ÁLLE waarden van een sleutel terug: enkelvoudige
// config (hopos.cores, hopos.node, hopos.s3.*) heeft er één, een herhaalde
// sleutel (hopos.init[]={...} per regel) meerdere. Configureren = het
// tekstbestandje bewerken, geen rebuild.
var bootParamAll = func(key string) []string { return nil }

// boardWarn is wat dít board over zichzelf moet bekennen vóór het werk begint —
// gezet door de board-kant van deze main (board_<x>.go), nil als er niets te
// melden is. Bewust een haak en geen board.Board-methode: het is een eigenschap
// van de PORT, niet van het silicium, en hij verdwijnt zodra de reden weg is
// (licheerv: geen hardware-TRNG, dus voorspelbare crypto).
var boardWarn func()

// bootParam is de enkele-waarde-variant: de eerste hit van bootParamAll ("" =
// niet gezet → de default in main). De meeste sleutels zijn enkelvoudig.
func bootParam(key string) string {
	if v := bootParamAll(key); len(v) > 0 {
		return v[0]
	}
	return ""
}

// nodeSerial: board-terugval voor de node-identiteit als hopos.node= niet
// gezet is (Pi: "hopos-<serial>"). "" = geen terugval → de main-default.
var nodeSerial = func() string { return "" }

// bunny: Dereks origineel (2026-07-11) — oren netjes boven het snuitje.
// Bewust geen architectuur in de tagline: ARM64 is het heden, maar AMD64-
// boardjes liggen al klaar (Derek).
var bunny = []string{
	`   (\(\`,
	`   ( -.-)     HopOS`,
	`   o_(")(")   --------------`,
	`              the Go-only OS`,
	``, // witregel: scheidt de vaste header zichtbaar van de scrollende log
}

// smpSink absorbeert het warm-up-werk zodat de compiler de lus niet
// wegoptimaliseert (de write naar een package-var blijft staan).
var smpSink uint64

// nodeSMPWarmup forceert bij boot één niet-yieldende goroutine per core
// tegelijk, zodat de extra node-Ms (en dus de cores, via nodeTask → PSCI) NU
// deterministisch opkomen: een kapotte bring-up valt bij bóót — watchdog en
// kabel zichtbaar — en niet pas onder de eerste productie-last. Geen
// benchmark meer (die zei op een 20ms-burst weinig; de ramp is de meting).
//
// Elk warm-up-hart schrijft zijn resultaat op zijn EIGEN index en de som volgt ná
// Wait. Rechtstreeks in smpSink schrijven deed precies wat deze functie wil
// uitlokken — alle cores tegelijk op één woord — en dat is een echte data race
// waar de race-detector op de host over valt; een absorberende sink hoeft geen
// gedeeld woord te zijn.
func nodeSMPWarmup(cores int) {
	var wg sync.WaitGroup
	sums := make([]uint64, cores)
	for i := 0; i < cores; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var x uint64
			for j := 0; j < 4_000_000; j++ {
				x += uint64(j)*3 + 7
			}
			sums[i] = x
		}(i)
	}
	wg.Wait()
	for _, s := range sums {
		smpSink += s
	}
}

func main() {
	// Eerst het geheugenplafond — automatisch uit het RAM-raam van dit board
	// (cpu/memlimit): Go remt zichzelf af in plaats van door de muur te gaan
	// (de stille OOM-dood van 02-08). Vóór alles, dus ook vóór de bunny.
	memlimit.Arm()

	// Dereks bunny — het origineel, door hemzelf aangeleverd (2026-07-11).
	// Op de UART als banner; op het scherm als vaste header (fb.Header,
	// verderop) die nooit mee-scrolt — zoals Linux zijn logo bovenin laat
	// staan. Zo verdwijnt hij ook nooit meer in een context-compactie. Hop!
	fmt.Println("")
	for _, r := range bunny {
		fmt.Println(r)
	}
	fmt.Println("")

	// Uniforme per-regel-timestamps op de console — ná de bunny (die blijft
	// schoon). Het log-pakket (hop-agent/leader) zet z'n eigen datum uit
	// zodat er nooit een dubbele stempel komt; de console-hook levert er één.
	log.SetFlags(0)
	if b, ok := board.Current().(interface{ EnableTimestamps() }); ok {
		b.EnableTimestamps()
	}

	fmt.Printf("runtime %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	// Vóór alles: het privilege-niveau waarin we booten. De kooi is een
	// invariant, geen optie — kunnen we hem niet zetten, dan starten we niet.
	// Wat "niet kunnen" betekent zegt het board (EL2 op ARM, M-mode op RISC-V).
	if err := board.Current().Privilege(); err != nil {
		fail("boot", err)
	}
	fmt.Println(board.Current().Firmware())

	// KERN-FLIP (docs/kern-flip.md): kwamen we hier doordat de vórige HopOS
	// onder zichzelf vandaan sprong? Dan draaien zijn apps nog — in hun eigen
	// partities, op hun eigen cores — en moeten we ze overnemen in plaats van
	// hun wereld vers neer te zetten. Dit MOET vóór elke slot- of kooi-init:
	// het consumeert het handoff-blob en zet de adoptie-stand waarop de
	// kooi-laag straks beslist of ze de plan-regio met rust laat.
	// Mag deze node zichzelf later vervangen? Dat is één beslissing en hij moet
	// vóór élke kooi-init vaststaan, want hij bepaalt of de switch-code die een
	// app-core uitvoert in het kern-image blijft of naar de plan-regio
	// verhuist. Staat hij uit, dan gedraagt deze node zich byte-voor-byte als
	// vóór de kern-flip bestond — zelfde binary, ander pad.
	//
	// Default AAN: onze eigen images zetten hem in de config, want een node die
	// je beheert wil je kunnen updaten zonder hem te herstarten. Wie dat niet
	// wil zet hopos.flip.enable=0 en krijgt exact het oude gedrag.
	slots.SetFlipCapable(bootParam("hopos.flip.enable") != "0")

	flipped, isFlip := kernflip.Adopted()
	// Is er een flip geweest die NIET geland is, dan staat dat in de
	// vluchtrecorder op de boot-scratch — het enige spoor dat een reboot
	// overleeft. Meteen na Adopted (die de recorder bij een geslaagde landing
	// al wiste), zodat de melding vóór alle andere boot-ruis staat.
	if !isFlip {
		kernflip.ReportLastFlip()
	}
	if isFlip {
		fmt.Printf("flip: adopted kernel generation %d — %s, %d resident(s) handed over, previous kernel had %#x+%dMB HOPOS_FLIP_BOOT\n",
			flipped.Gen, hopBudget(), len(flipped.Slots), flipped.OldBase, flipped.OldSize>>20)
	}
	// hopos.reboot=1: dit image is een herstart-verzoek (watchdog.go
	// rebootNow) — ná het flip-rapport, zodat de console zegt wat er gebeurde.
	if bootParam("hopos.reboot") == "1" {
		rebootNow()
	}

	// Log-console op de firmware-framebuffer als het board er een heeft — het
	// beeld-kanaal voor een node zónder debug-kabel. Zo niet (QEMU -nographic,
	// board vóór zijn beeld-fase): no-op, printk blijft naar UART/log.
	if d, ok := board.Current().Framebuffer(); ok {
		fb.Init(d)
		fb.Header(bunny...) // vaste bunny bovenin, de logs scrollen eronder
		fmt.Printf("console: framebuffer %dx%d @ %#x, %d bpp — mirroring log to display\n",
			d.Width, d.Height, uint64(d.Base), d.BPP)
		// Live meetregels rechts naast de bunny (Derek 15-07): kern-mem,
		// datum, tijd — met seconden, elke seconde ververst: een bevroren
		// klok = een hangende kern, in één oogopslag.
		go screenStatus()
	}

	// Netwerk opbrengen. Geen harde eis (net als storage en SNTP hieronder):
	// een board dat geen link/DHCP krijgt (ProbeNIC faalt hard na zijn eigen
	// time-outs) draait door als headless/compute-node i.p.v. permanent te
	// hangen. Extern verkeer (leader-API, image-download) is dan weg, maar de
	// node blijft leven en kan later herstellen — degradeer, niet fail().
	// De idle-meetlat van HOP's eigen core (node.go idleStat), opt-in — en de
	// knop die de RX-lus terug in de poll-stand zet (hopnet.ForcePoll), vóór
	// Up, want die kiest.
	if bootParam("hopos.idlestat") == "1" {
		go idleStat()
	}
	hopnet.ForcePoll = bootParam("hopos.rxpoll") == "1"
	slots.ForceIdleYield = bootParam("hopos.idleyield") == "1"
	hopswitch.UsePump(func(status func() bool, wake func()) {
		idle.WatchWork(status, wake)
	})
	// HOP is poort 0 van dezelfde LAN-switch. Die poort moet bestaan vóór
	// hopnet zijn device-naad opbouwt; er is geen tweede gateway-queue meer.
	if err := hopswitch.Up(); err != nil {
		fail("switch", err)
	}
	netErr := hopnet.Up()
	// De wekker voor app-cores die op EL2 slapen (IdleYield, Cores.Kick):
	// alleen op een board dat kan kicken, en ná de vectoren — de kick is een
	// HVC naar HOP's eigen EL2-handler.
	slots.StartWaker()
	if netErr != nil {
		fmt.Printf("net: %v — continuing headless/compute-only (no external network)\n", netErr)
	}
	if isFlip {
		// Recorder mee: een geflipte kern die HIER voorbij is stierf niet in
		// de board/NIC-bring-up-op-levende-hardware (de hoofdverdachte).
		kernflip.MarkNetUp()
	}
	// USB-invoer (alleen de gui-smaak): toetsenbord en muis op het ijzer. Hier
	// en niet eerder, want de gebeurtenissen gaan over de interne switch naar
	// de display-app — dezelfde POST /input die de browser-KVM gebruikt.
	startUSBInput()

	// Klok via SNTP. Geen harde eis: HOP's HMAC-auth is klok-vrij, dus een
	// node zonder bereikbare NTP-server draait door — alleen TLS faalt dan.
	if err := hopnet.SyncTime("pool.ntp.org:123"); err != nil {
		fmt.Printf("clock: SNTP failed (%v) — time remains at epoch, TLS will fail\n", err)
	} else {
		fmt.Printf("clock: %s (SNTP)\n", time.Now().UTC().Format(time.RFC3339))
	}
	// Hersync per uur tegen drift (P2b/C6; de teller loopt op de 54MHz-
	// kristal — prima, maar een soak-dag is lang). Stilletjes: alleen
	// falen is het loggen waard.
	go func() {
		for {
			time.Sleep(time.Hour)
			if err := hopnet.SyncTime("pool.ntp.org:123"); err != nil {
				fmt.Printf("clock: resync failed (%v) — retrying in one hour\n", err)
			}
		}
	}()

	// Storage: eigen PCIe-enumeratie → NVMe-driver → hopfs. Zonder schijf
	// draait de node door, maar jobs met volumes weigeren dan bij Start.
	// Een board zonder ECAM-plan (Pi 5: NVMe loopt daar straks via de
	// brcmstb-RC, metal/driver/brcmpcie) slaat de probe over.
	// Een board dat zijn eigen opslag kent, levert hem zelf — inclusief het
	// VENSTER waarin wij mogen schrijven. Dat is geen luxe: op een machine die
	// we met het OS van de eigenaar delen (Mac mini: macOS op dezelfde SSD) is
	// "de hele schijf" precies het verkeerde antwoord.
	if bd, ok := board.Current().(interface {
		Disk() (*nvme.Controller, uint64, uint64, error)
	}); ok {
		if disk, first, count, err := bd.Disk(); err != nil {
			fmt.Printf("storage: %v — running without volumes\n", err)
		} else {
			slots.UseFS(hopfs.NewRange(disk, first, count))
			fmt.Printf("storage: nvme %q — %d MB of our own, LBA %d..%d — volumes available\n",
				disk.Model, count*disk.BlockSize>>20, first, first+count-1)
		}
	} else if win := board.Current().PCIe(); win.ECAMBase == 0 {
		fmt.Println("storage: no ECAM window on this board — running without volumes (NVMe pending)")
	} else if disk, err := nvme.Probe(win, layout.NVMeDMABase, layout.NVMeDMASize); err != nil {
		fmt.Printf("storage: %v — running without volumes\n", err)
	} else {
		slots.UseFS(hopfs.New(disk))
		fmt.Printf("storage: nvme %q, %d MB — volumes available\n",
			disk.Model, disk.Blocks*disk.BlockSize>>20)
	}

	// De bewoners van de vórige kern overnemen (kern-flip): pas hier, want een
	// geadopteerd slot krijgt meteen zijn servicer terug — en die bedient
	// hop-ABI-RPC's die de storage-laag en de switch nodig hebben. Wie zijn
	// heartbeat niet laat lopen wordt niet geadopteerd maar opgeruimd, dus dit
	// pad kan nooit een spookpartitie erven.
	if isFlip && len(flipped.Slots) > 0 {
		live := slots.AdoptSlots(flipped.Slots)
		// En de conntrack erachteraan: de apps leven door, dus hun
		// verbindingen horen dat ook te doen. Ná de adoptie, want een flow van
		// een slot dat het niet haalde hoort niet te blijven staan —
		// RestoreNAT toetst dat op de switch-poort.
		flows := hopswitch.RestoreNAT(flipped.NAT)
		fmt.Printf("flip: %d of %d resident(s) and %d of %d NAT flow(s) survived the kernel swap HOPOS_FLIP_ADOPT\n",
			live, len(flipped.Slots), flows, len(flipped.NAT.Flows))
	}

	// Board-specifiek nawerk: op de Pi's start hier het klokbeleid +
	// de thermiek-telemetrie (metal/driver/dvfs via de firmware-mailbox); QEMU
	// heeft geen mailbox en laat de hook leeg. HOP zelf blijft oblivious.
	if boardExtra != nil {
		boardExtra()
	}

	// De KERN-FLIP heeft bewust maar ÉÉN trigger: het console-commando (`flip
	// <url> <sha256>` op de conport, zie hieronder bij hopos.console). Er was
	// ook een config-gedreven variant (`hopos.flip=<url>` bij boot) en die is
	// gesloopt — één weg, geen opties (Derek 01-09). Beleid — wánneer een node
	// updatet, waarvandaan — hoort niet in de kern maar in een job: een
	// updater-app uit hopos.init[] die het commando naar de eigen console
	// stuurt kan precies hetzelfde, en dan is de kern-kant alleen mechanisme.

	// Hoeveel cores houdt de HOP-runtime voor zichzelf (core 0 telt mee; de rest
	// zijn app-slots)? HopOS leest het uit de platform-config (board-hook):
	// SetHopCores reserveert ze uit de slot-pool — slotmgr biedt HOP de rest —
	// en bij N>1 brengt de node-SMP hieronder de extra cores als Go-Ms op
	// (GOMAXPROCS=N; Go spreidt de node-goroutines zelf). Default 1: geen
	// verspilling bij weinig apps, opt-in hoger (hopos.cores=2) als de flow er
	// druk genoeg voor is.

	nCores := 1
	if n, err := strconv.Atoi(bootParam("hopos.cores")); err == nil && n >= 1 {
		nCores = n
	}
	// Bovengrens op de FYSIEKE cores. Dit is bewust géén grens op het aantal
	// kooien/slots — die mogen de core-telling wél overschrijden (sharegroups
	// stapelen kooien op één core). Maar de node-cores hier zijn echt ijzer:
	// hopos.cores telt core 0 mee en HOP pakt daarnaast cores 1..n-1, dus er
	// moeten n-1 app-cores bestáán. Een typefout op het bootmedium
	// (hopos.cores=22 op een 4-core Pi) liet ConfigureNode anders cores
	// dispatchen die er niet zijn en control-pages buiten het gereserveerde plan
	// gebruiken. Klemmen + luid melden i.p.v. stil scheef booten.
	if max := layout.NumAppCores() + 1; nCores > max {
		fmt.Printf("hop: WARNING hopos.cores=%d exceeds this board's %d physical cores — clamping to %d\n",
			nCores, max, max)
		nCores = max
	}
	slots.SetHopCores(nCores)
	if nCores > 1 {
		// Checkpoints op de console (serial+GOP): op een headless node zijn dit
		// dé bakens die op de kabel tonen hoe ver de node-SMP-bring-up komt.
		fmt.Printf("hop: node-SMP: reserving %d cores, installing vectors...\n", nCores)
		// Vectoren klaar vóór de node-cores opkomen (ze zetten VBAR_EL2 op de
		// revoke-vectoren, net als core 0 uit bootKernel); later in Start no-op.
		slots.EnsureVectors()
		// Geef de node-runtime nCores cores. Dezelfde multicore-machinerie als een
		// app (goos.Task + GOMAXPROCS + de gedeelde EL2-trampoline), maar de node
		// dispatcht zijn eigen cores direct via PSCI (hij ís HOP). Go spreidt de
		// node-goroutines (switch/leader/plaatsing) daarna zelf over de cores.
		smp.ConfigureNode(nCores, nodeDispatch)
		nodeSMPWarmup(nCores)
		// Levensteken op de console: dispatched = door de runtime opgevraagde
		// extra cores; PSCI-state 0 (On) = de core leeft in de node-runtime.
		fmt.Printf("hop: node runtime on %d cores (GOMAXPROCS=%d, dispatched=%d) HOPOS_NODE_SMP\n",
			nCores, runtime.GOMAXPROCS(0), smp.NodeStarted())
		for c := 1; c < nCores; c++ {
			fmt.Printf("hop: node-core %d %s\n", c, nodeCoreState(c))
		}
	}

	sm := slotmgr.New()

	cfg := config.DefaultConfig()
	cfg.Cluster.Name = "hopos"
	if v := bootParam("hopos.cluster"); v != "" {
		cfg.Cluster.Name = v
	}
	// Node-identiteit (P2b/C5): boot-parameter of board-serial — twee nodes
	// op één LAN mogen nooit allebei "hopos-1" heten. QEMU heeft geen van
	// beide en houdt de oude naam.
	cfg.Node.ID = "hopos-1"
	if n := bootParam("hopos.node"); n != "" {
		cfg.Node.ID = n
	} else if s := nodeSerial(); s != "" {
		cfg.Node.ID = s
	}
	cfg.Node.IP = board.Current().Net().IP
	cfg.Node.Port = 8080 // leader-API = 9080

	// Clusterconfig uit de platform-config: hiermee gaan HMAC-auth en de
	// S3-gecommitte clusterstaat (agentboot: persister + LoadCommittedState)
	// aan op ijzer — een reboot herplaatst dan de eigen jobs (declaratief).
	// Zonder deze sleutels: het oude, vluchtige standalone-gedrag. De waarden
	// zelf NOOIT loggen (de key/secret staan alleen op het boot-medium).
	cfg.APIKey = bootParam("hopos.apikey")
	if cfg.APIKey != "" {
		fmt.Println("hop: API authentication enabled (X-Hop-Auth HMAC)")
	}
	// Fail closed zonder sleutel. Een lege APIKey zet de HMAC-middleware
	// volledig uit (httputil.RequireHMAC geeft de handler dan ongewijzigd
	// terug), en de agent/leader luisteren op het LAN: dan kan ELKE host een
	// job dispatchen (POST /v1/jobs, agent /run) op een vertrouwde node. Dat is
	// geen "vluchtig standalone-gedrag" maar ongeauthenticeerde
	// code-uitvoering, dus het moet een expliciete keuze zijn i.p.v. de
	// stilzwijgende default. `hopos.insecure=1` is die keuze (bank/dev);
	// zonder sleutel én zonder die vlag start de API niet en blijft de node
	// gewoon leven — een node die niet luistert is beter dan een open node.
	apiInsecure := bootParam("hopos.insecure") == "1"
	s3 := &cfg.Cluster.Lock.S3
	s3.Endpoint = bootParam("hopos.s3.endpoint")
	s3.Bucket = bootParam("hopos.s3.bucket")
	s3.Region = bootParam("hopos.s3.region")
	s3.AccessKeyID = bootParam("hopos.s3.key")
	s3.SecretAccessKey = bootParam("hopos.s3.secret")
	s3.UsePathStyle = bootParam("hopos.s3.pathstyle") == "1"
	if s3.Bucket != "" && s3.Endpoint != "" {
		cfg.Cluster.Lock.Type = "s3"
		fmt.Printf("hop: cluster %q: S3 committed state on %s/%s — jobs survive reboot\n",
			cfg.Cluster.Name, s3.Endpoint, s3.Bucket)
		// Dezelfde bucket + creds nog één keer: de app-storage. Elke job
		// krijgt zijn eigen map apps/<cluster>/<job>/ (naast leases/<cluster>
		// en state/<cluster>) via de store-ops van de hop-ABI — zie
		// kern/slots/storage.go. De bytes lopen over HOP: de creds en de TLS
		// wonen hier al, de app ziet alleen namen binnen zijn eigen map.
		slots.UseStore(newS3Store(s3), "apps/"+cfg.Cluster.Name)
		fmt.Printf("hop: app object store enabled — apps/%s/<job>/ in the same bucket\n",
			cfg.Cluster.Name)
	}
	if netErr == nil {
		go func() {
			if err := slots.ServeSystem(); err != nil {
				fmt.Printf("system: %v — app calls unavailable\n", err)
			}
		}()
	}

	// Bewust géén template-substitutie ({{host}} e.d.) in de jobspecs: adressen
	// in de config zijn letterlijk. Binnen het slot-net is de node altijd
	// 10.100.0.1 (gateway; Dereks besluit 20-07 — hopswitch/gateway.go) en
	// namen zijn het domein van hopdns, niet van een config-macro.

	// Init-manifest van het boot-medium: elke `hopos.init[]={...}` is één job
	// als compacte JSON (kopieerbaar uit `hop apply`/de API). agentboot seedt ze
	// op een clean boot — zo komt een standalone node ZONDER S3 altijd met zijn
	// baseline op (Derek, 19-07). Ongeldige JSON overslaan met een luide regel;
	// de schema-validatie (verplichte naam e.d.) doet agentboot via DecodeInitJobs.
	for _, spec := range bootParamAll("hopos.init[]") {
		var m map[string]any
		if err := json.Unmarshal([]byte(spec), &m); err != nil {
			fmt.Printf("hop: hopos.init[] skipped — invalid JSON (%v): %s\n", err, spec)
			continue
		}
		cfg.Cluster.InitJobs = append(cfg.Cluster.InitJobs, m)
	}
	if n := len(cfg.Cluster.InitJobs); n > 0 {
		fmt.Printf("hop: %d init job(s) from boot config — seeded on a clean boot\n", n)
	}

	// App-catalogus (19-07, launcher): elke `hopos.apps[]={...}` is één
	// beschikbare-maar-niet-gestarte jobspec, zelfde vorm als hopos.init[].
	// HopOS geeft de bundel als HOPOS_APPS-env aan elk slot dat die sleutel
	// (leeg) in zijn jobspec declareert — opt-in, want de env-blob is schaars
	// (layout.CtrlEnvMax). De launcher toont ze en POST ze onaangeroerd naar
	// de agent. Gewoon parameters die toevallig door een app gelezen worden;
	// HopOS zelf doet er verder niets mee.
	var apps []string
	for _, spec := range bootParamAll("hopos.apps[]") {
		if !json.Valid([]byte(spec)) {
			fmt.Printf("hop: hopos.apps[] skipped — invalid JSON: %s\n", spec)
			continue
		}
		apps = append(apps, spec)
	}
	if len(apps) > 0 {
		fmt.Printf("hop: %d app(s) in the boot-config catalog (HOPOS_APPS)\n", len(apps))
	}
	slotEnv := envSlots{
		SlotManager: sm,
		always:      map[string]string{"HOPOS_HOST": cfg.Node.IP},
		optin:       map[string]string{"HOPOS_APPS": "[" + strings.Join(apps, ",") + "]"},
	}

	// Geheugen. HOP kent per job de MemoryLimit en overspawnt nooit — dus het
	// getal dat we aanbieden is de plaatsings-ceiling. Twee dingen bewaken:
	//  1. Heeft de node fysiek genoeg RAM voor het (statische) layout? Zo
	//     niet, dan zouden slots/ringen buiten het echte RAM vallen — stille
	//     corruptie. Dan weigeren we hard i.p.v. door te draaien.
	//  2. Bied HOP exact de slot-capaciteit aan die we kunnen waarmaken.
	// De gedetecteerde DRAM (via de DTB, x0) is de bron; faalt de detectie,
	// dan vertrouwen we op het layout (QEMU zet x0 niet — zie board/fdt).
	offer := slots.PoolBytes() // HOP alloceert hieruit per job (dynamische partities)
	// Zelf-plannende boards (uefi/ACPI) hebben de pool al op de gemeten vrije
	// RAM getrimd (board-init, UsableRun) — dan is de RequiredRAM-check
	// betekenisloos (hij mengt bovendien de board-eigen adressen met qemuvirt's
	// HopRAMStart). Alleen de statische-layout-mains (QEMU/Pi) toetsen tegen
	// RequiredRAM.
	selfPlanned := false
	if sp, ok := board.Current().(interface{ SelfPlannedPool() bool }); ok {
		selfPlanned = sp.SelfPlannedPool()
	}
	if total := board.Current().MemTotal(); selfPlanned {
		fmt.Printf("memory: %d MB DRAM — board trimmed the pool to free RAM; offering HOP a %d MB partition pool (allocated per job)\n",
			total>>20, offer>>20)
	} else if total > 0 {
		if total < layout.RequiredRAM() {
			fail("memory", fmt.Errorf("node has %d MB DRAM, layout requires %d MB (slots/rings would fall outside RAM)",
				total>>20, layout.RequiredRAM()>>20))
		}
		fmt.Printf("memory: %d MB DRAM (DTB) — layout requires %d MB; offering HOP a %d MB partition pool (allocated per job)\n",
			total>>20, layout.RequiredRAM()>>20, offer>>20)
	} else {
		// LUID, niet stil: geen geldige DTB (UEFI/ACPI, of een kromme blob) →
		// MemTotal==0. De RAM-sanity-check hierboven (fysiek genoeg voor het
		// layout?) wordt daardoor OVERGESLAGEN en de pool is een terugval, niet
		// gemeten RAM. Op dit board draait HOP blind op de statische aannames.
		fmt.Printf("WARNING HOPOS_RAM_CHECK_SKIPPED: no valid DTB (MemTotal=0) — skipping the RAM sanity check against layout.RequiredRAM (%d MB); trusting the static layout, offering HOP a %d MB partition pool (allocated per job)\n",
			layout.RequiredRAM()>>20, offer>>20)
	}

	// De VORM van de pool, niet alleen zijn som. Een pool van drie regio's kan
	// 60MB vrij hebben zonder ergens 36MB aan één stuk, en dan laat de toelating
	// een job toe die de plaatser moet weigeren — die hand-back-lus velde deze
	// node drie keer op 19-08. Eén regel bij de boot maakt dat zichtbaar vóór er
	// iets geplaatst is, en hij is het meetinstrument voor elke HopBase-hersnit:
	// staat er één regio, dan is de fragmentatie weg.
	pool := layout.Pool()
	fmt.Printf("memory: pool is %d region(s), largest placeable %d MB —", len(pool), slots.PoolLargest()>>20)
	for _, r := range pool {
		fmt.Printf(" [%#x+%dMB]", r.Base, r.Size>>20)
	}
	fmt.Println()

	// HOP's eigen budget náást de pool. Dit is het getal waarop de HopBase-keuze
	// van een board wordt afgerekend: elke MB die HOP niet gebruikt hoort bij de
	// apps. Zonder deze regel is "kan HOP met minder?" een gok — hiermee is het
	// een meting. Let op: dit is de bóót-stand; de piek zit bij een
	// job-plaatsing (imagecopy), en die staat in dezelfde vorm bij de eerste
	// geslaagde start.
	fmt.Printf("memory: HOP itself has %s — %s\n", hopBudget(), hopUsage())
	slots.SetKernMem(hopUsage)

	// Zonder extern netwerk kan de agent/leader niet luisteren: net.SocketFunc is
	// nil, dus agentboot.Run zou meteen falen en fail("agent") de node alsnog
	// permanent hangen — ná een misleidend HOPOS_AGENT_UP. Degradeer echt: de
	// interne switch, klok, storage en dvfs draaien al; blijf headless leven
	// (een reboot of latere link herstelt) i.p.v. de agent te starten en te faulten.
	if netErr != nil {
		park(fmt.Sprintf("hop: headless — no external network, agent/leader not started; node %s stays alive HOPOS_NODE_HEADLESS",
			cfg.Node.ID))
	}

	// De console óók op een TCP-poort, als de config dat vraagt: `hopos.console=<poort>`.
	// Standaard UIT — deze poort geeft élke lezer alles wat de node print, en dat
	// is diagnose-gemak op een bank en een lek daarbuiten. Hier en niet eerder:
	// hij leunt op de netstack die net omhoog kwam, en vóór de agent zodat een
	// node die op de auth-poort blijft hangen (hieronder) alsnog te lezen is —
	// juist dán wil je erbij.
	if n, err := strconv.Atoi(bootParam("hopos.console")); err == nil && n > 0 {
		conport.Serve(n)
	}

	// De auth-poort (zie hopos.apikey hierboven): zonder sleutel en zonder
	// expliciete opt-out gaat de API niet open. De node blijft leven — switch,
	// klok, storage en dvfs draaien al — zodat dit een configuratiefout is die
	// je op de console ziet, niet een node die stilletjes op het LAN staat te
	// wachten op de eerste willekeurige POST /v1/jobs.
	if cfg.APIKey == "" && !apiInsecure {
		park(fmt.Sprintf("hop: REFUSING to start agent/leader — no hopos.apikey set, so the HTTP API would accept unauthenticated job dispatch from any host on the LAN.\n"+
			"     Set hopos.apikey=<random-hex> on the boot medium, or hopos.insecure=1 to accept an open API on purpose.\n"+
			"     Node %s stays alive without the API. HOPOS_API_NO_AUTH", cfg.Node.ID))
	}
	if apiInsecure && cfg.APIKey == "" {
		fmt.Println("hop: WARNING — API authentication is OFF (hopos.insecure=1): any host that can reach this node can dispatch jobs. HOPOS_API_INSECURE")
	}
	// Wat dít board over zichzelf te bekennen heeft. Naast de auth-waarschuwing,
	// want ze horen bij elkaar: een operator die hier zijn beslissing op baseert
	// wil ze in één blik zien, en niet ergens in de boot-ruis erboven.
	if boardWarn != nil {
		boardWarn()
	}

	// De node-watchdog: één beleid voor elk board (watchdog.go), ná boardWarn
	// zodat een board dat zijn WDT-blok eerst moet bewijzen (de hart-probe op
	// de LicheeRV) die uitslag heeft.
	go nodeCanary()

	// De flip is pas écht geland als de agent gaat draaien: recorder leeg.
	kernflip.BootLanded()
	fmt.Printf("hop: agent starting — node %s, agent :%d, leader :%d — HOPOS_AGENT_UP\n",
		cfg.Node.ID, cfg.Node.Port, cfg.Node.Port+1000)

	// PID-1-regel: Run blokkeert; keert hij terug, dan is dat een fout.
	err := agentboot.Run(context.Background(), agentboot.Options{
		Config:      cfg,
		NodeID:      cfg.Node.ID,
		Slots:       slotEnv,
		MemoryBytes: offer,
		// De kern-flip, beide kanten (docs/kern-flip.md). RestoreState geeft
		// de agent de jobs en taken van zijn voorganger terug — zonder dat
		// kent hij zijn eigen doorlopende taken niet meer en wil hij ze
		// plaatsen op slots die ze al bezetten. OnSnapshot is de weg terug:
		// de flip leest de state vlak vóór hij springt.
		//
		// Dit werkt óók zonder S3, en dat is precies waarom het hier zit en
		// niet uit de gecommitte clusterstaat komt: een standalone node met
		// alleen hopos.init[] heeft geen bron om uit te herstellen.
		RestoreState: flipped.Agent,
		OnSnapshot:   kernflip.UseAgentState,
		// De kern-flip op aanvraag: POST /flip op de agent-API, achter
		// dezelfde HMAC als job-dispatch — wie een kern mag aanleveren moet
		// bewijzen wat een job-gever bewijst. Dit is de ENIGE trigger (Derek
		// 01-09; een console-commando en een config-flip zijn er geweest en
		// gesloopt — niet terugbrengen). Alleen aangeboden als deze node ook
		// echt kan flippen, anders zegt het endpoint eerlijk 501.
		OnFlip: flipRequest(),
		// De die-temperatuur op elke heartbeat (board.Thermometer; 0 = geen
		// sensor) — zichtbaar in `hop agents`.
		Temp: board.TempMilliC,
	})
	fail("agent", err)
}

// flipRequest is de OnFlip-haak voor de agent (POST /flip): het mechanisme
// zit in kern/kernflip, het beleid bij de aanvrager. nil als deze node niet
// kán flippen (hopos.flip.enable=0) — het endpoint meldt dan 501 in plaats
// van een verzoek aan te nemen dat toch strandt.
func flipRequest() func(url, sha string) error {
	if bootParam("hopos.flip.enable") == "0" {
		return nil
	}
	return func(url, sha string) error {
		return kernflip.FlipFromURL(url, sha)
	}
}

// envSlots vult de slot-env aan bij elke start: `always` gaat er altijd in
// (mits de jobspec de sleutel niet zelf zet — de spec wint), `optin` alleen
// als de jobspec de sleutel leeg declareert ("HOPOS_APPS":"" → HopOS vult
// hem). Zo krijgt elke app HOPOS_HOST gratis, maar betaalt alleen wie erom
// vraagt de env-ruimte van de app-catalogus. Puur een schil om de slotmgr —
// de rest van het SlotManager-contract gaat er onaangeroerd doorheen.
type envSlots struct {
	hopos.SlotManager
	always map[string]string
	optin  map[string]string
}

// PoolLargest reist niet mee via de ingebedde interface — hopos.PoolReporter
// staat bewust NAAST SlotManager — dus geeft de schil hem expliciet door. Zonder
// dit ziet HOP's toelating de optionele interface niet en valt hij terug op de
// som, wat precies het gedrag is dat we wilden weghalen.
func (e envSlots) PoolLargest() uint64 {
	if pr, ok := e.SlotManager.(hopos.PoolReporter); ok {
		return pr.PoolLargest()
	}
	return 0
}

func (e envSlots) StartStream(slot int, image io.Reader, size int64, spec hopos.StartSpec) error {
	spec.Env = e.merge(spec.Env)
	return e.SlotManager.StartStream(slot, image, size, spec)
}

func (e envSlots) merge(env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+len(e.always))
	for k, v := range env {
		out[k] = v
	}
	for k, v := range e.always {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	for k, v := range e.optin {
		if cur, ok := out[k]; ok && cur == "" {
			out[k] = v
		}
	}
	return out
}
