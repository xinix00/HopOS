// Hello is the smallest realistic HopOS app — the starting point that
// docs/app.md builds on (English on purpose: this file is documentation).
// applib handles the node handshake (READY, heartbeats, the kill flag);
// appnet brings up the app's own network stack. main only does the work.
package main

import (
	"fmt"
	"net/http"

	"hop-os/metal/app/applib"
	"hop-os/metal/app/applib/appnet"
)

func main() {
	app := applib.Init()

	ip, err := appnet.Up(app) // the app's own TCP/IP stack, own IP
	if err != nil {
		app.Logf("net: %v", err)
		app.Exit(1)
	}

	port := app.Env("ER_PORT_HTTP") // published port from the job spec
	if port == "" {
		port = "8080"
	}
	app.Logf("hello: serving on %s:%s", ip, port)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from HopOS — slot %d\n", app.Slot)
	})
	app.Logf("http: %v", http.ListenAndServe(":"+port, nil))
	app.Exit(1) // a service that stops serving is a crash, by design
}
