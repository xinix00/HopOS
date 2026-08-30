// probeapple is het meetinstrument van de Apple-silicon-bring-up (Mac mini M4,
// t8132, geboot via m1n1 — zie board/apple). Zelfde rol als proberk3566: de
// REFERENTIE-aannames van het board omzetten in gemeten feiten vóór er één
// regel agent-code voor dit silicium geschreven wordt.
//
// Wat hij meet, in volgorde van "sterft stil" naar "inventaris":
//
//  1. dat er output komt → de tamago-runtime draait op RAM boven 512GB (de
//     L0-tabel van de tamago-fork) én de UART-route naar de laptop klopt;
//  2. het boot-EL en de effectieve HCR_EL2 → EL2 (kooi mogelijk) en of dit
//     silicium E2H kan wissen (nVHE zoals elk ander board) of VHE-only is
//     (dan moet cpu/el2 de _EL12-encoderingen leren);
//  3. MPIDR/CoreID, ID-registers (PARange, VH, E2H0), TCR (48-bit-pad);
//  4. het param-blok van de loader: UARTs, ADT, DRAM, framebuffer, cores;
//  5. de FDT via x0 (m1n1 geeft er geen zonder DT-prep — verwacht: geen);
//  6. geheugen: een schrijf/lees-test aan de top van het venster (Normal) en
//     op een device-gemapt woord (WakeBase);
//  7. de klok: CNTFRQ en een gemeten slaap van 100ms;
//  8. de cores: elke door m1n1 geparkeerde core loslaten (spin-table) naar de
//     parkeerlus die zijn MPIDR neerlegt — het kooi-fundament zonder PSCI;
//  9. een 1Hz-heartbeat → CNTFRQ klopt en de node blijft leven.
//
// Bouwen/laden: image/apple-m4.sh, dan image/apple/load-probe.py.
package main

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"strings"
	"time"
	_ "unsafe" // voor go:linkname

	"github.com/usbarmory/tamago/arm64"

	"github.com/xinix00/HopOS/metal/board/apple"
	applehop "github.com/xinix00/HopOS/metal/board/apple/hop"
	"github.com/xinix00/HopOS/metal/cpu/idle"
	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/driver/nic/tg3"
	"github.com/xinix00/HopOS/metal/driver/nvme"
	"github.com/xinix00/HopOS/metal/driver/pcie"
	"github.com/xinix00/HopOS/metal/driver/rtkit"
	"github.com/xinix00/HopOS/metal/fw/adt"
)

// PCIe op de M4 (ADT /arm-io/apcie, 29-08): ECAM op 0x1cb0000000 (256MB, bus
// 0..8), drie rootpoorten als bridges op bus 0 — poort 0 = wlan/bt (bcm4387),
// poort 2 = "lan-1gb" (de Broadcom 57762). m1n1's kboot_boot heeft de poorten
// gereset en de links getraind, maar wijst geen busnummers of BARs toe; dat
// doet de meting hier minimaal (busnummers) om het endpoint te kunnen zien.
// De DMA van de NIC loopt door dart-apcie2 (dart,t8110): TCR per stream op
// +0x1000 + sid*4 (bit0 translate, bit1 bypass-DART, bit2 bypass-DAPF).
const (
	pcieECAM   = 0x1cb0000000
	dartAPCIE2 = 0x492000000
	dartTCR    = 0x1000
	dartTTBR   = 0x1400
	dartError  = 0x100
)

// pcieCfg is het ECAM-adres van een config-register (bus/dev/fn/offset).
func pcieCfg(bus, devno, fn int, off uintptr) uintptr {
	return uintptr(pcieECAM) + uintptr(bus)<<20 + uintptr(devno)<<15 + uintptr(fn)<<12 + off
}

// probePCIe: bus 0 scannen, elke bridge een eigen secundaire bus geven en
// daarachter kijken; de NIC met BAR-inhoud en command-register melden. Alleen
// busnummers worden geschreven — geen BARs, geen enable.
func probePCIe() {
	// De controller zelf opbrengen als niemand dat deed. Onder m1n1 is dit een
	// no-op en zegt de melding dat ook; zonder m1n1 is het de hele bring-up.
	// Een exceptie hier is de normale uitkomst van bring-up op ongedocumenteerd
	// silicium. Zonder ESR/FAR is hij "hij viel om"; mét is hij een adres en een
	// reden, en dat scheelt per stuk een bootcyclus.
	arm64.SystemExceptionHandler = func(pc uintptr) {
		say("\n!!EL1 ESR=%#016x FAR=%#016x PC=%#x\n", apple.ReadESR(), apple.ReadFAR(), pc)
		arm64.DefaultExceptionHandler(pc)
	}
	apple.PCIeLog = func(s string) { say("%s\n", s) }
	if err := apple.InitPCIe(); err != nil {
		say("\n%v\n", err)
	} else {
		say("\n%s\n", apple.PCIeUp())
	}
	say("pcie: scanning ECAM %#x bus 0 (root ports)\n", uintptr(pcieECAM))
	roots := pcie.Scan(pcie.Window{ECAMBase: uintptr(pcieECAM)})
	if len(roots) == 0 {
		say("pcie: nothing on bus 0 — ECAM unreachable or ports not initialized by m1n1\n")
		return
	}
	next := 1
	for _, r := range roots {
		say("pcie: %s hdr=%d\n", r, r.HdrType)
		if !r.IsBridge() {
			continue
		}
		// Type-1-header: primary 0, secondary/subordinate = eigen bus. Terug
		// lezen, want een write die niet landt is de eerste verdachte als er
		// niets achter de brug verschijnt.
		bus := next
		next++
		reg := pcieCfg(0, r.Dev, r.Fn, 0x18)
		dev.Write32(reg, dev.Read32(reg)&^0x00ffffff|uint32(bus)<<8|uint32(bus)<<16)
		dev.MB()
		say("pcie:   bus numbers %#x (want sec=%d sub=%d)\n", dev.Read32(reg), bus, bus)

		// Bridge control (0x3e, bovenste helft van 0x3c): bit 6 = secondary
		// bus reset. Staat die aan, dan houdt de brug het endpoint in reset en
		// antwoordt niemand — uitzetten en de PCIe-hersteltijd afwachten.
		bctl := pcieCfg(0, r.Dev, r.Fn, 0x3c)
		if v := dev.Read32(bctl); v&(1<<22) != 0 {
			say("pcie:   secondary bus reset was SET (%#x) — clearing\n", v)
			dev.Write32(bctl, v&^(1<<22))
			dev.MB()
			time.Sleep(200 * time.Millisecond)
		}
		if lnk, ok := pcieLinkStatus(0, r.Dev, r.Fn); ok {
			say("pcie:   link: speed gen%d width x%d active=%d (LNKSTA %#x)\n", lnk&0xF, lnk>>4&0x3F, lnk>>13&1, lnk)
		}
		// De hele secundaire bus aflopen: een endpoint hoeft niet op device 0
		// te zitten, en fn>0 bestaat (de wlan-brug draagt wifi én bluetooth).
		found := 0
		for devno := 0; devno < 32; devno++ {
			for fn := 0; fn < 8; fn++ {
				id := dev.Read32(pcieCfg(bus, devno, fn, 0))
				if id == 0xffffffff || id&0xffff == 0 {
					if fn == 0 {
						break
					}
					continue
				}
				found++
				class := dev.Read32(pcieCfg(bus, devno, fn, 8)) >> 8
				cmd := dev.Read32(pcieCfg(bus, devno, fn, 4))
				say("pcie:   %02x:%02x.%x %04x:%04x class %06x cmd/status %#x\n", bus, devno, fn, id&0xffff, id>>16, class, cmd)
				for b := 0; b < 6; b++ {
					if bar := dev.Read32(pcieCfg(bus, devno, fn, 0x10+uintptr(b)*4)); bar != 0 {
						say("pcie:     BAR%d %#x\n", b, bar)
					}
				}
				if id&0xffff == 0x14e4 {
					say("pcie:   ^ Broadcom (tg3 family) reachable behind root port %02x\n", r.Dev)
				}
			}
		}
		if found == 0 {
			say("pcie:   nothing answers on bus %d (link is up but config reads return all-ones)\n", bus)
		}
	}
	// De link zelf opbrengen (PERST via GPIO + LTSSM), de DART in bypass en de
	// BAR's toewijzen — wat m1n1 op t8132 niet doet. Gemeten via de proxy op
	// 29-08; dit is dezelfde volgorde, nu vanuit het board.
	if nic, err := applehop.EnumerateNIC(); err != nil {
		say("apcie: %v\n", err)
	} else {
		nicDev := nic
		say("apcie: link up, endpoint %s\n", nic)
		bar0 := nic.BAR(0)
		say("apcie:   BAR0 %#x BAR2 %#x — registers reachable\n", bar0, nic.BAR(2))
		if bar0 != 0 {
			// tg3: het eerste registerwoord is PCI_VENDOR/DEVICE (0x00) en op
			// 0x6804 staat de chip-id in MISC_HOST_CTRL. Eén read bewijst dat
			// de BAR-toewijzing en het geheugenvenster kloppen.
			// tg3 spiegelt zijn PCI config space op de eerste 0x100 bytes van
			// BAR0; de device-registers zitten daarachter. Het MAC-adres dat
			// Apple's firmware achterliet (MAC_ADDR_0_HIGH/LOW) is de beste
			// controle dat we de juiste registerkaart lezen: het hoort
			// 1c:f6:4c:54:fa:90 te zijn, wat de mini op het net gebruikte.
			b := uintptr(bar0)
			hi, lo := dev.Read32(b+0x410), dev.Read32(b+0x414)
			say("apcie:   config-shadow %#x, chip-rev %#x\n", dev.Read32(b), dev.Read32(b+0x08)>>16)
			say("apcie:   MAC %02x:%02x:%02x:%02x:%02x:%02x (MAC_ADDR_0 %#x %#x)\n",
				hi>>8&0xff, hi&0xff, lo>>24&0xff, lo>>16&0xff, lo>>8&0xff, lo&0xff, hi, lo)
			say("apcie:   MAC_MODE %#x MAC_STATUS %#x MI_MODE %#x\n",
				dev.Read32(b+0x400), dev.Read32(b+0x404), dev.Read32(b+0x454))

			// De driver zelf: reset, MAC uit de ADT, PHY over MDIO en wachten
			// op link. Dit is de helft van tg3 die zonder ringen te bewijzen is.
			// Config-space-adres van 02:00.0 erbij: het SRAM-venster van tg3
			// werkt alleen langs die weg (zie writeMem).
			nic := applehop.NIC(nicDev)
			say("tg3: ASIC %#x, MAC from ADT %s\n", nic.ChipRev(), nic.HardwareAddr())
			if err := nic.Reset(); err != nil {
				say("tg3: %v\n", err)
			} else {
				nic.SetMAC()
				hi, lo := dev.Read32(b+0x410), dev.Read32(b+0x414)
				say("tg3: reset OK, MAC_ADDR_0 now %04x%08x\n", hi, lo)
				if id, err := nic.PHYID(); err != nil {
					say("tg3: PHY id: %v\n", err)
				} else {
					say("tg3: PHY id %#x (0/0xffffffff = no PHY on the MDIO bus)\n", id)
					if speed, fd, err := nic.LinkUp(4 * time.Second); err != nil {
						say("tg3: link: %v\n", err)
					} else {
						nic.SetPortMode(speed, fd)
						say("tg3: LINK UP — %d Mb/s %s duplex\n", speed,
							map[bool]string{true: "full", false: "half"}[fd])

						// De ringen, en dan gewoon luisteren. Een net dat
						// leeft stuurt vanzelf broadcast (ARP, mDNS): komt daar
						// binnen enkele seconden iets van binnen, dan kloppen
						// ring, DMA en filter. Eén frame de deur uit meet de
						// andere richting.
						say("tg3: selftest: %s\n", nic.SelfTest())
						if err := nic.Init(uintptr(apple.NetDMAPA), tg3.NeedBytes); err != nil {
							say("tg3: rings: %v\n", err)
						} else {
							probe := make([]byte, 60)
							for i := 0; i < 6; i++ {
								probe[i] = 0xff
							}
							copy(probe[6:], nic.HardwareAddr())
							probe[12], probe[13] = 0x08, 0x06 // ARP
							if err := nic.Transmit(probe); err != nil {
								say("tg3: transmit: %v\n", err)
							}

							frame := make([]byte, 2048)
							got, bytes := 0, 0
							deadline := time.Now().Add(5 * time.Second)
							for time.Now().Before(deadline) {
								n, err := nic.Receive(frame)
								if err != nil {
									say("tg3: receive: %v\n", err)
									break
								}
								if n == 0 {
									continue
								}
								got, bytes = got+1, bytes+n
								if got <= 3 {
									say("tg3:   RX %d bytes  dst %02x:%02x:%02x:%02x:%02x:%02x  src %02x:%02x:%02x:%02x:%02x:%02x  type %02x%02x\n",
										n, frame[0], frame[1], frame[2], frame[3], frame[4], frame[5],
										frame[6], frame[7], frame[8], frame[9], frame[10], frame[11],
										frame[12], frame[13])
								}
							}
							say("tg3: %d frames / %d bytes in 5s\n", got, bytes)
							say("tg3:   %s\n", nic.Counters())
						}
						say("tg3: stats %s\n", nic.Stats())
						if e, a := apple.DARTError(); e != 0 {
							say("dart: ERROR %#x at %#x\n", e, a)
						}
					}
				}
			}
		}
	}

	// De opslag: ANS, Apple's NVMe-coprocessor. Geen PCIe-device maar een
	// RTKit-mailbox (ASC) met een SART als adresfilter. Deze eerste meting stelt
	// één vraag: leeft het blok, en in welke staat liet iBoot het achter? Het
	// CPU_CONTROL-bit zegt of de coprocessor draait, BOOT_STATUS of zijn
	// firmware klaar is, en de mailbox-controls of er berichten klaarstaan.
	// Registerkaart: m1n1 src/asc.c en src/nvme.c.
	if ansBase, ansNVMMU, ansNVMe, ansSART := apple.ANSAddrs(); ansBase != 0 {
		const (
			cpuControl = 0x44
			cpuRunning = 0x10
			mboxA2I    = 0x8000 + 0x110
			mboxI2A    = 0x8000 + 0x114
			bootStatus = 0x1300 // NVME_BOOT_STATUS
		)
		a := uintptr(ansBase)
		say("ans: base %#x nvmmu %#x nvme %#x sart %#x (v%d)\n",
			ansBase, ansNVMMU, ansNVMe, ansSART, apple.SARTVersion())
		say("ans: CPU_CONTROL %#x (running=%d) mbox a2i %#x i2a %#x\n",
			dev.Read32(a+cpuControl), dev.Read32(a+cpuControl)>>4&1,
			dev.Read32(a+mboxA2I), dev.Read32(a+mboxI2A))
		if ansNVMe != 0 {
			say("ans: NVMe BOOT_STATUS %#x CSTS %#x CC %#x\n",
				dev.Read32(uintptr(ansNVMe)+bootStatus),
				dev.Read32(uintptr(ansNVMe)+0x1c),
				dev.Read32(uintptr(ansNVMe)+0x14))

			// De SART moet ons DMA-gebied doorlaten, anders komt de
			// coprocessor er niet bij — zonder foutmelding, want een filter
			// meldt niet wat het tegenhoudt.
			n, first, size := apple.SARTWindows()
			say("sart: %d venster(s) van de firmware, eerste %#x+%#x\n", n, first, size)
			if !apple.AllowDMA(apple.StorageDMAPA, apple.StorageDMASize) {
				say("sart: kon geen venster openen voor %#x — de queues blijven onbereikbaar\n",
					uint64(apple.StorageDMAPA))
			}

			disk := &nvme.Controller{}
			cfg := nvme.AppleConfig{
				NVMe:  uintptr(ansNVMe),
				NVMMU: uintptr(ansNVMMU),
				RTKit: &rtkit.Dev{
					Name:  "ans",
					Base:  uintptr(ansBase),
					Alloc: apple.StorageBuf,
					// Allow is nil: de hele opslag-regio staat er al in.
				},
			}
			err := disk.InitApple(cfg, apple.StorageDMAPA, apple.StorageDMASize)
			if disk.Blocks != 0 {
				say("nvme: %q — %d blokken van %d bytes (%d GB)\n", disk.Model,
					disk.Blocks, disk.BlockSize, disk.Blocks*disk.BlockSize/1e9)
			}
			if err != nil {
				say("nvme: %v\n", err)
				say("nvme: %s\n", disk.AppleDiag())
			} else {
				dumpGPT(disk)
				// En netjes teruggeven. Zonder dit staat de coprocessor er de
				// volgende boot nog in en gaat hij niet meer open.
				if err := disk.Shutdown(); err != nil {
					say("nvme: shutdown: %v\n", err)
				} else {
					say("nvme: coprocessor teruggegeven\n")
				}
			}
		}
	} else {
		say("ans: geen adressen in het param-blok (oude loader?)\n")
	}

	// De DART van poort 2 en zijn tweede zeef, de DAPF. TCR bit 2 (bypass-DAPF)
	// bleef nul na een schrijf; als PROTECT de TTBR/TCR vergrendelt weten we
	// waarom, en dan is de DAPF-tabel de enige weg. Adres uit de ADT: de
	// dart-apcie2-node draagt twee reg-vensters, DART en DAPF.
	{
		const dartBase, dapfBase = uintptr(0x492000000), uintptr(0x38079c000)
		say("dart: PROTECT %#x TCR0 %#x", dev.Read32(dartBase+0x200), dev.Read32(dartBase+0x1000))
		dev.Write32(dartBase+0x1000, 0x6)
		say(" (na write 0x6: %#x)", dev.Read32(dartBase+0x1000))
		dev.Write32(dartBase+0x1000, 0x7)
		say(" (na write 0x7: %#x)\n", dev.Read32(dartBase+0x1000))
		dev.Write32(dartBase+0x1000, 0x2)
		for i := uintptr(0); i < 6; i++ {
			b := dapfBase + i*0x40
			r0, r4 := dev.Read32(b), dev.Read32(b+4)
			start := uint64(dev.Read32(b+0x0c))<<32 | uint64(dev.Read32(b+8))
			end := uint64(dev.Read32(b+0x14))<<32 | uint64(dev.Read32(b+0x10))
			say("dapf: [%d] r0 %#x r4 %#x window %#x..%#x r20 %#x\n",
				i, r0, r4, start, end, dev.Read32(b+0x20))
		}
	}

	// De controller zelf: wat zegt het silicium over de link? De config-space
	// van de brug spreekt PCIe (LNKSTA), maar Apple's poort heeft zijn eigen
	// status- en resetregisters (m1n1 src/pcie.c). port_base komt uit de ADT:
	// 7 gedeelde regs, daarna 8 per poort → poort N = reg[7+8N].
	for _, p := range []struct {
		port int
		base uintptr
	}{{0, 0x490028000}, {2, 0x492028000}} {
		say("apcie: port %d @ %#x LINKSTS %#x (up=%d busy=%d l2=%d) STATUS %#x APPCLK %#x RESET(t602x) %#x\n",
			p.port, p.base,
			dev.Read32(p.base+0x208), dev.Read32(p.base+0x208)&1, dev.Read32(p.base+0x208)>>2&1, dev.Read32(p.base+0x208)>>6&1,
			dev.Read32(p.base+0x804), dev.Read32(p.base+0x800), dev.Read32(p.base+0x82c))
	}

	// De DART van poort 2: staat vertaling aan, en voor welke streams?
	say("dart: apcie2 @ %#x ERROR %#x\n", uintptr(dartAPCIE2), dev.Read32(uintptr(dartAPCIE2)+dartError))
	for sid := 0; sid < 4; sid++ {
		tcr := dev.Read32(uintptr(dartAPCIE2) + dartTCR + uintptr(sid)*4)
		ttbr := dev.Read32(uintptr(dartAPCIE2) + dartTTBR + uintptr(sid)*4)
		say("dart:   sid %d TCR %#x (translate=%d bypass-dart=%d bypass-dapf=%d) TTBR %#x\n",
			sid, tcr, tcr&1, tcr>>1&1, tcr>>2&1, ttbr)
	}
}

// pcieLinkStatus loopt de capability-lijst af naar de PCIe-capability (id 0x10)
// en geeft LNKSTA (offset +0x12) terug.

// probeFirmware: wat de FIRMWARE achterliet — waar dit image geladen is,
// wat er in boot_args staat en wat de device tree zegt. Staat los van de
// PCIe-meting: juist als die niets vindt wil je deze regels zien.
func probeFirmware() {
	// Het bootobject: waar de firmware dit image neerzette en of de stub het
	// naar zijn linkadres verplaatst heeft. Met AT= in de loader is dit de hele
	// proef — het image stond ergens anders en draait tóch hier.
	if src := apple.StubSource(); src == 0 {
		say("stub: niet gedraaid (image zonder bootstub, of rechtstreeks naar de kern gesprongen)\n")
	} else if src == apple.RamBase {
		say("stub: geladen op %#x = het linkadres, niets te verplaatsen; x0 %#x\n", src, apple.FirmwareX0())
	} else {
		say("stub: geladen op %#x, VERPLAATST naar %#x (%d MB verderop); x0 %#x\n",
			src, uint64(apple.RamBase), (uint64(apple.RamBase)-src)>>20, apple.FirmwareX0())
	}
	if _, ok := apple.Params(); !ok {
		say("stub: geen loader — alles uit boot_args en de device tree\n")
	}

	// iBoot's boot_args, door onze eigen lezer. Zijn wíj straks het bootobject,
	// dan komt dit blok in x0 binnen en is het param-blok overbodig — dus toetsen
	// we hem nu, tegen echte firmware, zolang m1n1 het adres nog kan doorgeven.
	if x0 := apple.FirmwareX0(); x0 != 0 {
		// Via apple.Boot(): die maakt het lage DRAM eerst bereikbaar (op dit
		// silicium faultt een 1GB-blok boven 2^40) en onthoudt het antwoord.
		// Rechtstreeks xnuboot.Read op x0 doen faultt vóór die stap.
		if ba, ok := apple.Boot(); ok {
			say("xnuboot: rev %d.%d  RAM %#x+%dMB (fysiek %dMB)  firmware tot %#x\n",
				ba.Revision, ba.Version, ba.PhysBase, ba.MemSize>>20,
				ba.MemSizeActual>>20, ba.TopOfKernelData)
			say("xnuboot: ADT %#x+%#x (devtree %#x, virt_base %#x)  framebuffer %#x %dx%d stride %d\n",
				ba.ADT, ba.ADTSize, ba.DevTree, ba.VirtBase, ba.FB.Base, ba.FB.W, ba.FB.H, ba.FB.Stride)
			if ba.ADT == 0 {
				say("xnuboot: blok op %#x woord voor woord:\n", x0)
				for off := uintptr(0); off < 0x80; off += 8 {
					say("xnuboot:   +%#03x %#018x\n", off,
						uint64(dev.Read32(uintptr(x0)+off))|uint64(dev.Read32(uintptr(x0)+off+4))<<32)
				}
			}
		} else {
			say("xnuboot: blok op %#x niet leesbaar\n", x0)
		}
	}

	// De ADT, door onze eigen lezer. Alles wat de loader nu in Python uit de
	// boom haalt en in het param-blok legt, moeten we straks zelf kunnen —
	// zonder m1n1 is er geen loader meer. Dit is de toets: dezelfde getallen
	// uit dezelfde boom, maar dan van deze kant.
	if ba, ok := apple.Boot(); ok {
		if t, ok := adt.Open(uintptr(ba.ADT), ba.ADTSize); ok {
			root, _ := t.Name(0)
			say("adt: wortel %q, %d bytes\n", root, ba.ADTSize)
			for _, e := range []struct {
				path string
				idx  int
			}{
				{"/arm-io/dockchannel-uart", 0}, {"/arm-io/uart0", 0},
				{"/arm-io/ans", 0}, {"/arm-io/ans", 3}, {"/arm-io/ans", 9},
				{"/arm-io/sart-ans", 0}, {"/arm-io/pmgr", 0},
			} {
				base, size, ok := t.RegOf(e.path, e.idx)
				say("adt: %-26s reg[%d] %#x+%#x (%v)\n", e.path, e.idx, base, size, ok)
			}
			asc, nvmmu, nvme, sart := apple.ANSAddrs()
			say("adt: ANS asc %#x nvmmu %#x nvme %#x sart %#x, pmgr-cpustart %#x\n",
				asc, nvmmu, nvme, sart, apple.PMGRCPUStart())
			for _, c := range apple.CPUs() {
				rv, locked, _ := apple.RVBAR(c)
				say("adt: core %d cluster %d die %d impl %#x → rvbar %#x lock=%v\n",
					c.Core, c.Cluster, c.Die, c.Impl, rv, locked)
			}
			if ok, why := apple.OwnCores(); ok {
				say("cores: van ons — RVBAR wijst in ons image, m1n1's spin-table is overbodig\n")
			} else {
				say("cores: NIET van ons — %s\n", why)
			}
			if n, ok := t.Path("/arm-io/apcie/pci-bridge2/lan-1gb"); ok {
				if a, sz, ok := t.Prop(n, "local-mac-address"); ok && sz >= 6 {
					say("adt: NIC MAC %02x:%02x:%02x:%02x:%02x:%02x\n",
						dev.Read8(a), dev.Read8(a+1), dev.Read8(a+2),
						dev.Read8(a+3), dev.Read8(a+4), dev.Read8(a+5))
				}
			}
		} else {
			say("adt: onze lezer weigert de boom op %#x\n", ba.ADT)
		}
	}

}

func pcieLinkStatus(bus, devno, fn int) (uint32, bool) {
	status := dev.Read32(pcieCfg(bus, devno, fn, 4)) >> 16
	if status&0x10 == 0 { // capabilities list
		return 0, false
	}
	ptr := uintptr(dev.Read32(pcieCfg(bus, devno, fn, 0x34)) & 0xfc)
	for i := 0; i < 48 && ptr != 0; i++ {
		hdr := dev.Read32(pcieCfg(bus, devno, fn, ptr))
		if hdr&0xff == 0x10 {
			return dev.Read32(pcieCfg(bus, devno, fn, ptr+0x10)) >> 16, true
		}
		ptr = uintptr(hdr >> 8 & 0xfc)
	}
	return 0, false
}

// Het venster van de probe: 64MB vanaf apple.RamBase (1TiB + 4GB).
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = apple.RamBase

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = 0x04000000 // 64MB

// report verzamelt alles wat de probe meldt, zodat de heartbeat het periodiek
// kan herhalen: op dit board haakt de console (dockchannel via de kis-poorten
// van de laptop) soms pas laat aan — na een kabel-herplug, of via een
// hypervisor-vuart — en een eenmalig rapport is dan al voorbij.
var report strings.Builder

func say(format string, args ...any) {
	s := fmt.Sprintf(format, args...)
	report.WriteString(s)
	fmt.Print(s)
}

func main() {
	// Eerst één levensteken, dan drie seconden niets: de loader opent de
	// console pas ná de sprong (de proxy en de console delen de kabel), en
	// het rapport mag niet in dat gat vallen. Komt er na de banner niets
	// meer, dan is de klok (time.Sleep) het eerste verdachte.
	fmt.Printf("\nprobeapple: alive on Apple silicon — full report in 3s\n")
	time.Sleep(3 * time.Second)
	say("\nprobeapple — Apple silicon bring-up probe (%s)\n\n", runtime.Version())

	// 2. boot-EL en de effectieve EL2-configuratie.
	el := apple.BootEL()
	say("boot: entered at EL%d (2 = stage-2 cage possible)\n", el)
	hcr := apple.EffectiveHCR()
	say("boot: HCR_EL2 after our write %#x — E2H=%d TGE=%d RW=%d\n",
		hcr, hcr>>34&1, hcr>>27&1, hcr>>31&1)
	if hcr>>34&1 == 1 {
		say("boot: E2H is stuck at 1 → VHE-only silicon: cpu/el2 must use the _EL12 encodings\n")
	} else {
		say("boot: E2H cleared → nVHE like every other HopOS board\n")
	}
	say("boot: CNTHCTL_EL2 %#x\n", apple.EffectiveCNTHCTL())

	// 3. identiteit en ID-registers.
	mpidr := dev.MPIDR()
	say("boot: MPIDR %#x → aff0 %d aff1 %d aff2 %d → CoreID %d (boot core per m1n1: %#x)\n",
		mpidr, mpidr&0xFF, mpidr>>8&0xFF, mpidr>>16&0xFF, apple.CoreID(), apple.BootMPIDR())
	mmfr0, mmfr1, mmfr4 := apple.ReadMMFR0(), apple.ReadMMFR1(), apple.ReadMMFR4()
	say("id: MMFR0 %#x (PARange %d) MMFR1 %#x (VH %d) MMFR4 %#x (E2H0 %d; 0xF = E2H is RES1)\n",
		mmfr0, mmfr0&0xF, mmfr1, mmfr1>>8&0xF, mmfr4, mmfr4>>24&0xF)
	tcr := apple.ReadTCR()
	say("mmu: TCR_EL1 %#x → T0SZ %d (16 = 48-bit L0 path, 25 = flat 39-bit) IPS %d, SCTLR_EL1 %#x\n",
		tcr, tcr&0x3F, tcr>>32&0x7, apple.ReadSCTLR())
	say("mmu: RAM window %#x..%#x (%d MB)\n", ramStart, ramStart+ramSize, ramSize>>20)

	// 4. het param-blok van de loader.
	p, ok := apple.Params()
	if !ok {
		say("params: no block at %#x (loader did not write one) — UARTs from constants\n", apple.ParamBase)
	} else {
		say("params: boot cpu %d, hop cpu %d (release %#x)\n", p.BootCPU, p.Hop.CPU, p.Hop.Release)
		for i, c := range apple.CPUs() {
			say("params: cpu%d cluster %d core %d release %#x\n", i, c.Cluster, c.Core, p.Release[i])
		}
	}

	// 6. geheugen: Normal (in het venster) en Device (buiten het venster).
	top := uintptr(ramStart+ramSize) - 0x2000
	dev.Write64(top, 0x5A5A_1234_ABCD_0001)
	say("mem: normal write/read at %#x → %#x\n", top, dev.Read64(top))
	dev.Write64(apple.WakeBase, 0xDEAD_BEEF_0000_0002)
	say("mem: device write/read at %#x → %#x\n", uintptr(apple.WakeBase), dev.Read64(apple.WakeBase))

	// 7. de klok.
	say("clock: CNTFRQ %d Hz\n", apple.CNTFRQ())
	// Loopt de teller op DEZE core? Na de hop woont HOP op een zuinige core, en
	// een teller die daar stilstaat laat élke time.Sleep eeuwig duren — dus dit
	// meten we vóór we er een aanroepen.
	c0 := apple.CNTPCT()
	for i := 0; i < 20_000_000; i++ {
		runtime.Gosched()
		break
	}
	for i := 0; i < 5_000_000; i++ {
	}
	say("clock: CNTPCT %d → %d (delta %d)\n", c0, apple.CNTPCT(), apple.CNTPCT()-c0)

	t0 := time.Now()
	time.Sleep(100 * time.Millisecond)
	say("clock: time.Sleep(100ms) took %s\n", time.Since(t0).Round(time.Millisecond))

	// 8a. Vóór we m1n1's geheugen aanraken: hoe ziet onze stage-1 die GB?
	//     Boot 4 gaf op de eerste write naar de spin-table een EL2-abort
	//     ("address size fault, level 0", FAR = het release-adres). De RAM-GB
	//     (via een L2-tabel) en de lage MMIO (1GB-blokken onder 2^39) werken;
	//     de verdachte is het 1GB-blok met een uitvoeradres boven 2^40.
	say("mmu: TTBR0_EL1 %#x, L0[0] %#x L0[2] %#x\n", apple.ReadTTBR0(), apple.L0Entry(0), apple.L0Entry(2))
	say("mmu: L1[m1n1 GB] %#x  L1[our GB] %#x\n", apple.L1Entry(uintptr(apple.DRAMBase)), apple.L1Entry(uintptr(ramStart)))
	say("mmu: SPRR_CONFIG_EL1 %#x (bit0 = Apple permission remap on)\n", apple.ReadSPRRConfig())
	if ok && p.Release[0] != 0 {
		a := uintptr(p.Release[0]) - 16 // spin_table[0].mpidr
		// Boot 5 bewees: het 1GB-blok faultt, een L2-tabel met 2MB-blokken
		// werkt. MapDRAM deed dat al bij het lezen van boot_args; hier alleen
		// de leestest die bewaakt dat het blijft werken.
		l0, l1, l2, l3 := apple.Walk(a)
		say("mmu: walk %#x → L0 %#x L1 %#x L2 %#x L3 %#x\n", a, l0, l1, l2, l3)
		say("mem: reading m1n1's spin table at %#x (if this dies, the mapping is the problem) ...\n", a)
		say("mem: spin_table[0].mpidr = %#x flag = %#x\n", dev.Read64(a), dev.Read64(a+8))
	}

	// 8. de cores: spin-table-release naar de parkeerlus.
	if self := apple.SelfCPU(); self >= 0 {
		entry := apple.ParkEntryPC()
		mech := "m1n1's spin table"
		if own, _ := apple.OwnCores(); own {
			mech = "PMGR + stubReset mailbox (cores are OURS)"
		}
		say("\nsmp: park loop at %#x; starting cores via %s\n", entry, mech)
		up := 0
		cpus := apple.CPUs()
		for cpu := range cpus {
			if cpu == self {
				continue
			}
			apple.ClearWake(cpu)
			// apple.Start kiest zelf de weg: m1n1's spin-table zolang die
			// bestaat, en anders — pas als RVBAR naar óns image wijst — de
			// PMGR-start met de brievenbus waar stubReset op wacht. Dit is
			// dus dezelfde regel die ná de installatie het eigen-cores-pad
			// bewijst, zonder dat er een ander probe-image nodig is.
			if !apple.Start(cpu, entry, uint64(apple.WakeSlot(cpu))) {
				say("smp: cpu%d has no start path (no spin table, cores not ours)\n", cpu)
				continue
			}
			var got uint64
			for i := 0; i < 50 && got == 0; i++ {
				time.Sleep(10 * time.Millisecond)
				got = apple.Wake(cpu)
			}
			if got == 0 {
				say("smp: cpu%d released but stayed silent\n", cpu)
				continue
			}
			want := uint64(cpus[cpu].Cluster)<<8 | uint64(cpus[cpu].Core)
			match := "matches the device tree"
			if got&0xFFFF != want {
				match = fmt.Sprintf("DIFFERS from the tree's cluster:core %#x", want)
			}
			say("smp: cpu%d UP — MPIDR %#x (aff0 %d aff1 %d), %s\n", cpu, got, got&0xFF, got>>8&0xFF, match)
			up++
		}
		say("smp: %d of %d secondary cores answered\n", up, len(cpus)-1)
	}

	// 8b. PCIe/DART: de topologie vóór de NIC-driver (meten eerst).
	probeFirmware()
	probePCIe()

	// 8c. Idle-mechanica: slaapt WFE hier eigenlijk wel? De governor meldde
	//     3,25M wakes/s bij 40% "slaap" — dat is 120ns per wake, en dan is de
	//     vraag of WFE op dit silicium überhaupt blokkeert of alleen een
	//     klaarstaand event opeet. Daarnaast de kandidaat-vervanger: WFI met de
	//     fysieke timer als deadline (wat de RISC-V-kant met de CLINT doet).
	hz := apple.CNTFRQ()
	say("\nidle-mechanics: CNTFRQ %d Hz, CNTKCTL_EL1 %#x\n", hz, apple.ReadCNTKCTL())
	for _, n := range []uint64{1, 10, 1000} {
		t := apple.WFEBurst(n)
		say("idle-mechanics: %4d WFE took %d ticks (%d ns each)\n", n, t, t*1e9/hz/n)
	}
	for _, ms := range []uint64{1, 5} {
		want := hz * ms / 1000
		t := apple.WFITimer(want)
		say("idle-mechanics: WFI with %dms timer slept %d ticks (%.2f ms) — %s\n",
			ms, t, float64(t)*1000/float64(hz),
			map[bool]string{true: "REAL SLEEP", false: "returned early"}[t > want*8/10])
	}

	// 9. heartbeat, mét de idle-meting (doel "idling", Derek 29-08): hoe vaak
	//    wordt de WFE-governor per seconde gewekt, en welk deel van de tijd
	//    slaapt de core echt? Op 1GHz CNTFRQ met EVNTI≤15 is de event-stream-
	//    periode 65µs — 15× vaker wakker dan op de Pi (~1ms). De wekteller
	//    landt op een device-woord (buiten het venster) zodat hij coherent
	//    leesbaar is, precies zoals CtrlWakes voor een app.
	wakesWord := uintptr(apple.WakeBase) + 0x800
	dev.Write64(wakesWord, 0)
	idle.PublishWakes(wakesWord)
	fmt.Printf("\nprobe: heartbeat every 1s — verify spacing against a watch; full report repeats every 20s\n")
	fmt.Printf("idle: CNTKCTL event stream — CounterHz %d\n", idle.CounterHz())
	start := time.Now()
	lastWakes, lastTicks := dev.Read64(wakesWord), idle.Ticks()
	for i := 1; ; i++ {
		time.Sleep(time.Second)
		w, t := dev.Read64(wakesWord), idle.Ticks()
		fmt.Printf("tick %d — uptime %s — idle: %d wakes/s, slept %.1f%%\n", i, time.Since(start).Round(time.Millisecond),
			w-lastWakes, 100*float64(t-lastTicks)/float64(idle.CounterHz()))
		lastWakes, lastTicks = w, t
		if i%20 == 0 {
			fmt.Printf("\n---- report (repeat %d) ----%s---- end of report ----\n", i/20, report.String())
		}
	}
}

// dumpGPT leest de partitietabel van de schijf. Puur lezen — dit is de vraag
// "waar mogen we schrijven" en de schijf beantwoordt hem zelf: de GPT zegt
// welke partities er staan en, uit het gat tussen de laatste en het einde,
// hoeveel ruimte er vrij is. LBA 1 draagt de header, de entries staan waar de
// header ze aanwijst; elke entry is 128 bytes en de naam is UTF-16.
func dumpGPT(disk *nvme.Controller) {
	bs := int(disk.BlockSize)
	hdr := make([]byte, bs)
	if err := disk.Read(1, hdr); err != nil {
		say("gpt: header lezen: %v\n", err)
		return
	}
	if string(hdr[0:8]) != "EFI PART" {
		say("gpt: geen GPT-handtekening op LBA 1 (%q) — schijf ongepartitioneerd of andere blokmaat\n",
			hdr[0:8])
		return
	}
	firstUsable := binary.LittleEndian.Uint64(hdr[40:])
	lastUsable := binary.LittleEndian.Uint64(hdr[48:])
	entryLBA := binary.LittleEndian.Uint64(hdr[72:])
	numEntries := binary.LittleEndian.Uint32(hdr[80:])
	entrySize := binary.LittleEndian.Uint32(hdr[84:])
	say("gpt: bruikbaar %d..%d, %d entries van %d bytes vanaf LBA %d\n",
		firstUsable, lastUsable, numEntries, entrySize, entryLBA)

	perBlock := uint32(bs) / entrySize
	buf := make([]byte, bs)
	var used, highest uint64
	for i := uint32(0); i < numEntries; i++ {
		if i%perBlock == 0 {
			if err := disk.Read(entryLBA+uint64(i/perBlock), buf); err != nil {
				say("gpt: entries lezen: %v\n", err)
				return
			}
		}
		e := buf[(i%perBlock)*entrySize:]
		empty := true
		for _, b := range e[0:16] {
			if b != 0 {
				empty = false
				break
			}
		}
		if empty {
			continue
		}
		first := binary.LittleEndian.Uint64(e[32:])
		last := binary.LittleEndian.Uint64(e[40:])
		name := make([]byte, 0, 36)
		for j := 56; j+1 < 56+72; j += 2 {
			c := binary.LittleEndian.Uint16(e[j:])
			if c == 0 {
				break
			}
			if c < 0x80 {
				name = append(name, byte(c))
			} else {
				name = append(name, '?')
			}
		}
		used += last - first + 1
		if last > highest {
			highest = last
		}
		say("gpt: [%d] %-24s LBA %d..%d (%d MB)\n", i, name, first, last,
			(last-first+1)*disk.BlockSize/(1<<20))
	}
	free := lastUsable - highest
	say("gpt: in gebruik %d MB, vrij achter de laatste partitie %d MB\n",
		used*disk.BlockSize/(1<<20), free*disk.BlockSize/(1<<20))
}
