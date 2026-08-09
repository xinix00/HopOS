module github.com/xinix00/HopOS/metal

go 1.26.4

require (
	github.com/usbarmory/go-net v0.0.0-20260626130943-dad9ef39fd9b
	github.com/usbarmory/tamago v1.26.4
)

require github.com/xinix00/lean v0.1.0 // indirect

require (
	github.com/google/btree v1.1.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	// Niet meer door ons geïmporteerd (de lneto-backend is 26-07 gesloopt),
	// maar go-net importeert hem zelf (zijn lneto.go) — dus blijft hij als
	// indirecte dependency in de graaf staan.
	github.com/soypat/lneto v0.1.1-0.20260609173350-82f946154800 // indirect
	github.com/xinix00/hoplockserver v0.1.2 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/time v0.7.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// Directe dep sinds de app-object-store (cmd/hopos/store.go gebruikt de
// streaming object-API + ListObjects van v0.3.0).
require github.com/xinix00/hoplock v0.3.0

require (
	// De gepubliceerde HOP-tag waar deze boom tegen bouwt (o.a.
	// hopos.ErrNoCapacity en StartStaged mét jobnaam). GEEN lokale replace
	// meer (Derek, 04-08): een hop-wijziging bereikt hop-os alleen via
	// commit+tag, dus wat hier bouwt bouwt overal — ook downstream (de
	// -hopos-satellieten), die een replace nooit zouden zien.
	github.com/xinix00/hop v0.20.13
	golang.org/x/crypto/x509roots/fallback v0.0.0-20260709184058-243e02a382f8
	gvisor.dev/gvisor v0.0.0-20250911055229-61a46406f068
)

// De hoplock-modules komen van GitHub (echte versies, geen pad op deze Mac) —
