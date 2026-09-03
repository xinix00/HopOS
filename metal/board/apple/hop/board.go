// Package hop is de HOP-bedrading van het Apple-board (Mac mini M4): de
// volledige board.Board-implementatie. Alleen HOP-kant-binaries (cmd/)
// importeren deze helft; app-images gebruiken het generieke app-board
// (board/hopslot) en linken zo nooit tegen boardcode — dezelfde bronsplitsing
// als bij de Pi's, de Radxa en de LicheeRV.
//
// Wat hier anders is dan op elk ander ARM-board: er is geen PSCI. Natief zijn
// de cores van ons (RVBAR → stubReset, PMGR-start); onder m1n1 laat
// apple.Release ze los uit zijn spin-table — dat is Cores().Start. Een core
// die eenmaal van ons is komt nooit meer bij de firmware terug — HopOS
// parkeert zijn cores zelf (kern/slots: de EL2-parkeerlus), dus State is hier
// onze eigen boekhouding: Off tot de eerste Start, daarna On.
//
// Core-nummering. HopOS rekent met core 0 = de HOP-core en 1..N = app-cores,
// aaneengesloten. m1n1 boot op cpu 6 (een P-core) en nummert 0..9; de
// logische HopOS-index laat die boot-cpu weg: cpu < boot → core cpu+1,
// cpu > boot → core cpu. Op de M4 (boot 6): cores 1..6 = de E-cores cpu 0..5,
// cores 7..9 = de P-cores cpu 7..9. Apps merken hier niets van: hun slotnummer
// komt van de slotHint die HOP in het image patcht (board/hopslot).
//
// Netwerk: nog geen NIC (de Broadcom 57762 achter Apple's PCIe is het volgende
// werk); ProbeNIC meldt "geen NIC" en de agent draait headless — precies het
// pad dat de Radxa op 05-08 ook eerst liep.
package hop

import (
	"fmt"
	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/board"
	"github.com/xinix00/HopOS/metal/v2/board/apple"
	"github.com/xinix00/HopOS/metal/v2/driver/fb"
	"github.com/xinix00/HopOS/metal/v2/driver/nvme"
	"github.com/xinix00/HopOS/metal/v2/driver/pcie"
	"github.com/xinix00/HopOS/metal/v2/driver/rtkit"
	"github.com/xinix00/HopOS/metal/v2/driver/smc"
	"github.com/xinix00/HopOS/metal/v2/fw/gpt"
	"sync"
	"time"
)

// machine is de board-implementatie voor Apple silicon onder m1n1.
type machine struct{}

// init registreert dit board: elke HOP-binary voor dit bord importeert deze
// hop-helft (cmd/hopos/board_apple.go), dus board.Current() is meteen geldig.
func init() { board.Use(machine{}) }

// Conformiteit compile-time bewezen (Derek, 18-07): zonder deze regel leunt het
// Board-contract puur op board.Use() at runtime.
var _ board.Board = machine{}

// cpuOf vertaalt een logische HopOS-core (1..N) naar de cpu-index van de
// boom; -1 als hij niet bestaat. Core 0 is HOP's eigen cpu. Dezelfde volgorde
// als Cores().App(), en alleen nog nodig voor CoreClass — de kern spreekt
// Cores in fysieke nummers.
func cpuOf(core int) int {
	self := apple.SelfCPU()
	if self < 0 || core < 0 || core >= apple.NumCPUs() {
		return -1
	}
	if core == 0 {
		return self
	}
	if core-1 < self {
		return core - 1
	}
	return core
}

func (machine) CoreID() int      { return apple.CoreID() }
func (machine) MemTotal() uint64 { return apple.MemTotal() }

// Privilege/Firmware: EL2-boot vereist. Er ís geen PSCI — iBoot zette ons
// neer en de cores zijn van ons (RVBAR → stubReset), of van m1n1's spin-table.
func (machine) Privilege() error { return board.RequireEL2(apple.BootEL()) }
func (machine) Firmware() string {
	return fmt.Sprintf("iBoot, no PSCI (boot EL%d, %d cpus, self cpu%d)", apple.BootEL(), apple.NumCPUs(), apple.SelfCPU())
}

// Cores: alle cpus behalve de eigen (M4: 9), in cpu-volgorde. Start is de
// spin-table-release of, met eigen cores, PMGR + de brievenbus (apple.Start,
// bewezen op alle negen, 28-08/31-08). State is eigen boekhouding: Off tot de
// eerste Start, daarna On — een core die eenmaal van ons is komt nooit meer
// bij de firmware terug, HopOS parkeert hem zelf. Geen Reset (nog): het
// PMGR-blok kán het (cpustart.go), maar het is niet bewezen en de EL2-parkeerlus
// is het.
//
// IdleMode en Kick: zo idlet Apple silicon.
//
// Een app-core idlet hier op EL2, niet op EL1: de app yieldt (IdleYield, de
// weg die een gedeelde core al had), de switcher slaapt op EL2 met WFI
// (cpu/el2/switch.s, VHE) en HOP wekt hem met een fast IPI — Kick, via HVC
// #3 naar HOP's eigen EL2-handler, waar IPI_RR_GLOBAL_EL1 woont. HOP's
// wekker (kern/slots waker.go) kickt de core wiens bewoner due is (CtxWake)
// of RX heeft. Komt de IPI aan terwijl de app op EL1 draait, dan ackt de
// FIQ-vector hem op EL2 en keert terug; de app ziet niets. Dit is m1n1's
// eigen park-recept op ditzelfde silicium (smp.c: wfi + fast IPI), en Linux'
// AIC-driver bevestigt de vorm (fast IPI = FIQ, ack via IPI_SR_EL1).
//
// GEMETEN 02-09 (kern G, na reboot): vitals op een E- én een P-core 0% cpu,
// 47 wekken/s, idle 100%; de switcher telt ~85 EL2-slaapjes/s, gelijk aan
// het kick-tempo — echte slaap, geen spin. Was: 74% en 1,3M rondes/s.
//
// Waarom niet op EL1, voor wie het nog eens probeert: een core die wij uit
// reset halen komt op met WFI_MODE=0 in CYC_OVRD, en dat register is op de
// M4 vanaf EL1 én EL2 undefined (20+ runs met ESR-rapport); m1n1 slaat het
// op de M4 over en Linux (t8103..t6034, nooit t8132) schrijft het alleen
// als VHE-host. Ook wekt op zo'n core geen enkele FIQ een WFI op EL1
// (timer én IPI gemeten). Op EL2 werkt het allemaal wél — dus daar.
func (machine) Cores() board.Cores {
	self := apple.SelfCPU
	return board.Cores{
		App: func() []int {
			var app []int
			for cpu := range apple.NumCPUs() {
				if cpu != self() {
					app = append(app, cpu)
				}
			}
			return app
		},
		Start: func(cpu int, entry, arg uint64) error {
			if cpu < 0 || cpu >= apple.NumCPUs() || cpu == self() {
				return fmt.Errorf("apple: no app core cpu%d", cpu)
			}
			if apple.Released(cpu) {
				return fmt.Errorf("apple: cpu%d already released — a second release would hijack a running core", cpu)
			}
			if !apple.Start(cpu, entry, arg) {
				return fmt.Errorf("apple: cpu%d did not start", cpu)
			}
			return nil
		},
		State: func(cpu int) board.PowerState {
			switch {
			case cpu < 0 || cpu >= apple.NumCPUs():
				return board.PowerState(-2)
			case cpu == self() || apple.Released(cpu):
				return board.PowerOn
			}
			return board.PowerOff
		},
		// Geen Reset: het PMGR-stopbit (cpustart.go StopCore) zet een core
		// NIET stil — GEMETEN 02-09: "gereset" cores parkeerden zich binnen
		// de flip opnieuw. Tot er een bewezen core-down-recept is (m1n1 kent
		// er geen; deep-WFI zonder retentie is het vermoedelijke pad) is dit
		// een board zonder reset, en parkeert een ingetrokken core zichzelf.
		IdleMode: func(cpu int) uint64 { return layout.IdleYield },
		Kick:     apple.Kick,
	}
}

// CoreClass: de E-cores ("sawtooth", cluster 0) zijn "small", de P-cores
// ("everest", cluster 1) "big". De clustergrens zit in apple.CoreID's tabel;
// hier via het MPIDR dat m1n1 per cpu rapporteerde (aff1 = cluster).
func (machine) CoreClass(core int) string {
	cpu := cpuOf(core)
	cpus := apple.CPUs()
	if cpu < 0 || cpu >= len(cpus) {
		return "big"
	}
	if cpus[cpu].Cluster == 0 {
		return "small"
	}
	return "big"
}

func (machine) TimerOffset() int64       { return apple.ARM64.TimerOffset }
func (machine) SetTimerOffset(off int64) { apple.ARM64.TimerOffset = off }
func (machine) SetWallTime(ns int64)     { apple.ARM64.SetTime(ns) }

// ProbeNIC, Net en DHCPLease staan in net.go — dat is de keten van PCIe-link
// tot DHCP-lease.

// PCIe: de ECAM en het MMIO-venster van Apple's rootpoort (ADT apcie).
func (machine) PCIe() pcie.Window { return Window() }

// Disk brengt de opslag van dit board op en geeft het VENSTER waarin HopOS mag
// schrijven: de coprocessor-SSD, en daarvan alleen het stuk dat niemand anders
// heeft. Optioneel deel van het board-contract (cmd/hopos vraagt er met een
// type-assertie naar), want de gewone weg — PCIe enumereren en een NVMe-device
// vinden — bestaat hier niet: de SSD zit achter de ANS en is geen PCIe-device.
//
// Twee dingen die dit board anders maken dan elk ander, en die allebei in deze
// functie zitten:
//
//  1. De coprocessor moet soms eerst gereset worden. iBoot laat hem draaien maar
//     met de NVMe-kant dicht; dan komt CSTS.RDY nooit op 1. De weg terug is
//     m1n1's volgorde — netjes in slaap praten, dán het power-domein resetten,
//     dán opnieuw opstarten (gemeten 30-08: resetten zónder die slaap levert een
//     blok op dat helemaal niets meer zegt). Daarom pas escaleren ná een
//     mislukte poging, nooit ervoor.
//  2. Deze SSD is niet van ons. macOS staat erop, en Recovery. Het venster komt
//     daarom uit de partitietabel van de schijf zelf (fw/gpt): het grootste
//     stuk waar geen partitie op staat. Is dat er niet, dan krijgt HopOS geen
//     schijf — liever geen volumes dan het bestandssysteem van de eigenaar.
func (machine) Disk() (*nvme.Controller, uint64, uint64, error) {
	asc, nvmmu, nvmeBase, _ := apple.ANSAddrs()
	if asc == 0 || nvmeBase == 0 {
		return nil, 0, 0, fmt.Errorf("apple: no ANS in the device tree")
	}
	if !apple.AllowDMA(apple.StorageDMAPA, apple.StorageDMASize) {
		return nil, 0, 0, fmt.Errorf("apple: SART would not open a window for the queues")
	}
	open := func() (*nvme.Controller, error) {
		c := &nvme.Controller{}
		cfg := nvme.AppleConfig{
			NVMe:  uintptr(nvmeBase),
			NVMMU: uintptr(nvmmu),
			RTKit: &rtkit.Dev{Name: "ans", Base: uintptr(asc), Alloc: apple.StorageBuf},
		}
		return c, c.InitApple(cfg, apple.StorageDMAPA, apple.StorageDMASize)
	}
	disk, err := open()
	if err != nil {
		fmt.Printf("storage: %v — resetting the coprocessor and retrying\n", err)
		if rt := (&rtkit.Dev{Name: "ans", Base: uintptr(asc), Alloc: apple.StorageBuf}); rt != nil {
			_ = rt.Sleep()
		}
		if n := apple.ResetANS(); n == 0 {
			return nil, 0, 0, fmt.Errorf("apple: no ANS power domain to reset: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
		if disk, err = open(); err != nil {
			return nil, 0, 0, err
		}
	}
	tbl, err := gpt.Read(disk.Read, int(disk.BlockSize))
	if err != nil {
		return nil, 0, 0, err
	}
	first, count := gpt.LargestGap(tbl)
	if count == 0 {
		return nil, 0, 0, fmt.Errorf("apple: the disk is full — free space with Disk Utility first")
	}
	return disk, first, count, nil
}

// TempMilliC: de die-temperatuur, via Apple's SMC. Zelfde contract als de Radxa
// (TSADC-register) en de Pi (firmware-mailbox); alleen de weg ernaartoe is hier
// een coprocessor aan dezelfde RTKit-bus als de opslag. 0 = onbekend, en dat mag:
// HOP behandelt een board zonder thermometer als een board zonder thermometer.
//
// De bedrading staat hier en niet in de board-basis, omdat de basis geen drivers
// mag importeren (docs/archief/indeling.md, bewaakt door tools/importcheck.go):
// silicium-kennis onder, driverkeuze boven.
//
// WELKE SLEUTEL de temperatuur draagt, staat nergens gedocumenteerd en
// verschilt per machine. Daarom raden we niet maar zoeken we één keer: eerst de
// namen die Apple op deze generatie gebruikt, en anders de warmste sleutel die
// met 'T' begint. Wat we vinden onthouden we — daarna is het één vraag per
// meting.
var (
	smcOnce sync.Once
	smcDev  *smc.Dev
	smcKey  uint32
)

func (machine) TempMilliC() int {
	smcOnce.Do(func() {
		// UIT, tenzij hopos.smc=1 — en NIET omdat dit blok gevaarlijk is.
		//
		// Dat dachten we 31-08 wel: twee pogingen eindigden met een node die na
		// ~2 minuten ophield (1m48s stil, of een herstart rond 2m20s), en dat
		// leek een SMC die je niet half moet achterlaten. Het was de watchdog van
		// de firmware, die in diezelfde periode élke boot omlegde rond 1:43, met
		// of zonder SMC (board/apple/wdt.go). Met de watchdog stil haalde de
		// agent MÉT hopos.smc=1 onder m1n1 gewoon 220s: dit blok zet de machine
		// niet vast.
		//
		// Wat wel waar blijft: het gesprek komt niet af. Het INITIALIZE-antwoord
		// met het shmem-adres komt nooit, dus er is ook geen temperatuur te
		// lezen. Default uit is daarom geen veiligheidsmaatregel maar
		// bring-up-hygiëne: één wijziging per installatie, zodat een natieve boot
		// die stukgaat maar één verdachte heeft. Experimenteren doe je met
		// hopos.smc=1, bij voorkeur onder m1n1 — daar kost een mislukking geen
		// reis naar 1TR.
		if apple.BootParam("hopos.smc") != "1" {
			fmt.Printf("smc: not started (set hopos.smc=1 to try) — no die temperature on this node\n")
			return
		}
		base := apple.ADTReg("/arm-io/smc", 0)
		if base == 0 {
			return
		}
		d, err := smc.Open(uintptr(base), apple.StorageBuf)
		if err != nil {
			fmt.Printf("smc: %v — no die temperature on this node\n", err)
			return
		}
		smcDev = d
		for _, k := range []string{"TC0P", "Tp09", "Tp0T", "TG0D"} {
			if _, err := d.Float(smc.Key(k)); err == nil {
				smcKey = smc.Key(k)
				return
			}
		}
		if hot, key := smc.Hottest(d); key != 0 {
			smcKey = key
			_ = hot
		}
	})
	if smcDev == nil || smcKey == 0 {
		return 0
	}
	c, err := smcDev.Float(smcKey)
	if err != nil {
		return 0
	}
	return int(c * 1000)
}

// Framebuffer: GEEN, en dat is een meting en geen aanname.
//
// De firmware laat wél een buffer achter (boot_args draagt 0x105e5304000,
// 640x1136, stride 2560, 32 bpp na maskering van de vlaggen) en onze stores
// landen er aantoonbaar in: kleurbalken geschilderd, middelste pixel
// teruggelezen, waarde klopte. Alleen scant niemand hem uit — met een
// HDMI-kabel vanaf de power-on bleef het scherm zwart, en zonder signaal.
// iBoot brengt de display-coprocessor op dit pad helemaal niet op
// (DISPEXT0_CPU heeft PS_ACTUAL 0), en beeld zou de hele DCP-keten vragen:
// dptx-phy is aanwezig, dus dat is het M2-Pro/Max-patroon — ~1500-2000 regels
// Go, op een chip die upstream nog niemand aan de praat heeft.
//
// Een buffer die niemand ziet is geen beeld maar een risico. Hij kostte ons
// 30-08 een middag: de console spiegelde erheen, en het venster als Normal-NC
// mappen nam ruim een megabyte firmware-geheugen mee (de buffer ligt niet op
// een 2MB-grens) — de node herstartte daarna om de paar minuten. Die fout zit
// nu dicht in cpu/memattr, maar de opbrengst blijft nul. Dus: uit.
//
// apple.FB() blijft bestaan als meting; zet dit op FB() zodra er ooit een
// firmware of een DCP-driver is die het scherm wél aanzet.
func (machine) Framebuffer() (fb.Desc, bool) { return fb.Desc{}, false }
