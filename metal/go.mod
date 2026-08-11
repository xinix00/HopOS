module github.com/xinix00/HopOS/metal

go 1.26.4

require (
	github.com/usbarmory/tamago v1.26.4
	github.com/xinix00/go-net v0.1.1-hopos.1
)

require github.com/xinix00/lean v0.1.0

require (
	github.com/google/btree v1.1.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/xinix00/hoplockserver v0.1.2 // indirect
	// Niet meer door ons geïmporteerd (de lneto-backend is 26-07 gesloopt),
	// maar go-net importeert hem zelf (zijn lneto.go) — dus blijft hij als
	// indirecte dependency in de graaf staan.
	github.com/xinix00/lneto v0.4.0-hopos.1 // indirect
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
	github.com/xinix00/hop v0.20.13-testing.4
	golang.org/x/crypto/x509roots/fallback v0.0.0-20260709184058-243e02a382f8
	gvisor.dev/gvisor v0.0.0-20250911055229-61a46406f068 // indirect
)

// De hoplock-modules komen van GitHub (echte versies, geen pad op deze Mac) —
// en sinds 10-08 de netstack ook. Die stond hier even als pad-replace omdat we
// zelf de gaten in lneto/go-net dichtten; dat is nu een échte fork met een
// eigen module-pad (xinix00/lneto, xinix00/go-net) en een tag.
//
// Waarom een fork-pad en geen replace: een replace geldt ALLEEN in de main
// module. Alles wat metal importeert — hop-os-surf, de vitals/welcome-apps —
// zag de replace dus niet en bouwde tegen ONGEPATCHTE upstream-lneto, terwijl
// metal zelf de fixes had. Met een fork-pad reist de netstack mee in de
// require-graaf en hoeven die repo's niets te weten.
//
// Upstream overnemen: tools/refork-netstack.sh (de `hopos`-branches in beide
// clones houden het upstream-pad en blijven de basis voor de PR's).
