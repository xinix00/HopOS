//go:build tamago && arm64

package applib

// parkExit doet een HVC → HopOS' EL2-parkeerpad (zie park_arm64.s).
func parkExit() { hvcExit() }

func hvcExit()
