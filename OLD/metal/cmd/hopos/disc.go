//go:build media

package main

// De optische drives aan deze node: wie ze vindt, hoe ze heten in /devices, en
// de vraag `disc` op de consolepoort.
//
// De scanner van usbin is de enige eigenaar van de USB-bus, dus die geeft een
// opslagapparaat hier af (usbin.Storage) als handvat: elk commando aan de drive
// gaat als verzoek naar die eigenaar. Wat er dan gebeurt is bewust weinig: de
// drive wordt geopend, krijgt een naam in de namespace van de node
// (/devices/disc0, disc1, ...) en zegt één keer op de console wie hij is en
// waar hij hangt. Lezen is daarna werk van een app, die hem mount als elk
// ander volume. Trekt iemand hem eruit, dan verdwijnt hij weer uit /devices.
//
// Alleen in de media-smaak (gui + codec + disc; -tags "gui media"). Die smaak
// draagt de gui altijd mee, en dat is nodig omdat usbin daar woont; dat is
// historie (USB kwam binnen als toetsenbord) en geen ontwerp. Zodra er een node
// is die opslag wil zonder scherm, hoort de USB-stack een laag te zakken.

import (
	"errors"
	"fmt"
	"sync"

	"github.com/xinix00/HopOS/metal/v2/gui/usbin"
	"github.com/xinix00/HopOS/metal/v2/kern/slots"
	"github.com/xinix00/HopOS/metal/v2/media/driver/optical"
)

// disc is één drive zoals deze node hem kent: de naam in /devices, waar hij
// fysiek hangt en wie hij is. Genoeg om `disc0` uit een jobspec terug te
// vinden op een poort aan de achterkant.
type disc struct {
	name  string         // disc0, disc1, ...
	id    string         // vendor:product
	where string         // controller en poort
	drive *optical.Drive // nil: er hing hier een drive, maar hij is eruit
}

var (
	discMu sync.Mutex
	discs  []disc
)

// startDisc hangt de opslag-haak op. Vóór usbin.Start, want een drive die al
// in de poort zit wordt tijdens de eerste scan gevonden.
func startDisc() {
	usbin.Storage = func(b *usbin.Bulk) {
		go attachDisc(b)
	}
}

// attachDisc opent de drive, geeft hem een naam en zegt wat hij ziet. In een
// eigen goroutine: de haak draait op de goroutine van de bus, en elk commando
// aan de drive is een verzoek aan diezelfde goroutine.
func attachDisc(b *usbin.Bulk) {
	id := fmt.Sprintf("%04x:%04x", b.VendorID, b.ProductID)
	drive, err := optical.Open(b)
	if err != nil {
		if errors.Is(err, optical.ErrNotOptical) {
			// Een USB-stick meldt zich met precies hetzelfde interface-drietal,
			// en de bootstick van deze node is er zo één. Daar zitten we niet aan.
			fmt.Printf("disc: %s is mass storage but not an optical drive — left alone\n", id)
			return
		}
		fmt.Printf("disc: %s: %v\n", id, err)
		return
	}
	where := fmt.Sprintf("%s port %d", b.Host, b.Port)

	// Een naam hoort bij een PLEK, niet bij een aanmelding. Een drive die
	// opnieuw enumereert (herplug, of een controller die zichzelf herstelt)
	// moet dezelfde disc0 blijven, anders wijst de mount in een jobspec ineens
	// nergens meer heen. Gemeten 22-09: na een flip kwam dezelfde drive als
	// disc1 terug en las de share nul bytes.
	discMu.Lock()
	name := ""
	for i := range discs {
		if discs[i].where == where {
			name = discs[i].name
			discs[i].id, discs[i].drive = id, drive
			break
		}
	}
	if name == "" {
		name = slots.NextDeviceName("disc")
		discs = append(discs, disc{name: name, id: id, where: where, drive: drive})
	}
	slots.AddDevice(name, drive)
	discMu.Unlock()
	go detachDisc(name, drive, b.Gone())

	fmt.Printf("disc: %s = %s (%s) on %s, %d KB per transfer, mount %s HOPOS_DISC_UP\n",
		name, drive.Name(), id, where, b.MaxTransfer()>>10, slots.DevicePath(name))
	fmt.Printf("disc: %s: %s\n", name, discState(drive))
	discSpeed(name, drive)
}

// detachDisc haalt een drive uit /devices zodra zijn apparaat weg is. De naam
// blijft bij de plek, zodat dezelfde poort weer disc0 wordt; alleen als daar
// intussen al een nieuwe drive hangt (snel herpluggen) blijft alles staan.
func detachDisc(name string, drive *optical.Drive, gone <-chan struct{}) {
	<-gone
	discMu.Lock()
	defer discMu.Unlock()
	for i := range discs {
		if discs[i].name == name && discs[i].drive == drive {
			discs[i].drive = nil
			slots.RemoveDevice(name)
			fmt.Printf("disc: %s is gone (%s)\n", name, discs[i].where)
		}
	}
}

// discSpeed vraagt de drive om volle snelheid en zegt of hij dat aannam. Bij
// elke disc opnieuw: een lade die opengaat zet de snelheid terug op de
// instapstand van de drive.
func discSpeed(name string, drive *optical.Drive) {
	if err := drive.SetSpeed(optical.SpeedMax); err != nil {
		fmt.Printf("disc: %s stays at its default speed (%v)\n", name, err)
		return
	}
	fmt.Printf("disc: %s set to maximum read speed\n", name)
}

// discState is de ene regel die zegt wat er in een drive ligt. Ook het antwoord
// op de console-vraag, zodat er maar één plek is die dit formuleert.
func discState(drive *optical.Drive) string {
	size, err := drive.Size()
	if err != nil {
		return fmt.Sprintf("no disc (%v)", err)
	}
	profile, err := drive.Profile()
	if err != nil {
		return fmt.Sprintf("disc of %d MB, but the drive would not say what it is: %v", size>>20, err)
	}
	return fmt.Sprintf("%s, %d MB", optical.ProfileName(profile), size>>20)
}

// discQuery beantwoordt `printf 'disc\n' | nc node 5555`: wat hangt eraan, hoe
// heet het in /devices, en wat ligt erin. Leest elke drive opnieuw uit, want
// een lade gaat open en dicht zonder dat iemand het hier meldt.
func discQuery() string {
	discMu.Lock()
	list := append([]disc(nil), discs...)
	discMu.Unlock()
	if len(list) == 0 {
		return "disc: no optical drive on this node"
	}
	out := ""
	for _, c := range list {
		if out != "" {
			out += "\n"
		}
		if c.drive == nil {
			out += fmt.Sprintf("disc: %s on %s — unplugged", slots.DevicePath(c.name), c.where)
			continue
		}
		out += fmt.Sprintf("disc: %s (%s) = %s on %s — %s",
			slots.DevicePath(c.name), c.id, c.drive.Name(), c.where, discState(c.drive))
	}
	return out
}
