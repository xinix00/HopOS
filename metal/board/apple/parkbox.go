// park.go — de brievenbus waarmee HOP een core zijn werk geeft.
//
// Er is precies één brievenbus (HopParkArg/PC/For in de boot-scratch) en er
// kan er precies één overdracht tegelijk in liggen. Dat is geen beperking maar
// het hele punt: cores komen uit reset aan bij stubReset, met de MMU uit en
// zonder enige kennis van de wereld, en het enige dat ze over zichzelf weten is
// hun MPIDR. Het adreswoord vertelt wie de entry mag oppakken.
//
// Twee regels maken dat waterdicht, en beide ontbraken (GEMETEN 31-08, de
// eerste keer dat we natief een tweede app-core vroegen):
//
//  1. VRIJ is een eigen waarde, niet "nul". Nul is een geldig adres — het
//     E-cluster begint bij core 0, aff 0x0000 — dus met nul als "leeg" kon de
//     zuinigste core van de SoC nooit werk aannemen én las HOP een bezette bus
//     als vrij. Vrij is nu ^0: een adres dat geen core heeft.
//  2. WACHTEN, niet opgeven. De vorige core bevestigt pas nadat PMGR hem
//     aanzette en de reset losliet. Wie meteen opgeeft bij een bezette bus laat
//     de tweede core stil wegvallen — en dat is erger dan een fout, want de app
//     is dan al verteld dat hij twee harten heeft en valt om in de eerste
//     goroutine die op het ontbrekende hart landt.
package apple

import "github.com/xinix00/HopOS/metal/dev"

// parkFree is "de bus is vrij". Geen enkele core heeft dit als aff1:aff0, en
// het is niet de waarde die een vers image in de scratch achterlaat — parkArm
// zet hem dus expliciet, één keer, vóór de eerste overdracht.
const parkFree = ^uint64(0)

// parkSpins begrenst het wachten. Eén ronde is een cache-onderhoudsoperatie
// plus een lees uit DRAM; twee miljoen daarvan is orde honderd milliseconden en
// daarmee ruim boven de tijd die een core nodig heeft om uit reset te komen en
// zijn ontvangst te bevestigen (dat is werk van microseconden). Het is een
// bovengrens tegen een core die nooit opdaagt, geen verwachte wachttijd.
const parkSpins = 1 << 21

var parkArmed bool

// parkAddr is het adreswoord van een core: aff1:aff0, precies wat de boom per
// core in zijn reg-woord zet en wat stubReset uit zijn eigen MPIDR haalt.
func parkAddr(c CPU) uint64 { return uint64(c.Cluster)<<8 | uint64(c.Core) }

// parkArm zet de bus op vrij. Moet gebeurd zijn vóór er ooit een entry in de
// bus ligt: zolang het adreswoord nog de nul van het image draagt zou een
// geparkeerde E-core 0 zich aangesproken voelen door werk dat niet van hem is.
func parkArm() {
	if parkArmed {
		return
	}
	parkArmed = true
	dev.Write64(HopParkFor, parkFree)
	dev.CleanInv(HopParkFor, 8)
	dev.MB()
}

// parkWait wacht tot de bus vrij is. false = er ligt na parkSpins rondes nog
// altijd een onbevestigde overdracht; de core die hem had aannemen is dan niet
// opgekomen en de aanroeper moet dat luid melden, niet stil doorlopen.
//
// De veeg vóór elke lees is nodig omdat de bevestiging van de andere kant met
// de MMU uit geschreven wordt, dus rechtstreeks naar DRAM: zonder invalidatie
// blijft onze eigen regel het oude adres tonen. Op dat moment is die regel
// schoon (de schrijver veegde hem al), dus clean-and-invalidate schrijft hier
// niets terug over de bevestiging heen.
func parkWait() bool {
	for i := 0; i < parkSpins; i++ {
		dev.CleanInv(HopParkFor, 8)
		if dev.Read64(HopParkFor) == parkFree {
			return true
		}
		dev.MB()
	}
	return false
}
