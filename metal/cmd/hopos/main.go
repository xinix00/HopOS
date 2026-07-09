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
	_ "unsafe"

	"hop/pkg/agentboot"
	"hop/pkg/config"

	"hop-os/metal/board"
	_ "hop-os/metal/board/qemuvirt" // registreert het board (init) + tamago-hooks
	"hop-os/metal/fb"
	"hop-os/metal/hopfs"
	"hop-os/metal/hopnet"
	"hop-os/metal/hopswitch"
	"hop-os/metal/layout"
	"hop-os/metal/nvme"
	"hop-os/metal/slotmgr"
	"hop-os/metal/slots"
)

//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = layout.HopRAMStart

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = layout.HopRAMSize

func fail(what string, err error) {
	fmt.Printf("FAIL %s: %v\nHOPOS_AGENT_FAIL\n", what, err)
	for {
		time.Sleep(time.Hour)
	}
}

func main() {
	fmt.Println("")
	fmt.Println("HopOS: HOP-agent bare-metal op arm64 — geen Linux aan boord")
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

	// Storage: eigen PCIe-enumeratie → NVMe-driver → hopfs. Zonder schijf
	// draait de node door, maar jobs met volumes weigeren dan bij Start.
	if disk, err := nvme.Probe(board.Current().PCIe(), layout.NVMeDMABase, layout.NVMeDMASize); err != nil {
		fmt.Printf("storage: %v — node draait zonder volumes\n", err)
	} else {
		slots.UseFS(hopfs.New(disk))
		fmt.Printf("storage: nvme %q, %dMB — volumes beschikbaar\n",
			disk.Model, disk.Blocks*disk.BlockSize>>20)
	}

	sm := slotmgr.New()

	cfg := config.DefaultConfig()
	cfg.Cluster.Name = "hopos"
	cfg.Node.ID = "hopos-1"
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
