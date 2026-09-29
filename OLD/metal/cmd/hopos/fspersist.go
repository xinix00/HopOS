package main

import (
	"fmt"
	"time"

	"github.com/xinix00/HopOS/metal/v2/kern/hopfs"
	"github.com/xinix00/HopOS/metal/v2/kern/kernflip"
	"github.com/xinix00/HopOS/metal/v2/kern/slots"
)

// Stateless of stateful — de keuze voor een KOUDE boot (hopos.storage):
//
//   - stateless (default): een koude boot begint met een lege schijf. Wat er
//     stond is niet meer vindbaar (beide plekken van de boom gewist), dus ook
//     niets wat zich op de node genesteld had overleeft een reboot.
//   - stateful: een koude boot laadt de laatst vastgelegde boom; die wordt
//     dan ook elke fsCommitEvery vastgelegd, zodat een stroomuitval hoogstens
//     zoveel kost.
//
// Een FLIP houdt de boom altijd, ongeacht de keuze: de apps draaien door en
// kunnen niet ineens zonder hun data zitten. Een flip die in zijn boot sterft
// en door de watchdog koud herstart, valt dus wél onder de keuze.
func storageStateful() bool { return bootParam("hopos.storage") == "stateful" }

// storageWipe: hopos.nvmewipe geldt alleen op een koude boot. Op een flip zou
// een vergeten config-regel de lopende apps hun volumes afpakken.
func storageWipe(isFlip bool) bool {
	return !isFlip && (bootParam("hopos.nvmewipe") == "1" || DefaultNVMeWipe == "1")
}

// storageFresh: begint deze boot leeg?
func storageFresh(isFlip, wipe bool) bool {
	return wipe || (!isFlip && !storageStateful())
}

// fsCommitEvery is de grens van wat een harde stroomuitval kost op een
// stateful node: zoveel seconden aan nieuwe namen en groottes. De data zelf
// staat er al; het is de boom die haar terugvindt.
const fsCommitEvery = 10 * time.Second

// useFS zet een gemounte hopfs in dienst: de slots krijgen hem, de console
// zegt wat er gevonden is, en de boom wordt vastgelegd — bij een flip altijd
// vlak vóór de sprong (kernflip.UseStorage), op een stateful node ook
// periodiek.
func useFS(fsys *hopfs.FS, found string, isFlip bool) {
	slots.UseFS(fsys)
	mode := "stateless: a cold boot starts empty, a flip keeps the volumes"
	if storageStateful() {
		mode = "stateful: the volumes survive a flip and a reboot"
	}
	fmt.Printf("storage: %s (hopos.storage) - %s\n", mode, found)
	if !fsys.Persistent() {
		return
	}
	if storageStateful() {
		go fsys.CommitEvery(fsCommitEvery)
	}
	kernflip.UseStorage(func() (func(), error) {
		thaw, gen, err := fsys.Freeze()
		if err == nil {
			fmt.Printf("storage: tree saved as generation %d and held for the flip\n", gen)
		}
		return thaw, err
	})
}
