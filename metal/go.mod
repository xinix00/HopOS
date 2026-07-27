module hop-os/metal

go 1.26.4

require (
	github.com/usbarmory/go-net v0.0.0-20260626130943-dad9ef39fd9b
	github.com/usbarmory/tamago v1.26.4
)

require (
	github.com/google/btree v1.1.2 // indirect
	// Niet meer door ons geïmporteerd (de lneto-backend is 26-07 gesloopt),
	// maar go-net importeert hem zelf (zijn lneto.go) — dus blijft hij als
	// indirecte dependency in de graaf staan.
	github.com/soypat/lneto v0.1.1-0.20260609173350-82f946154800 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/xinix00/hoplock v0.2.0 // indirect
	github.com/xinix00/hoplockserver v0.1.1 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/time v0.7.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

require (
	golang.org/x/crypto/x509roots/fallback v0.0.0-20260709184058-243e02a382f8
	gvisor.dev/gvisor v0.0.0-20250911055229-61a46406f068
	// Het versienummer documenteert de pairing (deze hop-os bouwt tegen HOP
	// v0.20.1, o.a. hopos.ErrNoCapacity); de replace hieronder blijft leidend.
	hop v0.20.1
)

// De hoplock-modules komen van GitHub (echte versies, geen pad op deze Mac) —
// zo bouwt een verse clone ook bij iemand anders. Blijft één lokale replace
// over: de hop-module heet intern `module hop` (geen URL), dus die kan pas naar
// GitHub als hij daar hernoemd is naar github.com/xinix00/hop — dat verandert
// het publieke importpad van hop zelf en hoort in díé repo (mét release), niet
// hier.
replace hop => /Users/derek/Git/easy/hop
