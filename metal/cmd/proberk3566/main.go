// proberk3566 is het meetinstrument van de Radxa Zero 3E-bring-up
// (docs/archief/radxa-zero3.md): het zet de REFERENTIE-aannames van
// board/rk3566 om in gemeten feiten, vóór er één regel agent-code voor dit bord
// geschreven wordt (Dereks methode: referentie eerst, meetinstrument eerst
// bewijzen).
//
// Wat hij meet, in volgorde van "sterft stil" naar "inventaris" — en dát is de
// ordening die telt: elke sectie kondigt zich aan vóór hij een nieuw blok
// aanraakt, want een ongeklokt Rockchip-blok geeft geen abort maar houdt de bus
// vast. Waar de log stopt ís dan het antwoord.
//
//  1. dat er überhaupt output komt → UART-basis, reg-shift en de baud kloppen;
//  2. het boot-EL uit de scratch → booti leverde EL2 en cpuinit.s draaide;
//  3. de rauwe MPIDR → de CoreID-aanname (op core 0 niet onderscheidend);
//  4. de DTB via x0 → geheugenkaart, /memreserve/ (waar zit TF-A/OP-TEE écht),
//     bootargs (APPEND), initrd (hopos.cfg-route), simplefb (die er niet is);
//  5. PSCI: versie, en CPU_ON van core 1..3 naar een parkeerlus die zijn eigen
//     MPIDR neerlegt — het kooi-fundament, én de enige manier om aff0-vs-aff1
//     te beslissen. Beide targets worden geprobeerd; welke werkt is de meting;
//  6. GICD_TYPER → inventaris, en bewijst dat het blok geklokt is;
//  7. het GMAC-versieregister (gmac.go) → daarna de hele glue-stapel: klokken,
//     pinmux, M1-pinset, GRF-modus, PHY-reset, en een MDIO-scan over alle 32
//     adressen. Eén PHY-id bewijst die vijf lagen in één regel;
//  8. de kleine blokken (blocks.go): temperatuursensor, watchdog-TOP (gemeten,
//     niet gewapend — een probe die zichzelf herstart kan niets meten) en het
//     TRNG met twee rondes, want "elke keer hetzelfde antwoord" is de storing
//     die er willekeurig uitziet;
//  9. de scanout (blocks.go): power-domein → VOP2 → cfg_done → HDMI-ID's LEZEN
//     zonder te schrijven → PHY-lock → TMDS, plus een testpatroon dat in één
//     frame kleurvolgorde, stride én geometrie laat zien;
//  10. een 1Hz-heartbeat mét temperatuur → CNTFRQ klopt, en het thermische
//     verloop is meteen zichtbaar in plaats van als momentopname.
//
// ALLE TREDEN zijn inmiddels op ijzer gehaald (05/06-08, zie het draaiboek):
// EL2-boot, PSCI, gigabit met DHCP-lease, en beeld op HDMI. Deze probe blijft
// bestaan omdat hij een ándere vraag beantwoordt dan de agent: hij meet het
// silicium zonder van een netwerk, een plan of een config af te hangen — het
// instrument dat een regressie in de glue aanwijst zonder de hele stapel te
// verdenken.
//
// Bouwen/flashen: image/radxa-zero3.sh. Console: wat U-Boot naliet
// (Rockchip-default 1500000 8N1).
package main

import (
	"fmt"
	"runtime"
	"time"
	_ "unsafe" // voor go:linkname

	"github.com/xinix00/HopOS/metal/board/rk3566"
	"github.com/xinix00/HopOS/metal/cpu/psci"
	"github.com/xinix00/HopOS/metal/dev"
	"github.com/xinix00/HopOS/metal/fw/fdt"
)

// GICD van de GIC-600 (rk3566.dtsi) — alleen voor de TYPER-read hieronder;
// HopOS pollt en heeft geen interrupt-controller nodig, dus dit hoort niet in
// het board-pakket tot een driver hem echt gebruikt.
const gicdBase = 0xFD400000

// Het HOP-venster van dit bord (zie rk3566.RamBase voor het waarom van de
// basis; 64MB blijft onder de klassieke OP-TEE-regio op 0x08400000 tot de
// /memreserve/-dump bewijst wat daar echt woont).
//
//go:linkname ramStart runtime/goos.RamStart
var ramStart uint = rk3566.RamBase

//go:linkname ramSize runtime/goos.RamSize
var ramSize uint = 0x04000000 // 64MB

func main() {
	fmt.Printf("\nproberk3566 — Radxa Zero 3E bring-up probe (%s)\n\n", runtime.Version())

	// boot-EL: door cpuinit.s op de scratch gelegd vóór de drop naar EL1.
	//    0 betekent: cpuinit zag geen EL2/EL3 (EL1-boot) — dan is er geen
	//    stage-2 mogelijk en is dit bord (nog) geen HopOS-kandidaat.
	el := dev.Read64(rk3566.BootScratch)
	fmt.Printf("boot: entered at EL%d (0 = EL1 — no cage possible on this chain)\n", el)

	// De identiteits-aanname: A55-cluster → aff1-nummering. Op core 0 zijn aff0
	// en aff1 beide nul, dus dit bewijst niets — punt 5 doet dat.
	mpidr := dev.MPIDR()
	fmt.Printf("boot: MPIDR %#x → CoreID %d (aff1 assumption — verify 0 here, 1..3 on secondaries)\n",
		mpidr, rk3566.CoreID())

	// De DTB die U-Boot in x0 meegaf.
	dtb := uintptr(dev.Read64(rk3566.DTBPtr))
	if dtb == 0 || !fdt.Valid(dtb) {
		fmt.Printf("fdt: no valid DTB via x0 (ptr %#x) — booti convention broken?\n", dtb)
	} else {
		fmt.Printf("fdt: DTB at %#x (%d bytes)\n", dtb, fdt.BlobSize(dtb))
		if n, ok := fdt.MemTotal(dtb); ok {
			fmt.Printf("mem: %d MB DRAM\n", n>>20)
		}
		if regs, ok := fdt.MemRegions(dtb); ok {
			for _, r := range regs {
				fmt.Printf("mem: region %#x..%#x\n", r.Addr, r.Addr+r.Size)
			}
		}
		// Wie woont er in laag DRAM? Dit beslist het definitieve HOP-venster
		// en of 0x08400000 (OP-TEE-conventie) echt bezet is.
		if rsv := fdt.MemReserve(dtb); len(rsv) == 0 {
			fmt.Printf("mem: no /memreserve/ entries — low-DRAM occupants unknown, assume TF-A near 0x40000\n")
		} else {
			for _, r := range rsv {
				fmt.Printf("mem: reserved %#x..%#x\n", r.Addr, r.Addr+r.Size)
			}
		}
		if args, ok := fdt.Bootargs(dtb); ok {
			fmt.Printf("chosen: bootargs %q\n", args)
		} else {
			fmt.Printf("chosen: no bootargs (extlinux APPEND not delivered)\n")
		}
		if s, e, ok := fdt.InitrdRegion(dtb); ok {
			fmt.Printf("chosen: initrd %#x..%#x (%d bytes) — the hopos.cfg route works\n", s, e, e-s)
		} else {
			fmt.Printf("chosen: no initrd (add one to the extlinux entry to prove the cfg route)\n")
		}
		if f, ok := fdt.Framebuffer(dtb); ok {
			fmt.Printf("fb: simplefb %dx%d stride %d bpp %d at %#x — glass without a driver\n",
				f.Width, f.Height, f.Stride, f.BPP, f.Base)
		} else {
			fmt.Printf("fb: no simplefb node — U-Boot video off or not patched in (GUI stays headless)\n")
		}
	}

	// PSCI: de provider (TF-A bl31) en het wekken van de andere drie cores.
	//    Dit is het kooi-fundament — zonder CPU_ON heeft HopOS niets om een app
	//    op te zetten — én de enige manier om de MPIDR-nummering te beslissen:
	//    op core 0 zijn aff0 en aff1 beide nul, dus niet onderscheidend.
	maj, min := psci.Version()
	fmt.Printf("\npsci: v%d.%d (SMC conduit)\n", maj, min)

	entry := rk3566.ParkEntryPC()
	fmt.Printf("psci: park loop at %#x\n", entry)
	for core := 1; core <= 3; core++ {
		// Twee kandidaat-targets, want dát is precies de open vraag: nummeren
		// de A55's in aff0 (0,1,2,3) of in aff1 (0x100, 0x200, 0x300)?
		for _, target := range []uint64{uint64(core) << 8, uint64(core)} {
			rk3566.ClearWake(core)
			ret := psci.On(target, entry, uint64(rk3566.WakeSlot(core)))
			if ret != 0 {
				fmt.Printf("psci: CPU_ON target %#x → %d (afgewezen)\n", target, ret)
				continue
			}
			// Even wachten tot hij zijn MPIDR neerlegt; komt er niets, dan
			// accepteerde de firmware het target maar leeft de core niet.
			var got uint64
			for i := 0; i < 50 && got == 0; i++ {
				time.Sleep(10 * time.Millisecond)
				got = rk3566.Wake(core)
			}
			if got == 0 {
				// Óók affinity_info printen: dat scheidt "de core loopt maar zijn
				// schrijfactie is voor ons onzichtbaar" van "de core is nooit
				// gestart". Zonder dat onderscheid kostte deze regel op 05-08 een
				// hele boot aan gissen.
				fmt.Printf("psci: CPU_ON target %#x → accepted, but core stayed silent (affinity_info %d)\n",
					target, psci.AffinityInfo(target))
				continue
			}
			fmt.Printf("psci: core %d UP via target %#x — its MPIDR %#x (aff0 %d, aff1 %d)\n",
				core, target, got, got&0xFF, (got>>8)&0xFF)
			fmt.Printf("psci: affinity_info(%#x) = %d (0=on, 1=off, 2=on_pending)\n",
				target, psci.AffinityInfo(target))
			break // dit target werkte; het andere hoeft niet
		}
	}

	// GIC-600: alleen een read van GICD_TYPER — hoeveel interrupt-ID's en
	//    hoeveel CPU-interfaces meldt het silicium. HopOS pollt, dus dit is
	//    inventaris en geen driver; het is wél gratis en het bewijst dat het
	//    blok geklokt is.
	typer := dev.Read32(gicdBase + 0x4)
	fmt.Printf("\ngic: GICD_TYPER %#x → %d SPIs, %d CPU interfaces%s\n",
		typer, (int(typer&0x1F)+1)*32, ((int(typer)>>5)&0x7)+1,
		map[bool]string{true: ", security extn", false: ""}[typer&(1<<10) != 0])

	// ALS LAATSTE de GMAC, want deze read is de enige die kán hangen: een
	//    ongeklokt Rockchip-blok geeft geen abort maar houdt de bus vast, en dan
	//    sterft de probe zonder één woord. Door hem achteraan te zetten hebben
	//    we alles hierboven al gelezen — dat is het verschil tussen "we weten
	//    niets" en "we weten alles behalve dit".
	//
	//    GEMETEN 05-08: hij geeft 0x3051 en hangt niet — het blok is geklokt
	//    ondanks dat U-Boot "No ethernet found" meldt. De read staat hier nog
	//    steeds als laatste van de vroege sectie, want dat is wat hem goedkoop
	//    maakt: gaat er ooit iets mis met de klokken, dan hebben we alles
	//    hierboven al gemeten.
	fmt.Printf("\ngmac: reading version register at %#x (this may hang — that IS the measurement)\n", rk3566.GMAC1Base+0x110)
	ver := dev.Read32(rk3566.GMAC1Base + 0x110) // DWMAC4: GMAC_VERSION
	fmt.Printf("gmac: version %#x (snpsver %#x, userver %#x) — block is clocked\n",
		ver, ver&0xFF, (ver>>8)&0xFF)

	probeGMAC()
	probeBlocks()
	probeVOP2()
	probeHDMI()

	// Heartbeat: 1Hz-tikken die met een stopwatch te verifiëren zijn —
	//    lopen ze te snel/langzaam, dan is CNTFRQ niet wat de runtime denkt.
	fmt.Printf("\nprobe: heartbeat every 1s — verify spacing against a watch\n")
	start := time.Now()
	for i := 1; ; i++ {
		time.Sleep(time.Second)
		fmt.Printf("tick %d — uptime %s%s\n", i, time.Since(start).Round(time.Millisecond), tempLine())
	}
}
