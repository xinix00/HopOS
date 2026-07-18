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
	"fmt"
	"log"
	"runtime"
	"strconv"
	"sync"
	"time"

	// TLS-wortels: tamago heeft geen OS en dus geen system-CA-store — zonder
	// deze fallback-bundel (de Mozilla-roots die Go zelf meelevert) faalt
	// élke https-fetch op certificaatvalidatie. Nodig voor het S3-artifact-
	// pad (P2b, gemeten 2026-07-11: lege x509-pool op de node).
	_ "golang.org/x/crypto/x509roots/fallback"

	"hop/pkg/agentboot"
	"hop/pkg/config"

	"hop-os/metal/abi/layout"
	"hop-os/metal/board"
	"hop-os/metal/cpu/smp"
	"hop-os/metal/driver/fb"
	"hop-os/metal/driver/nvme"
	"hop-os/metal/kern/hopfs"
	"hop-os/metal/kern/slotmgr"
	"hop-os/metal/kern/slots"
	"hop-os/metal/net/hopnet"
	"hop-os/metal/net/hopswitch"
)

func fail(what string, err error) {
	fmt.Printf("FAIL %s: %v\nHOPOS_AGENT_FAIL\n", what, err)
	for {
		time.Sleep(time.Hour)
	}
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

// bootParam leest één sleutel uit de platform-config — HopOS leest die, want
// HOP-userspace kan er niet bij: op de Pi's is dat cmdline.txt (via de DTB),
// op UEFI-boards hopos.cfg op de stick (door de stub via de firmware-FAT
// gelezen). Dezelfde sleutels op beide platforms: hopos.cores, hopos.node,
// hopos.cluster, hopos.apikey, hopos.s3.* — configureren = het tekstbestandje
// bewerken, geen rebuild. "" = niet gezet → de default hieronder in main.
var bootParam = func(key string) string { return "" }

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
func nodeSMPWarmup(cores int) {
	var wg sync.WaitGroup
	for i := 0; i < cores; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var x uint64
			for j := 0; j < 4_000_000; j++ {
				x += uint64(j)*3 + 7
			}
			smpSink = x
		}()
	}
	wg.Wait()
}

func main() {
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

	// Vóór de eerste PSCI-call (SMC): HopOS eist een EL2-boot — de
	// stage-2-kooi is een invariant, geen optie.
	if el := board.Current().BootEL(); el < 2 {
		fail("boot", fmt.Errorf("booted at EL%d: HopOS requires EL2 (QEMU: virtualization=on)", el))
	}

	major, minor := board.Current().PSCIVersion()
	fmt.Printf("psci: v%d.%d (boot EL%d, SMC conduit)\n", major, minor, board.Current().BootEL())

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
	netErr := hopnet.Up()
	if netErr != nil {
		fmt.Printf("net: %v — continuing headless/compute-only (no external network)\n", netErr)
	}
	// De interne L2-switch (per-slot netwerk): elke task krijgt een adres op
	// het interne net en kan met appnet een eigen stack opbrengen.
	if err := hopswitch.Up(); err != nil {
		fail("switch", err)
	}

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
	if win := board.Current().PCIe(); win.ECAMBase == 0 {
		fmt.Println("storage: no ECAM window on this board — running without volumes (NVMe pending)")
	} else if disk, err := nvme.Probe(win, layout.NVMeDMABase, layout.NVMeDMASize); err != nil {
		fmt.Printf("storage: %v — running without volumes\n", err)
	} else {
		slots.UseFS(hopfs.New(disk))
		fmt.Printf("storage: nvme %q, %d MB — volumes available\n",
			disk.Model, disk.Blocks*disk.BlockSize>>20)
	}

	// Board-specifiek nawerk: op de Pi's start hier het klokbeleid +
	// de thermiek-telemetrie (metal/driver/dvfs via de firmware-mailbox); QEMU
	// heeft geen mailbox en laat de hook leeg. HOP zelf blijft oblivious.
	if boardExtra != nil {
		boardExtra()
	}

	// Hoeveel cores houdt de HOP-runtime voor zichzelf? HopOS leest het uit de
	// platform-config (board-hook) en past het toe: SetHopCores reserveert de
	// cores uit de slot-pool (slotmgr biedt HOP de rest), en bij N>1 brengt de
	// node-SMP hieronder de extra cores als Go-Ms op (GOMAXPROCS=N; Go spreidt de
	// node-goroutines zelf). N=1 (default) = geen reservering, geen extra cores.
	// Hoeveel cores voor de HOP-runtime (core 0 telt mee; de rest zijn
	// app-slots). Default 1: geen verspilling bij weinig apps — opt-in hoger
	// (hopos.cores=2) als de flow er druk genoeg voor is; >1 zet GOMAXPROCS en
	// Go spreidt de node-goroutines zelf. Slotmgr biedt HOP (totaal − N) slots.
	nCores := 1
	if n, err := strconv.Atoi(bootParam("hopos.cores")); err == nil && n >= 1 {
		nCores = n
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
		smp.ConfigureNode(nCores, func(core int, entry, ctx uint64) {
			board.Current().CPUOn(uint64(core), entry, ctx)
		})
		nodeSMPWarmup(nCores)
		// Levensteken op de console: dispatched = door de runtime opgevraagde
		// extra cores; PSCI-state 0 (On) = de core leeft in de node-runtime.
		fmt.Printf("hop: node runtime on %d cores (GOMAXPROCS=%d, dispatched=%d) HOPOS_NODE_SMP\n",
			nCores, runtime.GOMAXPROCS(0), smp.NodeStarted())
		for c := 1; c < nCores; c++ {
			fmt.Printf("hop: node-core %d PSCI-state=%d\n", c, board.Current().AffinityInfo(uint64(c)))
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

	// Zonder extern netwerk kan de agent/leader niet luisteren: net.SocketFunc is
	// nil, dus agentboot.Run zou meteen falen en fail("agent") de node alsnog
	// permanent hangen — ná een misleidend HOPOS_AGENT_UP. Degradeer echt: de
	// interne switch, klok, storage en dvfs draaien al; blijf headless leven
	// (een reboot of latere link herstelt) i.p.v. de agent te starten en te faulten.
	if netErr != nil {
		fmt.Printf("hop: headless — no external network, agent/leader not started; node %s stays alive HOPOS_NODE_HEADLESS\n",
			cfg.Node.ID)
		for {
			time.Sleep(time.Hour)
		}
	}

	fmt.Printf("hop: agent starting — node %s, agent :%d, leader :%d — HOPOS_AGENT_UP\n",
		cfg.Node.ID, cfg.Node.Port, cfg.Node.Port+1000)

	// PID-1-regel: Run blokkeert; keert hij terug, dan is dat een fout.
	err := agentboot.Run(context.Background(), agentboot.Options{
		Config:      cfg,
		NodeID:      cfg.Node.ID,
		Slots:       sm,
		MemoryBytes: offer,
	})
	fail("agent", err)
}
