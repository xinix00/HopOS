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
	"runtime"
	"time"

	"hop/pkg/agentboot"
	"hop/pkg/config"

	"hop-os/metal/board"
	"hop-os/metal/fb"
	"hop-os/metal/hopfs"
	"hop-os/metal/hopnet"
	"hop-os/metal/hopswitch"
	"hop-os/metal/layout"
	"hop-os/metal/nvme"
	"hop-os/metal/slotmgr"
	"hop-os/metal/slots"
)

func fail(what string, err error) {
	fmt.Printf("FAIL %s: %v\nHOPOS_AGENT_FAIL\n", what, err)
	for {
		time.Sleep(time.Hour)
	}
}

// boardExtra: optioneel board-specifiek nawerk (gezet door board_*.go in
// zijn init) — de Pi's starten er het klokbeleid mee.
var boardExtra func()

// nodeName: board-specifieke node-identiteit. Op de Pi's: eerst de
// boot-parameter hopos.node= (cmdline.txt op de kaart — configureren
// zonder rebuild), anders "hopos-<serial>"; "" = generieke naam.
var nodeName = func() string { return "" }

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
	fmt.Printf("runtime %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	// Vóór de eerste PSCI-call (SMC): HopOS eist een EL2-boot — de
	// stage-2-kooi is een invariant, geen optie.
	if el := board.Current().BootEL(); el < 2 {
		fail("boot", fmt.Errorf("EL%d-boot: HopOS vereist EL2 (QEMU: virtualization=on)", el))
	}

	major, minor := board.Current().PSCIVersion()
	fmt.Printf("PSCI versie %d.%d (boot-EL%d, conduit SMC)\n", major, minor, board.Current().BootEL())

	// Log-console op de firmware-framebuffer als het board er een heeft — het
	// beeld-kanaal voor een node zónder debug-kabel. Zo niet (QEMU -nographic,
	// board vóór zijn beeld-fase): no-op, printk blijft naar UART/log.
	if d, ok := board.Current().Framebuffer(); ok {
		fb.Init(d)
		fb.Header(bunny...) // vaste bunny bovenin, de logs scrollen eronder
		fmt.Printf("console: framebuffer %dx%d @ %#x (%d-bpp) — logs op scherm\n",
			d.Width, d.Height, uint64(d.Base), d.BPP)
	}

	if err := hopnet.Up(); err != nil {
		fail("net", err)
	}
	// De interne L2-switch (per-slot netwerk): elke task krijgt een adres op
	// het interne net en kan met appnet een eigen stack opbrengen.
	if err := hopswitch.Up(); err != nil {
		fail("switch", err)
	}

	// Klok via SNTP. Geen harde eis: HOP's HMAC-auth is klok-vrij, dus een
	// node zonder bereikbare NTP-server draait door — alleen TLS faalt dan.
	if err := hopnet.SyncTime("pool.ntp.org:123"); err != nil {
		fmt.Printf("klok: sntp mislukt (%v) — tijd blijft 1970, TLS zal falen\n", err)
	} else {
		fmt.Printf("klok: %s (SNTP)\n", time.Now().UTC().Format(time.RFC3339))
	}
	// Hersync per uur tegen drift (P2b/C6; de teller loopt op de 54MHz-
	// kristal — prima, maar een soak-dag is lang). Stilletjes: alleen
	// falen is het loggen waard.
	go func() {
		for {
			time.Sleep(time.Hour)
			if err := hopnet.SyncTime("pool.ntp.org:123"); err != nil {
				fmt.Printf("klok: hersync mislukt (%v) — volgende poging over een uur\n", err)
			}
		}
	}()

	// Storage: eigen PCIe-enumeratie → NVMe-driver → hopfs. Zonder schijf
	// draait de node door, maar jobs met volumes weigeren dan bij Start.
	// Een board zonder ECAM-plan (Pi 5: NVMe loopt daar straks via de
	// brcmstb-RC, metal/brcmpcie) slaat de probe over.
	if win := board.Current().PCIe(); win.ECAMBase == 0 {
		fmt.Println("storage: geen ECAM-plan op dit board — node draait zonder volumes (NVMe volgt)")
	} else if disk, err := nvme.Probe(win, layout.NVMeDMABase, layout.NVMeDMASize); err != nil {
		fmt.Printf("storage: %v — node draait zonder volumes\n", err)
	} else {
		slots.UseFS(hopfs.New(disk))
		fmt.Printf("storage: nvme %q, %dMB — volumes beschikbaar\n",
			disk.Model, disk.Blocks*disk.BlockSize>>20)
	}

	// Board-specifiek nawerk: op de Pi's start hier het klokbeleid +
	// de thermiek-telemetrie (metal/dvfs via de firmware-mailbox); QEMU
	// heeft geen mailbox en laat de hook leeg. HOP zelf blijft oblivious.
	if boardExtra != nil {
		boardExtra()
	}

	sm := slotmgr.New()

	cfg := config.DefaultConfig()
	cfg.Cluster.Name = "hopos"
	// Node-identiteit (P2b/C5): boot-parameter of board-serial — twee nodes
	// op één LAN mogen nooit allebei "hopos-1" heten. QEMU heeft geen van
	// beide en houdt de oude naam.
	cfg.Node.ID = "hopos-1"
	if n := nodeName(); n != "" {
		cfg.Node.ID = n
	}
	cfg.Node.IP = board.Current().Net().IP
	cfg.Node.Port = 8080 // leader-API = 9080

	// Geheugen. HOP kent per job de MemoryLimit en overspawnt nooit — dus het
	// getal dat we aanbieden is de plaatsings-ceiling. Twee dingen bewaken:
	//  1. Heeft de node fysiek genoeg RAM voor het (statische) layout? Zo
	//     niet, dan zouden slots/ringen buiten het echte RAM vallen — stille
	//     corruptie. Dan weigeren we hard i.p.v. door te draaien.
	//  2. Bied HOP exact de slot-capaciteit aan die we kunnen waarmaken.
	// De gedetecteerde DRAM (via de DTB, x0) is de bron; faalt de detectie,
	// dan vertrouwen we op het layout (QEMU zet x0 niet — zie board/fdt).
	offer := slots.PoolBytes() // HOP alloceert hieruit per job (dynamische partities)
	if total := board.Current().MemTotal(); total > 0 {
		if total < layout.RequiredRAM() {
			fail("geheugen", fmt.Errorf("node heeft %d MB DRAM, layout vereist %d MB (slots/ringen zouden buiten RAM vallen)",
				total>>20, layout.RequiredRAM()>>20))
		}
		fmt.Printf("geheugen: %d MB DRAM (DTB) — layout vereist %d MB; HOP krijgt een %d MB partitie-pool (per job dynamisch)\n",
			total>>20, layout.RequiredRAM()>>20, offer>>20)
	} else {
		fmt.Printf("geheugen: DTB-detectie faalde — vertrouw op het layout, HOP krijgt een %d MB partitie-pool (per job dynamisch)\n", offer>>20)
	}

	fmt.Printf("HOP-agent start: node %s, agent :%d, leader :%d — HOPOS_AGENT_UP\n",
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
