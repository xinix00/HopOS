//go:build !tamago

package main

import (
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/xinix00/HopOS/apps/cloudflared-lean/internal/origin"
)

// De host-variant bestaat niet als hulpje: dit is waar de tunnel tegen de échte
// edge getest wordt vóór er een boot-cyclus aan een node opgaat. Zelfde
// protocolcode, zelfde proxy — alleen de netstack eronder verschilt.
func main() {
	logf := func(format string, args ...any) { log.Printf(format, args...) }

	stop := make(chan struct{})
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		logf("cloudflared-lean: stopping")
		close(stop)
	}()

	if err := run(os.Getenv, logf, runtime.GOOS+"_"+runtime.GOARCH, origin.Proxy, stop); err != nil {
		log.Printf("cloudflared-lean: %v", err)
		os.Exit(1)
	}
}
