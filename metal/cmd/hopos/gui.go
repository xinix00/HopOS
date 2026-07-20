//go:build gui

// gui.go — de gui-smaak van de agent: linkt het display-vlak (metal/gui) mee
// en registreert gui/fbgrant als grant-provider bij kern/slots (cmd is het
// knooppunt, indeling.md regel 4). Elke board-tag heeft zo twee builds: kaal
// en `-tags gui` (elk imagescript heeft de knop: default gui, GUI=0 = kaal);
// de kale build heeft geen debug-listener, geen display-code en geeft het
// glas nooit weg. De fb-cónsole is géén gui — die blijft in de basis.
package main

import (
	"hop-os/metal/gui/debug"
	"hop-os/metal/gui/fbgrant"
	"hop-os/metal/kern/slots"
)

func init() {
	slots.RegisterGrant(slots.GrantHooks{Env: fbgrant.Env, Arm: fbgrant.Arm, Release: fbgrant.Release})
}

func startDebug() { debug.Start() }
