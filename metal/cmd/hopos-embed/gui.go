//go:build gui

// gui.go — de gui-smaak van de embed-mains: zelfde fbgrant-bedrading als
// cmd/hopos/gui.go (cmd is het knooppunt), zodat `-tags gui` op élke
// HOP-binary hetzelfde betekent. Geen debug-listener hier: de embed-mains
// zijn de P1-demo/regressiekernen, geen agents.
package main

import (
	"github.com/xinix00/HopOS/metal/gui/fbgrant"
	"github.com/xinix00/HopOS/metal/kern/slots"
)

func init() {
	slots.RegisterGrant(slots.GrantHooks{Env: fbgrant.Env, Arm: fbgrant.Arm, Release: fbgrant.Release})
}
