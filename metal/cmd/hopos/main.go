// HopOS met de echte HOP-agent aan boord (QEMU virt, fase 1): core 0 boot,
// brengt het netwerk op en start hop's agent + leader (pkg/agentboot) met de
// slot-manager als runner-backend. Jobs met driver "hop" komen binnen via de
// leader-API (:9080), de agent (:8080) downloadt de app-image en start hem
// op een vrije core — dezelfde HOP-bytes als op Linux/macOS, zonder Linux.
//
// Steiger (fase 1): standalone-cluster (deze node is z'n eigen leader) en
// app-images zijn per slot gelinkt — de artifact-URL moet dus matchen met het
// slot dat HopRunner kiest (eerste vrije). Beide gaan eruit zodra
// hoplockserver-over-netwerk (fase 2) en het definitieve imageformaat
// (fase 4, PLAN.md §4.4) er zijn.
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

	major, minor := board.Current().PSCIVersion()
	fmt.Printf("PSCI versie %d.%d (boot-EL%d, conduit %s)\n",
		major, minor, board.Current().BootEL(), map[bool]string{true: "SMC", false: "HVC"}[board.Current().BootEL() >= 2])

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

	// Geheugen dat we HOP aanbieden. HOP kent per job de MemoryLimit en
	// overspawnt nooit (leader-capaciteit) — dus dit getal is de ceiling
	// waartegen hij plant. We melden het gedetecteerde DRAM (uit de DTB),
	// maar begrensd tot wat de huidige slot-layout ook echt kan waarmaken
	// (11 × SlotStride): tot de dynamische partities er zijn (gap #1, deel 2)
	// zou méér aanbieden HOP jobs laten plaatsen die slots.Start weigert.
	slotCap := uint64(layout.MaxSlots) * layout.SlotStride
	offer := slotCap
	if total := board.Current().MemTotal(); total > 0 {
		fmt.Printf("geheugen: %d MB DRAM gedetecteerd (DTB); slot-layout kan er nu %d MB van benutten\n",
			total>>20, slotCap>>20)
		if total < offer {
			offer = total // klein board: nooit meer aanbieden dan er fysiek is
		}
	} else {
		fmt.Printf("geheugen: DTB-detectie faalde — val terug op slot-layout (%d MB)\n", slotCap>>20)
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
