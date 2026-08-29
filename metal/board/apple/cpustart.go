// cpustart.go — cores starten en stoppen zonder tussenpersoon.
//
// Op Apple silicon is er geen PSCI en geen spin-table van de firmware: een core
// start je door twee bits in het PMGR-blok te zetten, en hij komt uit reset op
// het adres dat in zijn RVBAR staat. Dat is de hele mechaniek — drie
// schrijfacties, en voor deze generatie zelfs zonder chicken bits (m1n1's tabel
// heeft `init = NULL` voor beide M4-core-typen).
//
// WAAROM DIT NOG NIET IN GEBRUIK IS: RVBAR wordt door iBoot gezet én
// vergrendeld. Gemeten 29-08 op alle tien de cores van deze mini:
//
//	cpu0 impl 0x0210050000 → 0x00100100025a8001  lock=1  rvbar=0x100025a8000
//	                                                     ↑ m1n1's laadadres
//
// Zolang m1n1 het bootobject is, landt élke core die wij hier starten dus in
// ZIJN vectoren — niet in de onze. Dat is precies waarom een eigen bootobject
// geen luxe is maar de voorwaarde: pas dan wijst RVBAR naar ons, en pas dan
// zijn de cores van ons. Wat hier staat is de helft die nu al klopt.
package apple

import "github.com/xinix00/HopOS/metal/dev"

// Het PMGR-startblok (m1n1 src/smp.c). Per die een eigen kopie; de M4-mini
// heeft er één.
const (
	pmgrStop   = 0x0 // bit per (4*cluster + core): stoppen
	pmgrEnable = 0x4 // bit per (4*cluster + core): systeemkant aanzetten —
	//                    zonder dit werken de interrupts van die core niet
	pmgrStart    = 0x8          // + 4*cluster: bit per core: lopen
	pmgrDieStep  = 0x2000000000 // m1n1 PMGR_DIE_OFFSET; de M4-mini heeft één die
	rvbarLock    = 1 << 0
	rvbarAddrMsk = 0x0000FFFFFFFFF000
)

// RVBAR geeft het resetadres van een core en of het vergrendeld is. Dit is de
// vraag "wiens core is dit eigenlijk": wijst het naar ons image, dan kunnen we
// hem hebben.
func RVBAR(c CPU) (addr uint64, locked bool, ok bool) {
	if c.Impl == 0 {
		return 0, false, false
	}
	v := uint64(dev.Read32(uintptr(c.Impl))) | uint64(dev.Read32(uintptr(c.Impl)+4))<<32
	return v & rvbarAddrMsk, v&rvbarLock != 0, true
}

// StartCore zet een core aan. Hij begint te lopen op het adres in zijn RVBAR;
// wat daar staat is niet aan ons zolang de firmware dat vergrendelt.
func StartCore(c CPU) bool {
	base := PMGRCPUStart()
	if base == 0 || c.Cluster > 7 || c.Core > 7 {
		return false
	}
	b := uintptr(base) + uintptr(c.Die)*pmgrDieStep
	// Eerst de systeemkant, dan pas lopen — die volgorde staat in m1n1 met de
	// aantekening "zonder dit werken de IRQ's niet".
	dev.Write32(b+pmgrEnable, 1<<(4*c.Cluster+c.Core))
	dev.MB()
	dev.Write32(b+pmgrStart+uintptr(c.Cluster)*4, 1<<c.Core)
	dev.MB()
	return true
}

// StopCore zet een core stil. Nodig vóór een herstart, en de enige nette manier
// om er een terug te geven.
func StopCore(c CPU) bool {
	base := PMGRCPUStart()
	if base == 0 || c.Cluster > 7 || c.Core > 7 {
		return false
	}
	b := uintptr(base) + uintptr(c.Die)*pmgrDieStep
	dev.Write32(b+pmgrStop, 1<<(4*c.Cluster+c.Core))
	dev.MB()
	return true
}

// OwnCores meldt of de cores van ons zijn: wijst hun resetadres naar het begin
// van ONS bootobject, dan landen ze in stubReset en kunnen we ze zelf starten.
// Zo niet, dan zegt deze functie precies waarom niet.
//
// De maatstaf is StubSource — het adres waar de firmware ons neerzette, door de
// stub opgeschreven vlak voor hij naar de kern sprong. Niet RamBase: dáár draait
// het image, maar RVBAR wijst naar waar het gelád werd, en dat kiest iBoot.
func OwnCores() (bool, string) {
	cpus := CPUs()
	if len(cpus) == 0 {
		return false, "geen core-lijst in de ADT"
	}
	addr, locked, ok := RVBAR(cpus[0])
	if !ok {
		return false, "geen cpu-impl-reg in de ADT"
	}
	src := StubSource()
	if src == 0 {
		return false, "geen bootstub gedraaid — onbekend waar dit image geladen is"
	}
	if addr == src {
		return true, ""
	}
	if locked {
		return false, "RVBAR staat vergrendeld op een ander bootobject"
	}
	return false, "RVBAR wijst niet naar ons image"
}
