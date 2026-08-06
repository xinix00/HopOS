//go:build gui

// gui.go — de gui-smaak van de embed-mains: zelfde fbgrant-bedrading als
// cmd/hopos/gui.go (cmd is het knooppunt), zodat `-tags gui` op élke
// HOP-binary hetzelfde betekent. Geen debug-listener hier: de embed-mains
// zijn de P1-demo/regressiekernen, geen agents. De RK3566-scanout
// (gui/driver/rkscan, zie cmd/hopos/gui_rk3566.go) wordt hier bewust níét bedraad:
// de embed-mains bewijzen de kooi, niet het glas — Framebuffer() zegt dan
// false en de embed-run blijft UART-only.
package main

import (
	"github.com/xinix00/HopOS/metal/gui/fbgrant"
	"github.com/xinix00/HopOS/metal/kern/slots"
)

func init() {
	slots.RegisterGrant(slots.GrantHooks{Env: fbgrant.Env, Arm: fbgrant.Arm, Release: fbgrant.Release})
}
