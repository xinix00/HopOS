// Hello is the smallest possible HopOS app — the starting point that
// docs/app.md builds on (English on purpose: this file is documentation).
// applib handles the node handshake (READY, heartbeats, the kill flag);
// main only has to do its own work.
package main

import (
	"time"

	"hop-os/metal/app/applib"
)

func main() {
	app := applib.Init()
	app.Logf("hello from slot %d — %d MB RAM", app.Slot, app.RAMSize>>20)
	for i := 1; ; i++ {
		app.Logf("alive: round %d", i)
		time.Sleep(10 * time.Second)
	}
}
