// switchtest is het regressienet voor de hart-wissel: twee bewoners die niets
// anders doen dan yielden en daarna narekenen dat ze zichzélf terugkregen.
//
// Waarom dit bestaat. De twee bugs die multi-app op RISC-V blokkeerden (31-07)
// waren beide alleen zichtbaar mét twee bewoners, en beide kwamen aan het licht
// via een apploader die eerst een netstack opzette en 29MB over TLS trok. Dat is
// een debug-cyclus van minuten per poging, met vijf herstarts en een
// console-regel als enige uitkomst. Deze app haalt alles weg wat niet de wissel
// is: geen netwerk, geen TLS, geen download, geen allocatie in de hete lus.
// Wat overblijft faalt binnen seconden en zégt wat er stuk is.
//
// Wat hij bewijst, in de vorm waarin het misging:
//
//   - **de stack overleeft** — een buffer vol canary's die de yield omspant. De
//     G-vlag op de slot-PTE's liet een bewoner de stack van zijn voorganger
//     lezen; dan staat hier de canary van de ánder.
//   - **het retouradres overleeft** — de yield gebeurt onderin een call-keten en
//     elke laag controleert zijn eigen frame op de terugweg. De yield-stub spilde
//     een FP-register over het bewaarde ra; dan komt de return nooit hier terug
//     (of met een verminkte waarde).
//   - **de registers overleven** — een handvol waarden die de switcher moet
//     bewaren en herstellen, nagerekend ná de wissel.
//
// Bouwen en draaien (bench, zonder de release aan te raken):
//
//	cd metal && GOWORK=off GOTOOLCHAIN=local GOOS=tamago \
//	  GOOSPKG=github.com/usbarmory/tamago GOARCH=riscv64 $HOME/tamago-go/bin/go \
//	  build -tags "linkramsize linkcpuinit" -trimpath \
//	  -ldflags "-w -T $((0x88000000 + 0x10000)) -R 0x1000" \
//	  -o ~/httpd/switchtest.elf ./app/switchtest
//
// Twee bewoners = twee jobs op dezelfde sharegroup, hetzelfde artifact:
//
//	hopos.init[]={"name":"sw-a","driver":"hop","artifacts":[{"url":"http://192.168.2.1:8000/switchtest.elf","match":{"node.arch":"riscv64"}}],"memory_limit":16777216,"tags":{"sharegroup":"lrv"}}
//	hopos.init[]={"name":"sw-b", ... zelfde, ander name}
//
// Elke bewoner meldt zijn voortgang per 100.000 rondes. Blijven beide tellen
// oplopen, dan wisselt het hart correct; een fout meldt zich als één regel met
// de gemeten en de verwachte waarde, en daarna stopt de app met een exitcode.
package main

import (
	"fmt"
	"runtime"
	"time"

	"github.com/xinix00/HopOS/metal/v2/app/applib"
)

// reportEvery is hoe vaak een bewoner van zich laat horen. Hoog genoeg dat de
// console niet het meetinstrument wordt (op dit board kost een regel meer
// buffering dan de NIC-ring heeft), laag genoeg om binnen seconden te zien of
// het loopt.
const reportEvery = 100_000

// depth is hoe diep de yield in de call-keten zit. Vier frames is genoeg om een
// verminkt retouradres te laten opvallen en houdt de lus goedkoop; het punt is
// dát er frames tussen zitten, niet hoeveel.
const depth = 4

func main() {
	app := applib.Init()

	// De canary hangt aan het slotnummer: zo is "ik las de stack van mijn
	// buurman" niet een vage mismatch maar een aanwijsbare dader.
	canary := 0x5EED000000000000 | uint64(app.Slot)<<32 | 0xC0DE
	app.Logf("switchtest: slot %d, canary %#x, %d MB RAM — yielden tot iemand iets kapot vindt",
		app.Slot, canary, app.RAMSize>>20)

	for round := uint64(1); ; round++ {
		if err := descend(depth, canary); err != "" {
			app.Logf("switchtest: SLOT %d RONDE %d KAPOT — %s", app.Slot, round, err)
			app.Exit(1)
		}
		if round%reportEvery == 0 {
			app.Logf("switchtest: slot %d — %d rondes correct", app.Slot, round)
		}
	}
}

// descend legt frames op de stack en yieldt onderin. Elke laag zet zijn eigen
// canary in een lokale buffer en controleert die ná de terugkeer, dus een frame
// dat door de wissel is aangetast valt op de plek op waar het gebeurde.
//
// go:noinline op elke laag zou netter zijn, maar de recursie met een
// runtime-waarde als teller houdt de compiler al van inlinen af — en één
// mechanisme is beter dan twee.
func descend(n int, canary uint64) string {
	var frame [32]uint64
	for i := range frame {
		frame[i] = canary ^ uint64(n)
	}
	var err string
	if n > 0 {
		err = descend(n-1, canary)
	} else {
		err = yieldOnce(canary)
	}
	if err != "" {
		return err
	}
	// Ons eigen frame moet er ongeschonden zijn. Dit is de test op "de stack
	// overleeft de wissel" — en op "het frame van de ander is niet het mijne".
	for i := range frame {
		if frame[i] != canary^uint64(n) {
			return badFrame(n, i, frame[i], canary^uint64(n))
		}
	}
	return ""
}

// yieldOnce geeft het hart één keer af en rekent daarna na dat er niets van de
// buurman is achtergebleven. De yield loopt via de idle-governor: er is niets
// anders te doen, dus de scheduler valt idle en die trapt naar HOP's switcher —
// exact het productiepad, niet een eigen achterdeur ernaartoe.
func yieldOnce(canary uint64) string {
	a, b, c, d := canary, canary^1, canary^2, canary^3
	var heap = make([]uint64, 8) // buiten de lus zou goedkoper zijn, maar dít
	for i := range heap {        // toetst ook of de heap van de buurman wegblijft
		heap[i] = canary ^ uint64(i)
	}
	runtime.Gosched()
	time.Sleep(time.Microsecond) // dwingt de scheduler écht naar idle → yield
	switch {
	case a != canary:
		return badReg("a", a, canary)
	case b != canary^1:
		return badReg("b", b, canary^1)
	case c != canary^2:
		return badReg("c", c, canary^2)
	case d != canary^3:
		return badReg("d", d, canary^3)
	}
	for i := range heap {
		if heap[i] != canary^uint64(i) {
			return badFrame(-1, i, heap[i], canary^uint64(i))
		}
	}
	return ""
}

// badFrame/badReg bouwen de foutregel. Aparte functies zodat de hete lus geen
// formatteercode bevat: op de blije weg wordt hier nooit iets aangeroepen.
func badFrame(depth, idx int, got, want uint64) string {
	return fmt.Sprintf("frame %d[%d] = %#x, wil %#x (canary van slot %d?)",
		depth, idx, got, want, int(got>>32&0xFFFF))
}

func badReg(name string, got, want uint64) string {
	return fmt.Sprintf("register %s = %#x, wil %#x", name, got, want)
}
