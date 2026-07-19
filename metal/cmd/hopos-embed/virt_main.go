// HopOS fase-1-demo op QEMU -M virt: de HOP-kern (core 0) beheert app-slots
// zoals HOP's HopRunner dat straks doet — Start (met MemoryLimit-patch),
// Status (power + heartbeat), Stop (kill), en restart. Drie apps op drie
// cores, elk een eigen Go-runtime in een eigen partitie.
//
// In het echte systeem komen de images gesigneerd uit S3 via de HOP-agent;
// hier zijn ze embedded — laden/starten/stoppen is identiek.

//go:build qemuvirt

package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"time"
	"unsafe"

	"hop-os/metal/abi/checksum"
	"hop-os/metal/abi/layout"
	"hop-os/metal/board"
	_ "hop-os/metal/board/qemuvirt/hop" // registreert het board (init) + basis-hooks
	"hop-os/metal/dev"
	"hop-os/metal/driver/fb"
	"hop-os/metal/driver/nvme"
	"hop-os/metal/kern/hopfs"
	"hop-os/metal/kern/slots"
	"hop-os/metal/net/hopnet"
	"hop-os/metal/net/hopswitch"
)

// nvmeDemo bewijst PCIe-ECAM + de eigen NVMe-driver (fase-3-voorwerk):
// enumereer bus 0, wijs zelf BAR0 toe (er is geen firmware die dat deed),
// schrijf een blok scratch en lees het terug. Geeft de controller terug —
// daar bouwt hopfs (de storage-laag) op verder.
func nvmeDemo() (*nvme.Controller, error) {
	win := board.Current().PCIe()
	ctrl, err := nvme.Probe(win, layout.NVMeDMABase, layout.NVMeDMASize)
	if err != nil {
		return nil, err
	}
	fmt.Printf("nvme: %q, %d blokken × %dB @ %#x\n",
		ctrl.Model, ctrl.Blocks, ctrl.BlockSize, uint64(win.MMIOBase))

	wr := make([]byte, 4096)
	for i := range wr {
		wr[i] = byte(i*7 + 3)
	}
	if err := ctrl.Write(8, wr); err != nil {
		return nil, err
	}
	rd := make([]byte, 4096)
	if err := ctrl.Read(8, rd); err != nil {
		return nil, err
	}
	if !bytes.Equal(wr, rd) {
		return nil, fmt.Errorf("teruggelezen blok verschilt van geschreven blok")
	}
	return ctrl, nil
}

// waitExit en drainLogs zijn gedeeld met de Pi-mains (helpers.go).

// fbconsDemo bewijst de universele log-console (metal/driver/fb) op de échte
// toolchain: QEMU -M virt heeft geen firmware-framebuffer, dus we richten er
// zelf één in in gewoon RAM, laten de renderer erin tekenen en lezen de pixels
// terug. Dit is exact het pad dat op een board de GOP/simplefb-buffer voedt
// (alleen de discovery verschilt) — plus de printk-mirror, want fmt-output
// gaat na Init ook naar deze buffer. Daarna Disable: de rest van de demo
// mirrort niet in dit wegwerp-buffertje.
func fbconsDemo() error {
	const w, h = 64, 32
	const stride = w * 4
	buf := make([]byte, h*stride)
	base := uintptr(unsafe.Pointer(&buf[0]))
	fb.Init(fb.Desc{Base: base, Width: w, Height: h, Stride: stride, BPP: 32})
	if !fb.Active() {
		return fmt.Errorf("fb.Init activeerde de console niet")
	}
	// Init veegt uniform naar de achtergrondkleur.
	bg := dev.Read32(base)
	for off := 0; off < len(buf); off += 4 {
		if dev.Read32(base+uintptr(off)) != bg {
			fb.Disable()
			return fmt.Errorf("Init veegde niet uniform (@%d)", off)
		}
	}
	// Een dichte glyph tekenen; de eerste 16x16-cel moet nu voorgrondpixels
	// hebben, en de cel ernaast onaangeroerd (geen schrijven buiten de cel).
	fb.SetColor(0xFFFFFFFF)
	fb.Putc('#')
	fgSeen := false
	for gy := 0; gy < cellPx; gy++ {
		for gx := 0; gx < cellPx; gx++ {
			if dev.Read32(base+uintptr(gy*stride+gx*4)) != bg {
				fgSeen = true
			}
		}
	}
	if !fgSeen {
		fb.Disable()
		return fmt.Errorf("glyph tekende geen enkele pixel")
	}
	if dev.Read32(base+uintptr(2*cellPx*4)) != bg { // ruim voorbij cel 0, rij 0
		fb.Disable()
		return fmt.Errorf("glyph schreef buiten zijn cel")
	}
	fb.Disable()
	return nil
}

// cellPx is de pixelmaat van één tekencel in metal/driver/fb (8x8-font op 2×).
const cellPx = 16

// serveHello opent de demo-poort: het bewijs dat de netstack werkt.
func serveHello() error {
	l, err := net.Listen("tcp4", ":80")
	if err != nil {
		return fmt.Errorf("listen :80: %w", err)
	}
	go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "HopOS leeft — bare-metal Go op %s, geen Linux aan boord.\n", board.Current().Net().IP)
	}))
	return nil
}

// Eén canoniek gelinkte app-image (slot-1-bereik, zie image/qemu-run.sh):
// de stage-2-map legt hem op de partitie van elk slot.
//
//go:embed app.elf
var app []byte

func fail(what string, err error) {
	fmt.Printf("FAIL %s: %v\nHOPOS_SLOTS_FAIL\n", what, err)
	for {
		time.Sleep(time.Hour)
	}
}

func main() {
	fmt.Println("")
	fmt.Println("HopOS (virt): bare-metal Go op arm64 — geen Linux aan boord")
	fmt.Printf("runtime %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)

	// Vóór de eerste PSCI-call (SMC): HopOS eist een EL2-boot — de
	// stage-2-kooi is een invariant, geen optie.
	if el := board.Current().BootEL(); el < 2 {
		fail("boot", fmt.Errorf("EL%d-boot: HopOS vereist EL2 (QEMU: virtualization=on)", el))
	}

	major, minor := board.Current().PSCIVersion()
	fmt.Printf("PSCI versie %d.%d (boot-EL%d, conduit SMC)\n", major, minor, board.Current().BootEL())

	// Universele log-console (metal/driver/fb) — vroeg, vóór er goroutines loggen.
	// QEMU heeft geen firmware-framebuffer, dus we bewijzen de renderer op een
	// RAM-buffer; op een board voedt board.Framebuffer() (GOP/simplefb) 'm.
	if err := fbconsDemo(); err != nil {
		fail("fbcons", err)
	}
	fmt.Println("HOPOS_FBCONS_OK — universele fb-log-console: renderer + printk-mirror bewezen")

	if err := hopnet.Up(); err != nil {
		fail("net", err)
	}
	// De interne L2-switch (per-slot netwerk) — vóór de eerste slots.Start.
	if err := hopswitch.Up(); err != nil {
		fail("switch", err)
	}
	if err := serveHello(); err != nil {
		fail("http", err)
	}

	// Klok via SNTP — zonder RTC begint alles op 1970; TLS eist echte tijd.
	if err := hopnet.SyncTime("pool.ntp.org:123"); err != nil {
		fail("sntp", err)
	}
	if time.Now().Year() < 2026 {
		fail("sntp", fmt.Errorf("klok nog niet gezet: %s", time.Now()))
	}
	fmt.Printf("HOPOS_CLOCK_OK — klok via SNTP: %s\n", time.Now().UTC().Format(time.RFC3339))

	disk, err := nvmeDemo()
	if err != nil {
		fail("nvme", err)
	}
	fmt.Println("HOPOS_NVME_OK — eigen PCIe-ECAM + NVMe-driver: blok geschreven en teruggelezen")

	// De storage-laag van deze node: hopfs op de NVMe. Vanaf hier kunnen
	// tasks volumes mounten en via de hop-ABI bij hun bestanden.
	fsys := hopfs.New(disk)
	slots.UseFS(fsys)

	// Drie apps, drie cores — met verschillende MemoryLimits uit het
	// "manifest": bewijs dat HOP de RAM-declaratie per start bepaalt.
	apps := []struct {
		slot  int
		limit uint64
		env   map[string]string
	}{
		{1, 400 << 20, map[string]string{"BUCKET": "hop-apps", "ROLE": "worker"}}, // >128MB: bewijst de ruimere slots
		{2, 64 << 20, map[string]string{"BUCKET": "hop-cache"}},
		{3, 256 << 20, map[string]string{"BUCKET": "hop-db", "ROLE": "reader"}},
	}

	logCounts := make([]int, len(apps)+2)
	for _, a := range apps {
		if err := slots.Start(a.slot, app, a.limit, 1, a.env, nil, nil); err != nil {
			fail("start", err)
		}
		go drainLogs(a.slot, &logCounts[a.slot])
	}
	for _, a := range apps {
		if err := slots.WaitReady(a.slot, 5*time.Second); err != nil {
			fail("ready", err)
		}
	}

	// Heartbeats en ring-logs laten lopen, dan status tonen.
	time.Sleep(900 * time.Millisecond)
	for _, a := range apps {
		if logCounts[a.slot] == 0 {
			fail("ring", fmt.Errorf("geen ring-logs van slot %d", a.slot))
		}
	}
	for _, a := range apps {
		s := slots.Get(a.slot)
		fmt.Printf("slot %d: core=on=%v app=%d hb=%d ram=%dMB (limiet was %dMB)\n",
			a.slot, s.CoreOn, s.App, s.Heartbeat, s.RAMSize>>20, a.limit>>20)
		// De app declareert partitie − net-ringstaart als RAM (slots.appRAMSize):
		// de bovenste NetRingStride is zijn net-ring, geen heap.
		if !s.CoreOn || s.App != layout.StatusReady || s.Heartbeat == 0 || s.RAMSize != a.limit-layout.NetRingStride {
			fail("status", fmt.Errorf("slot %d inconsistent", a.slot))
		}
	}

	// Kill + restart van slot 2 — de Runner.Stop/Run-cyclus.
	fmt.Println("stop slot 2 (kill-flag)...")
	if err := slots.Stop(2, 3*time.Second); err != nil {
		fail("stop", err)
	}
	s := slots.Get(2)
	fmt.Printf("slot 2 gestopt: core-on=%v app=%d exit=%d\n", s.CoreOn, s.App, s.ExitCode)

	fmt.Println("herstart slot 2 met 32MB...")
	if err := slots.Start(2, app, 32<<20, 1, map[string]string{"BUCKET": "hop-cache-v2"}, nil, nil); err != nil {
		fail("restart", err)
	}
	go drainLogs(2, nil)
	if err := slots.WaitReady(2, 5*time.Second); err != nil {
		fail("restart-ready", err)
	}
	s = slots.Get(2)
	fmt.Printf("slot 2 terug: core-on=%v app=%d ram=%dMB\n", s.CoreOn, s.App, s.RAMSize>>20)

	time.Sleep(500 * time.Millisecond) // ring-logs van de herstarte app tonen

	// Stage-2-isolatie bewijzen: een app die buiten zijn kooi grijpt wordt
	// door de MMU-grens gestopt — de EL2-vector zet de core uit zónder dat
	// de app EXITED meldt.
	fmt.Println("isolatietest: slot 2 herstart met PROBE=hop...")
	if err := slots.Stop(2, 3*time.Second); err != nil {
		fail("iso-stop", err)
	}
	if err := slots.Start(2, app, 32<<20, 1, map[string]string{"PROBE": "hop"}, nil, nil); err != nil {
		fail("iso-start", err)
	}
	go drainLogs(2, nil)
	if err := slots.WaitReady(2, 5*time.Second); err != nil {
		fail("iso-ready", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for slots.Get(2).CoreOn {
		if time.Now().After(deadline) {
			fail("isolatie", fmt.Errorf("app leest HOP-geheugen zonder fault"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	s = slots.Get(2)
	if s.App == layout.StatusExited {
		fail("isolatie", fmt.Errorf("app exitte netjes (%d) — fault verwacht", s.ExitCode))
	}
	// Fault-rapportage: de EL2-vector hoort syndroom en adres van de
	// gestrande greep op de ctrl-page te hebben gezet.
	fmt.Printf("fault-rapport slot 2: vec=%d esr=%#x far=%#x\n", s.FaultVec, s.FaultESR, s.FaultFAR)
	if s.FaultVec != layout.FaultSync || s.FaultFAR != layout.HopRAMStart {
		fail("faultinfo", fmt.Errorf("verwacht vec=%d far=%#x", layout.FaultSync, uint64(layout.HopRAMStart)))
	}
	time.Sleep(200 * time.Millisecond) // laatste ring-logs tonen
	fmt.Println("HOPOS_ISOLATIE_OK — stage-2-kooi hard bewezen: core off buiten eigen slot")

	// SMC-kooi: een app die met de firmware wil praten wordt door HCR_EL2.TSC
	// op EL2 getrapt — zelfde vangnet als de geheugen-kooi, ander luik. Er
	// bestaat geen legitieme app-SMC (SMP-bring-up loopt via HOP, exit is een
	// HVC), dus het ESR hoort EC=0x17 (trapped SMC64) te dragen.
	fmt.Println("isolatietest: slot 2 start met PROBE=smc...")
	if err := slots.Start(2, app, 32<<20, 1, map[string]string{"PROBE": "smc"}, nil, nil); err != nil {
		fail("smc-start", err)
	}
	go drainLogs(2, nil)
	if err := slots.WaitReady(2, 5*time.Second); err != nil {
		fail("smc-ready", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for slots.Get(2).CoreOn {
		if time.Now().After(deadline) {
			fail("smc-kooi", fmt.Errorf("app deed een SMC zonder fault"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	s = slots.Get(2)
	if s.App == layout.StatusExited {
		fail("smc-kooi", fmt.Errorf("app exitte netjes (%d) — fault verwacht", s.ExitCode))
	}
	fmt.Printf("fault-rapport slot 2: vec=%d esr=%#x (EC=%#x)\n", s.FaultVec, s.FaultESR, (s.FaultESR>>26)&0x3F)
	if s.FaultVec != layout.FaultSync || (s.FaultESR>>26)&0x3F != 0x17 {
		fail("smc-faultinfo", fmt.Errorf("verwacht vec=%d EC=0x17, kreeg vec=%d EC=%#x", layout.FaultSync, s.FaultVec, (s.FaultESR>>26)&0x3F))
	}
	fmt.Println("HOPOS_SMCKOOI_OK — SMC uit de kooi getrapt: een app praat nooit met de firmware")

	// Hard-kill: een app die in een lus zonder preemptiepunt hangt negeert
	// de kill-flag; slots.Stop escaleert dan naar stage2.Revoke, die de
	// stage-2-map van het slot intrekt (HVC→TLBI) zodat de core op zijn
	// eerstvolgende fetch naar de EL2-vectoren faultt en zichzelf uitzet.
	// HANG=spin is een `for {}` (self-branch, géén geheugentoegang): meteen het
	// pathologische geval — vangt de intrekking dít, dan vangt hij alles.
	fmt.Println("hard-kill: slot 2 start met HANG=spin...")
	if err := slots.Start(2, app, 32<<20, 1, map[string]string{"HANG": "spin"}, nil, nil); err != nil {
		fail("hang-start", err)
	}
	go drainLogs(2, nil)
	if err := slots.WaitReady(2, 5*time.Second); err != nil {
		fail("hang-ready", err)
	}
	time.Sleep(300 * time.Millisecond) // laat hem echt hangen
	if err := slots.Stop(2, time.Second); err != nil {
		fail("hard-kill", err)
	}
	s = slots.Get(2)
	fmt.Printf("hard-kill-rapport slot 2: vec=%d (verwacht %d=stage-2-fault)\n", s.FaultVec, layout.FaultSync)
	if s.App == layout.StatusExited {
		fail("hard-kill", fmt.Errorf("app exitte netjes — hij hoorde te hangen"))
	}
	if s.FaultVec != layout.FaultSync {
		fail("hard-kill", fmt.Errorf("vec=%d, verwacht %d (stage-2-fault)", s.FaultVec, layout.FaultSync))
	}
	fmt.Println("HOPOS_HARDKILL_OK — hangende app via stage-2-intrekking van zijn core gezet")

	// KILL → HERSTART op DEZELFDE core: de kern van het parkeer-model. De
	// zojuist gevelde core (slot 2) is niet teruggegeven aan de firmware maar
	// geparkeerd op EL2; een verse app moet er meteen op kunnen draaien —
	// zónder PSCI CPU_ON (die core is nooit uitgezet). Bewijs: nieuwe app boot,
	// wordt READY, logt en heeft een lopende heartbeat op precies die core.
	fmt.Println("herstart-na-kill: verse app op de zojuist gevelde core (slot 2)...")
	var rekLogs int
	if err := slots.Start(2, app, 48<<20, 1, map[string]string{"ROLE": "na-kill"}, nil, nil); err != nil {
		fail("rekill-start", err)
	}
	go drainLogs(2, &rekLogs)
	if err := slots.WaitReady(2, 5*time.Second); err != nil {
		fail("rekill-ready", err)
	}
	time.Sleep(600 * time.Millisecond)
	s = slots.Get(2)
	fmt.Printf("herstart slot 2: core-on=%v app=%d hb=%d ram=%dMB logs=%d vec=%d\n",
		s.CoreOn, s.App, s.Heartbeat, s.RAMSize>>20, rekLogs, s.FaultVec)
	if !s.CoreOn || s.App != layout.StatusReady || s.Heartbeat == 0 || s.RAMSize != 48<<20-layout.NetRingStride || rekLogs == 0 {
		fail("rekill", fmt.Errorf("verse app kwam niet op na een hard-kill (geparkeerde core niet herbruikbaar?)"))
	}
	if s.FaultVec != layout.FaultNone {
		fail("rekill", fmt.Errorf("verse app erfde een fault-rapport (vec=%d) — control-page niet vers", s.FaultVec))
	}
	if err := slots.Stop(2, 3*time.Second); err != nil {
		fail("rekill-stop", err)
	}
	fmt.Println("HOPOS_REKILL_OK — geparkeerde core herbruikt: kill → verse app op dezelfde core, geen firmware-roundtrip")

	// Volumes-demo (het storage-model, PLAN.md §3): een writer-app zet de
	// dataset in het gemounte /data (op de NVMe), twee reader-apps met
	// dezelfde mount lezen hem parallel en melden hun checksum als exitcode;
	// een app zónder mount ziet /data helemaal niet, en andermans eigen root
	// en '..'-escapes zijn dicht. Nul gedeeld geheugen — alles via HOP.
	dataMount := map[string]string{"/data": "/data"}
	for slot := 1; slot <= 2; slot++ {
		if slots.Get(slot).CoreOn {
			if err := slots.Stop(slot, 3*time.Second); err != nil {
				fail("vol-stop", err)
			}
		}
	}

	fmt.Println("volumes: writer (slot 1, mount /data) schrijft de dataset...")
	if err := slots.Start(1, app, 64<<20, 1, map[string]string{"FSDEMO": "writer"}, dataMount, nil); err != nil {
		fail("vol-writer", err)
	}
	go drainLogs(1, nil)
	if code, err := waitExit(1, 10*time.Second); err != nil || code != 0 {
		fail("vol-writer", fmt.Errorf("exit=%d, err=%v", code, err))
	}

	// HOP rekent zelf de som over hetzelfde bestand in de storage-laag.
	dbBytes := make([]byte, 100<<10)
	if n, err := fsys.ReadAt("/data/db.bin", 0, dbBytes); err != nil || n != len(dbBytes) {
		fail("vol-check", fmt.Errorf("db.bin lezen: n=%d, %v", n, err))
	}
	sum := checksum.FNV64(dbBytes)

	fmt.Println("volumes: readers (slot 1+2, mount /data) lezen parallel...")
	for slot := 1; slot <= 2; slot++ {
		if err := slots.Start(slot, app, 64<<20, 1, map[string]string{"FSDEMO": "reader"}, dataMount, nil); err != nil {
			fail("vol-reader", err)
		}
		go drainLogs(slot, nil)
	}
	for slot := 1; slot <= 2; slot++ {
		code, err := waitExit(slot, 15*time.Second)
		if err != nil {
			fail("vol-reader", err)
		}
		if code != sum {
			fail("vol-reader", fmt.Errorf("slot %d checksum %#x ≠ HOP-som %#x", slot, code, sum))
		}
		fmt.Printf("slot %d las /data/db.bin: checksum %#x = HOP-som\n", slot, sum)
	}

	fmt.Println("volumes: app zonder mount (slot 2) hoort /data niet te zien...")
	if err := slots.Start(2, app, 32<<20, 1, map[string]string{"FSDEMO": "denied"}, nil, nil); err != nil {
		fail("vol-denied", err)
	}
	go drainLogs(2, nil)
	if code, err := waitExit(2, 10*time.Second); err != nil || code != 0 {
		fail("vol-denied", fmt.Errorf("exit=%d, err=%v", code, err))
	}
	fmt.Println("HOPOS_VOLUMES_OK — volumes: gedeeld pad, eigen root, mount-grens afgedwongen")

	// Fetch-demo: de app vraagt HOP een URL naar /data te downloaden (de
	// bulk gaat buiten de ring om). Doelwit: HOP's eigen hello-server —
	// zelfstandig en deterministisch (HOP mag als vertrouwde kern elk adres
	// bereiken; zie fetchClient in slots/rpc.go).
	fmt.Println("fetch: slot 1 laat HOP een URL naar /data/hello.txt halen...")
	fetchEnv := map[string]string{"FSDEMO": "fetch", "FETCH_URL": "http://" + board.Current().Net().IP + "/"}
	if err := slots.Start(1, app, 64<<20, 1, fetchEnv, dataMount, nil); err != nil {
		fail("fetch", err)
	}
	go drainLogs(1, nil)
	if code, err := waitExit(1, 15*time.Second); err != nil || code != 0 {
		fail("fetch", fmt.Errorf("exit=%d, err=%v", code, err))
	}
	hello := make([]byte, 256)
	n, err := fsys.ReadAt("/data/hello.txt", 0, hello)
	if err != nil || !bytes.Contains(hello[:n], []byte("HopOS leeft")) {
		fail("fetch", fmt.Errorf("hello.txt onverwacht (n=%d, %v): %q", n, err, hello[:n]))
	}
	fmt.Println("HOPOS_FETCH_OK — fetch via HOP: download landde in het volume")

	// Per-slot netwerk: elke app een eigen netstack over de frame-ringen,
	// HOP schuift alleen frames (metal/net/hopswitch). Bewijs: app → app zonder
	// dat er een TCP-stack op core 0 aan te pas komt (HOP is enkel L2-switch +
	// ARP-responder voor de gateway).
	fmt.Println("netdemo: slot 1 luistert op het interne net...")
	if err := slots.Start(1, app, 64<<20, 1, map[string]string{"NETDEMO": "listen"}, nil, nil); err != nil {
		fail("net-listen", err)
	}
	go drainLogs(1, nil)
	if err := slots.WaitReady(1, 5*time.Second); err != nil {
		fail("net-listen", err)
	}

	// App → app: slot 2 dialt slot 1 (via de gateway-ARP van de switch); exit
	// 0 = pong geverifieerd.
	addr := hopswitch.SlotIP(1) + ":8080"
	fmt.Println("netdemo: slot 2 dialt slot 1 — app↔app, HOP kopieert alleen frames...")
	dialEnv := map[string]string{"NETDEMO": "dial", "NET_DIAL": addr}
	if err := slots.Start(2, app, 64<<20, 1, dialEnv, nil, nil); err != nil {
		fail("net-dial", err)
	}
	go drainLogs(2, nil)
	if code, err := waitExit(2, 15*time.Second); err != nil || code != 0 {
		fail("net-dial", fmt.Errorf("exit=%d, err=%v", code, err))
	}
	if err := slots.Stop(1, 3*time.Second); err != nil {
		fail("net-stop", err)
	}
	fmt.Println("HOPOS_NET_SLOT_OK — per-slot netstack + L2-switch: app↔app bewezen")

	// Uitgaand (masquerade): een app dialt naar buiten — hier een DNS-query
	// (UDP) naar de node-resolver. HOP herschrijft bron slot-IP:poort →
	// node-IP:node-poort en het antwoord terug; nul TCP-terminatie op core 0.
	// Dit is het pad voor cloudflared/servers. (De query verlaat QEMU via
	// slirp; een antwoord bewijst de round-trip.)
	fmt.Println("netdemo: slot 2 doet een uitgaande DNS-query — masquerade naar buiten...")
	if err := slots.Start(2, app, 64<<20, 1, map[string]string{"NETDEMO": "out"}, nil, nil); err != nil {
		fail("net-out", err)
	}
	go drainLogs(2, nil)
	if code, err := waitExit(2, 15*time.Second); err != nil || code != 0 {
		fail("net-out", fmt.Errorf("exit=%d, err=%v", code, err))
	}
	fmt.Println("HOPOS_OUTBOUND_OK — uitgaande masquerade: app → buiten → app, HOP herschrijft alleen headers")

	// Poort-publicatie (stateloze DNAT): node-IP:8080 → slot 1, via de
	// ports-tabel van Start — exact de HopRunner-route, inclusief de
	// ER_PORT_*-conventie (app bindt het nummer dat HOP hem gaf). Het bewijs
	// komt per definitie van búíten QEMU: nc via de hostfwd in
	// qemu-run.sh (host :18080 → 10.0.2.15:8080 → DNAT → slot 1).
	// De listener blijft draaien; main slaapt hierna toch voor eeuwig.
	portsEnv := map[string]string{"NETDEMO": "listen", "ER_PORT_HTTP": "8080"}
	if err := slots.Start(1, app, 64<<20, 1, portsEnv, nil, map[string]int{"http": 8080}); err != nil {
		fail("ports", err)
	}
	go drainLogs(1, nil)
	if err := slots.WaitReady(1, 5*time.Second); err != nil {
		fail("ports", err)
	}
	fmt.Println("HOPOS_PORTS_READY — tcp/8080 doorgerouterd naar slot 1 (test extern: nc host:18080)")

	// Self-relocating spike (PLAN §4.4): één artifact voor elk slot — de
	// stage-2-map ís de relocatie (canoniek linkadres → eigen partitie, de
	// MMU vertaalt). De app is gelinkt voor het slot-1-bereik en draait hier
	// op slot 2 én 3 tegelijk; beide loggen RAM @ 0x50000000 (hun virtuele
	// beeld) terwijl ze fysiek in eigen partities leven.
	fmt.Println("reloc: zelfde artifact (slot-1-gelinkt) naar slot 2 en 3...")
	if slots.Get(3).CoreOn {
		if err := slots.Stop(3, 3*time.Second); err != nil {
			fail("reloc-stop", err)
		}
	}
	for slot := 2; slot <= 3; slot++ {
		if err := slots.Start(slot, app, 64<<20, 1, map[string]string{"ROLE": "reloc"}, nil, nil); err != nil {
			fail("reloc", err)
		}
		go drainLogs(slot, nil)
	}
	for slot := 2; slot <= 3; slot++ {
		if err := slots.WaitReady(slot, 5*time.Second); err != nil {
			fail("reloc-ready", err)
		}
	}
	time.Sleep(600 * time.Millisecond) // ring-logs + heartbeats laten lopen
	for slot := 2; slot <= 3; slot++ {
		s := slots.Get(slot)
		if !s.CoreOn || s.Heartbeat == 0 || s.RAMSize != 64<<20-layout.NetRingStride {
			fail("reloc-status", fmt.Errorf("slot %d: on=%v hb=%d ram=%dMB", slot, s.CoreOn, s.Heartbeat, s.RAMSize>>20))
		}
	}
	fmt.Println("HOPOS_RELOC_OK — zelfde artifact draait op slot 2 én 3: stage-2 is de relocatie")

	// SMP (fase 5): één app over meerdere cores met een gedeelde heap. HOP geeft
	// cores + memory (hier cores=2, zoals HOP's CPUShares/1024); de app krijgt
	// twee cores "as is" en parallelt via GOMAXPROCS — app-code doet er niets
	// voor. cores=2 gebruikt slot 1 (primair) + core 2 (secundair), dus eerst
	// 1-3 vrijmaken.
	fmt.Println("smp: slot 1 als 2-core app (gedeelde heap), core 2 secundair...")
	for slot := 1; slot <= 3; slot++ {
		if slots.Get(slot).CoreOn {
			if err := slots.Stop(slot, 3*time.Second); err != nil {
				fail("smp-stop", err)
			}
		}
	}
	if err := slots.Start(1, app, 128<<20, 2, map[string]string{"SMP": "bench"}, nil, nil); err != nil {
		fail("smp-start", err)
	}
	go drainLogs(1, nil)
	code, err := waitExit(1, 30*time.Second)
	if err != nil || code != 0 {
		fail("smp", fmt.Errorf("exit=%d, err=%v", code, err))
	}
	// Teardown: alle cores van de SMP-app moeten afgaan (primair + secundair).
	// CoreIdle toetst de CORE-mailbox (Get.CoreOn is sinds de core-deling de
	// slot-staat, en "core 2" is hier een core, geen slot).
	if err := slots.Stop(1, 5*time.Second); err != nil {
		fail("smp-teardown", err)
	}
	for _, c := range []int{1, 2} {
		if !slots.CoreIdle(c) {
			fail("smp-teardown", fmt.Errorf("core %d nog aan na teardown", c))
		}
	}
	fmt.Println("HOPOS_SMP_OK — één app op twee cores, gedeelde heap: rendezvous + GC bewezen, cores netjes afgebroken")

	// Core-deling (fase 6): twee apps op ÉÉN fysieke core, elk met eigen
	// kooi/partitie/netstack. De EL2-switch (cpu/el2/switch.s) wisselt op de
	// WFE-yields van de idle-governor; er is geen timer en geen GIC. Slot 2
	// en 3 delen core 2; core 3 blijft de hele test leeg — het bewijs dat de
	// tweede app er niet stiekem heen lekt.
	fmt.Println("share: slot 2 en 3 samen op core 2 (eigen kooi, gedeelde core)...")
	if err := slots.Start(2, app, 64<<20, 1, map[string]string{"ROLE": "share-a"}, nil, nil); err != nil {
		fail("share-a", err)
	}
	go drainLogs(2, nil)
	if err := slots.WaitReady(2, 5*time.Second); err != nil {
		fail("share-a-ready", err)
	}
	if err := slots.StartShared(2, 3, app, 64<<20, map[string]string{"ROLE": "share-b"}, nil, nil); err != nil {
		fail("share-b", err)
	}
	go drainLogs(3, nil)
	if err := slots.WaitReady(3, 5*time.Second); err != nil {
		fail("share-b-ready", err)
	}
	if !slots.CoreIdle(3) {
		fail("share-leak", fmt.Errorf("core 3 draait terwijl slot 3 op core 2 hoort te wonen"))
	}
	// Beide heartbeats moeten LOPEN (niet slechts bestaan): twee metingen.
	hb2, hb3 := slots.Get(2).Heartbeat, slots.Get(3).Heartbeat
	time.Sleep(1200 * time.Millisecond)
	s2, s3 := slots.Get(2), slots.Get(3)
	fmt.Printf("share: slot 2 hb %d→%d, slot 3 hb %d→%d — één core, twee kooien\n",
		hb2, s2.Heartbeat, hb3, s3.Heartbeat)
	if !s2.CoreOn || !s3.CoreOn || s2.Heartbeat <= hb2 || s3.Heartbeat <= hb3 {
		fail("share-hb", fmt.Errorf("heartbeats lopen niet door op de gedeelde core (slot2 %d→%d, slot3 %d→%d)",
			hb2, s2.Heartbeat, hb3, s3.Heartbeat))
	}
	// Kill één bewoner; de ander moet blijven kloppen (kill-per-context, geen
	// CPU_OFF van de gedeelde core).
	fmt.Println("share: stop slot 3 — slot 2 moet blijven draaien...")
	if err := slots.Stop(3, 3*time.Second); err != nil {
		fail("share-stop", err)
	}
	if slots.Get(3).CoreOn {
		fail("share-stop", fmt.Errorf("slot 3 meldt zich nog levend na Stop"))
	}
	hb2 = slots.Get(2).Heartbeat
	time.Sleep(800 * time.Millisecond)
	if s := slots.Get(2); !s.CoreOn || s.Heartbeat <= hb2 {
		fail("share-survivor", fmt.Errorf("slot 2 stierf mee met zijn buurman (hb %d→%d)", hb2, s.Heartbeat))
	}
	// En er past een verse bewoner naast (herbezetting van het gedeelde slot).
	fmt.Println("share: verse app als nieuwe mede-bewoner op core 2...")
	if err := slots.StartShared(2, 3, app, 48<<20, map[string]string{"ROLE": "share-c"}, nil, nil); err != nil {
		fail("share-c", err)
	}
	go drainLogs(3, nil)
	if err := slots.WaitReady(3, 5*time.Second); err != nil {
		fail("share-c-ready", err)
	}
	// Teardown in twee stappen: eerst de mede-bewoner (gedeeld pad), dan de
	// laatste (klassiek pad: de core parkeert).
	if err := slots.Stop(3, 3*time.Second); err != nil {
		fail("share-teardown", err)
	}
	if err := slots.Stop(2, 3*time.Second); err != nil {
		fail("share-teardown", err)
	}
	if !slots.CoreIdle(2) {
		fail("share-teardown", fmt.Errorf("core 2 parkeerde niet na het vertrek van de laatste bewoner"))
	}
	fmt.Println("HOPOS_SHARE_OK — twee kooien om één core: yield-switch, kill-één-houd-ander, herbezetting")

	// Sharegroup-pool (fase 6): meerdere kooien op een POOL van hele cores. Vier
	// apps in sharegroup "web", pool = 2 cores → 2 apps per core, elk een eigen
	// kooi. Dit is precies wat slotmgr voor de agent doet (PlaceCage kiest de
	// core, StartShared plaatst); hier direct met de embedded app zodat de demo
	// self-contained blijft. Kooi 4 ligt bóven de 3 app-cores — bewijst kooi≠core.
	fmt.Println("sharegroup: 4 apps in pool 'web' (2 hele cores), kooien 1-4...")
	for slot := 1; slot <= 3; slot++ {
		if slots.Get(slot).CoreOn {
			if err := slots.Stop(slot, 3*time.Second); err != nil {
				fail("sg-stop", err)
			}
		}
	}
	sgCages := []int{1, 2, 3, 4}
	sgCores := map[int]bool{}
	for _, cage := range sgCages {
		core, err := slots.PlaceCage(cage, "web", 2)
		if err != nil {
			fail("sg-place", err)
		}
		sgCores[core] = true
		if err := slots.StartShared(core, cage, app, 64<<20, map[string]string{"ROLE": "web"}, nil, nil); err != nil {
			fail("sg-start", err)
		}
		go drainLogs(cage, nil)
	}
	for _, cage := range sgCages {
		if err := slots.WaitReady(cage, 8*time.Second); err != nil {
			fail("sg-ready", err)
		}
	}
	if len(sgCores) != 2 {
		fail("sg-cores", fmt.Errorf("pool 'web' gebruikte %d cores, wil 2", len(sgCores)))
	}
	hbs := map[int]uint64{}
	for _, cage := range sgCages {
		hbs[cage] = slots.Get(cage).Heartbeat
	}
	time.Sleep(1500 * time.Millisecond)
	for _, cage := range sgCages {
		s := slots.Get(cage)
		if !s.CoreOn || s.Heartbeat <= hbs[cage] {
			fail("sg-hb", fmt.Errorf("kooi %d loopt niet door (hb %d→%d)", cage, hbs[cage], s.Heartbeat))
		}
	}
	fmt.Printf("sharegroup: 4 kooien op %d cores, alle heartbeats lopen door\n", len(sgCores))
	for _, cage := range sgCages {
		if err := slots.Stop(cage, 3*time.Second); err != nil {
			fail("sg-teardown", err)
		}
		slots.ReleaseCage(cage)
	}
	fmt.Println("HOPOS_SHAREGROUP_OK — pool van hele cores: 4 kooien op 2 cores, elk eigen kooi, gebalanceerd")

	// STRESS (Derek: "maak de server gek, ga helemaal los"): een ZWERM van 24
	// kooien in één sharegroup op een pool van 3 hele cores, met een kill-storm
	// ertussen. Bewijst de dichtheid — RAM is de muur, niet cores — en dat de
	// rotatie/boot-pending een boot-storm op gedeelde cores aankan. Elke kooi is
	// een eigen stage-2-kooi (48MB); 24 × 48 ≈ 1,15GB past in de QEMU-pool.
	const swarmN = 24
	fmt.Printf("swarm: %d kooien in pool 'swarm' op 3 cores stapelen...\n", swarmN)
	for cage := 1; cage <= swarmN; cage++ {
		if slots.Get(cage).CoreOn {
			if err := slots.Stop(cage, 3*time.Second); err != nil {
				fail("swarm-clear", err)
			}
			slots.ReleaseCage(cage)
		}
	}
	swarmCores := map[int]bool{}
	for cage := 1; cage <= swarmN; cage++ {
		core, err := slots.PlaceCage(cage, "swarm", 3)
		if err != nil {
			fail("swarm-place", fmt.Errorf("kooi %d: %w", cage, err))
		}
		swarmCores[core] = true
		if err := slots.StartShared(core, cage, app, 48<<20, map[string]string{"ROLE": "swarm"}, nil, nil); err != nil {
			fail("swarm-start", fmt.Errorf("kooi %d: %w", cage, err))
		}
		go drainLogs(cage, nil)
	}
	for cage := 1; cage <= swarmN; cage++ {
		if err := slots.WaitReady(cage, 20*time.Second); err != nil {
			fail("swarm-ready", fmt.Errorf("kooi %d: %w", cage, err))
		}
	}
	if len(swarmCores) != 3 {
		fail("swarm-cores", fmt.Errorf("zwerm gebruikte %d cores, wil 3", len(swarmCores)))
	}
	fmt.Printf("swarm: %d kooien LEVEN op %d cores (elk een eigen stage-2-kooi)\n", swarmN, len(swarmCores))

	// Alle heartbeats moeten doorlopen — ruim window, want 24 apps delen 3 cores.
	hb := make([]uint64, swarmN+1)
	for cage := 1; cage <= swarmN; cage++ {
		hb[cage] = slots.Get(cage).Heartbeat
	}
	time.Sleep(3 * time.Second)
	stuck := 0
	for cage := 1; cage <= swarmN; cage++ {
		if slots.Get(cage).Heartbeat <= hb[cage] {
			stuck++
		}
	}
	if stuck > 0 {
		fail("swarm-hb", fmt.Errorf("%d/%d kooien met stilstaande heartbeat", stuck, swarmN))
	}
	fmt.Printf("swarm: alle %d heartbeats lopen door\n", swarmN)

	// KILL-STORM: sloop de even kooien terwijl de oneven doordraaien.
	fmt.Println("swarm: kill-storm — 12 even kooien weg, oneven moeten doordraaien...")
	for cage := 2; cage <= swarmN; cage += 2 {
		if err := slots.Stop(cage, 3*time.Second); err != nil {
			fail("swarm-kill", fmt.Errorf("kooi %d: %w", cage, err))
		}
		slots.ReleaseCage(cage)
	}
	for cage := 1; cage <= swarmN; cage += 2 {
		hb[cage] = slots.Get(cage).Heartbeat
	}
	time.Sleep(2 * time.Second)
	for cage := 1; cage <= swarmN; cage += 2 {
		s := slots.Get(cage)
		if !s.CoreOn || s.Heartbeat <= hb[cage] {
			fail("swarm-survive", fmt.Errorf("oneven kooi %d stierf mee met de storm (hb %d→%d)", cage, hb[cage], s.Heartbeat))
		}
	}
	fmt.Println("swarm: 12 overlevenden draaien ongestoord door na de kill-storm")

	// Herbevolk de pool: de 12 even kooien terug (joinen de bestaande pool).
	for cage := 2; cage <= swarmN; cage += 2 {
		core, err := slots.PlaceCage(cage, "swarm", 3)
		if err != nil {
			fail("swarm-replace", fmt.Errorf("kooi %d: %w", cage, err))
		}
		if err := slots.StartShared(core, cage, app, 48<<20, map[string]string{"ROLE": "swarm2"}, nil, nil); err != nil {
			fail("swarm-replace", fmt.Errorf("kooi %d: %w", cage, err))
		}
		go drainLogs(cage, nil)
	}
	for cage := 2; cage <= swarmN; cage += 2 {
		if err := slots.WaitReady(cage, 20*time.Second); err != nil {
			fail("swarm-replace-ready", fmt.Errorf("kooi %d: %w", cage, err))
		}
	}
	fmt.Printf("swarm: alle %d kooien terug na de churn\n", swarmN)

	// Opruimen.
	for cage := 1; cage <= swarmN; cage++ {
		if err := slots.Stop(cage, 3*time.Second); err != nil {
			fail("swarm-teardown", err)
		}
		slots.ReleaseCage(cage)
	}
	fmt.Printf("HOPOS_SWARM_OK — %d kooien op 3 cores + kill-storm: dichtheid bewezen, RAM is de muur\n", swarmN)

	fmt.Println("HOPOS_SLOTS_OK — slots + MemoryLimit + hop-ABI-logring werken")

	for {
		time.Sleep(time.Hour)
	}
}
