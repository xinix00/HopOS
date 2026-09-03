//go:build gui

// gui.go — de gui-smaak van de agent: registreert gui/fbgrant als
// grant-provider bij kern/slots (cmd is het knooppunt, indeling.md regel 4).
// Elke board-tag heeft zo twee builds: kaal en `-tags gui` (elk imagescript
// heeft de knop: default gui, GUI=0 = kaal); de kale build heeft geen
// display-code en geeft het glas nooit weg — sinds 06-08 klopt dat tot op de
// regel: ook de RK3566-beeldketen (gui/driver/rkscan, bedraad in gui_rk3566.go) en
// de surface-grant (kern/slots achter dezelfde tag) linken kaal niet mee. De
// fb-cónsole is géén gui — die blijft in de basis.
package main

import (
	"github.com/xinix00/HopOS/metal/v2/gui/fbgrant"
	"github.com/xinix00/HopOS/metal/v2/kern/slots"
)

func init() {
	slots.RegisterGrant(slots.GrantHooks{Env: fbgrant.Env, Arm: fbgrant.Arm, Release: fbgrant.Release})
}
