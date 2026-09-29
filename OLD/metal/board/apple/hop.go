// hop.go — waar HopOS woont.
//
// De firmware levert ons af op de core waar iBoot begon, en dat is er een uit
// het snelle cluster (op deze mini cpu 6, MPIDR 0x10100, everest). Dat is de
// verkeerde: HopOS is een toezichthouder die het grootste deel van zijn tijd
// slaapt, en een app die een P-core kan gebruiken hoort hem te krijgen. De
// wissel zelf gebeurt in cpuinit.s, vóór de eerste Go-instructie — daar valt er
// nog niets te verhuizen. Wat hier staat is de nasleep: wie zijn we geworden,
// en hoe krijgt de achtergelaten core alsnog werk.
//
// Zelfde vorm als de loterij op riscv64 (board/licheerv), met m1n1's spin-table
// in plaats van PSCI.
package apple

import "github.com/xinix00/HopOS/metal/v2/dev"

var (
	selfCPU = -2 // -2 = nog niet bepaald, -1 = onbekend
	adopted bool
)

// SelfCPU is de m1n1-cpu-index van de core waar dit draait. Álles wat wil weten
// "welke core is HOP" vraagt het hier en niet aan het param-blok: de boot-cpu
// die de firmware noemt is na de hop een gewone app-core.
//
// Eén keer bepaald, en dat moet op de HOP-core gebeuren — SetupPlan doet dat,
// als eerste board-werk van de boot.
func SelfCPU() int {
	if selfCPU != -2 {
		return selfCPU
	}
	selfCPU = -1
	// Vergelijken op aff1:aff0 — cluster en core, precies wat de boom per core
	// in zijn reg-woord zet. Op deze SoC's is dat paar uniek, en het werkt met
	// én zonder loader.
	me := dev.MPIDR() & 0xFFFF
	for i, c := range CPUs() {
		if uint64(c.Cluster)<<8|uint64(c.Core) == me {
			selfCPU = i
			break
		}
	}
	return selfCPU
}

// ParkedCPU is de core die zich bij de hop parkeerde en op adoptie wacht, of -1
// als er niet gehopt is. Dat is per definitie de boot-cpu: alleen die kan
// zichzelf parkeren, want m1n1 heeft hem niet in zijn spin-table staan.
func ParkedCPU() int {
	p, ok := Params()
	if !ok || p.Hop.Release == 0 || SelfCPU() < 0 || SelfCPU() == p.BootCPU {
		return -1
	}
	return p.BootCPU
}

// Start laat core `cpu` lopen op entry met ctx in x0. Twee soorten cores, één
// contract — ze komen allebei aan op EL2 met de MMU uit:
//
//   - wie m1n1 parkeerde gaat via zijn spin-table (Release);
//   - de core die zichzelf bij de hop parkeerde wacht in ónze lus (cpuinit.s)
//     en wordt hier geadopteerd: eerst het argument, dan de entry, dan een
//     event. Die volgorde is niet vrijblijvend — wie de entry eerst schrijft
//     laat de core met een argument van nul vertrekken.
func Start(cpu int, entry, ctx uint64) bool {
	// Zijn de cores van ons — RVBAR wijst naar onze eigen stubReset — dan is er
	// geen tussenpersoon: we zetten hem aan via het PMGR-blok en geven hem zijn
	// werk via de brievenbus.
	if own, _ := OwnCores(); own {
		return startOwn(cpu, entry, ctx)
	}
	if cpu < 0 || cpu != ParkedCPU() {
		return Release(cpu, entry, ctx)
	}
	if adopted {
		return false
	}
	cpus := CPUs()
	if cpu >= len(cpus) {
		return false
	}
	adopted = true
	released[cpu] = true
	parkArm()
	dev.Write64(HopParkArg, ctx)
	dev.Write64(HopParkPC, entry)
	// De regel naar geheugen: wij schrijven met de MMU aan (en dit stuk valt
	// binnen HOP's eigen RAM, dus gecachet), de wachtende core leest met de MMU
	// uit. Zonder deze veeg blijft de entry in ónze cache hangen en wacht hij
	// eeuwig.
	dev.CleanInv(HopParkArg, 8)
	dev.CleanInv(HopParkPC, 8)
	dev.MB()
	// En als laatste het adreswoord: dát is het startsein, en het zegt de
	// wachtende core dat deze entry van hém is (park.go).
	dev.Write64(HopParkFor, parkAddr(cpus[cpu]))
	dev.CleanInv(HopParkFor, 8)
	dev.SEV()
	return true
}

// ownStarted: welke cores wij al uit reset haalden. Een tweede StartCore op een
// lopende core is een reset, niet een start.
var ownStarted [MaxCPUs]bool

// startOwn start een core zonder tussenpersoon: uit reset halen met het
// PMGR-blok, en hem zijn entry geven via de MPIDR-brievenbus waar stubReset op
// wacht. De volgorde is dwingend — argument, entry, en pas als laatste vóór wie
// het bedoeld is: dat laatste woord ís het startsein.
//
// NOG NIET OP IJZER: dit pad kan pas lopen als ons image het bootobject is, en
// tot die installatie meldt OwnCores() eerlijk `false` en komt hier niemand.
func startOwn(cpu int, entry, ctx uint64) bool {
	cpus := CPUs()
	if cpu < 0 || cpu >= len(cpus) || entry == 0 {
		return false
	}
	parkArm()
	// Loopt er nog een overdracht, dan is de brievenbus bezet — en dán wachten
	// we, want de vorige core bevestigt pas nadat PMGR hem aanzette en de reset
	// losliet. Meteen opgeven (wat hier stond) was precies de fout: een app die
	// twee cores vraagt krijgt zijn Start-oproepen vlak achter elkaar, de tweede
	// zag de bus nog bezet, en de app kreeg te horen dat hij twee harten had
	// terwijl er één kwam opdagen — hij viel om in de eerste goroutine die op
	// dat tweede hart hoorde te landen (GEMETEN 31-08: 1024 shares = één core =
	// stabiel, 2048 = twee cores = crashlus).
	if !parkWait() {
		return false
	}
	c := cpus[cpu]
	dev.Write64(HopParkArg, ctx)
	dev.Write64(HopParkPC, entry)
	dev.CleanInv(HopParkArg, 8)
	dev.CleanInv(HopParkPC, 8)
	dev.MB()
	dev.Write64(HopParkFor, parkAddr(c))
	dev.CleanInv(HopParkFor, 8)
	dev.SEV()

	if !ownStarted[cpu] {
		if !StartCore(c) {
			return false
		}
		ownStarted[cpu] = true
	}
	released[cpu] = true
	return true
}

// HopStatus is één regel voor de console: waar HOP terechtkwam en wat er met de
// achtergelaten core gebeurde. Een mislukte wissel is geen fout — cpuinit redt
// zichzelf dan — maar je wilt het wél zien staan.
func HopStatus() string {
	p, ok := Params()
	if !ok {
		return ""
	}
	if p.Hop.Release == 0 {
		return "hop: no efficiency core offered by the loader — HOP stays on the boot core"
	}
	if ParkedCPU() < 0 {
		return "WARNING HOPOS_HOP_FAILED: the efficiency core never came up — HOP runs on the boot core and one core stays idle"
	}
	return "hop: HOP moved to cpu " + itoa(SelfCPU()) + " (efficiency cluster); cpu " +
		itoa(p.BootCPU) + " parked and joins the app cores — HOPOS_HOP_DONE"
}

// itoa: één cijfergroep zonder strconv (dit pakket is board-basis en draait
// vóór de meeste runtime-luxe).
func itoa(n int) string {
	if n < 0 {
		return "?"
	}
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa(n/10) + string(rune('0'+n%10))
}
