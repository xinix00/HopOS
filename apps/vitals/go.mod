module github.com/xinix00/HopOS/apps/vitals

go 1.26.4

// Eigen module binnen de hop-repo, zelfde constructie als apps/welcome: een
// HopOS-app-image linkt appnet en dat hoort niet in de dependency-graaf van
// `go install hop/cmd/cli` te sluipen. Nested modules vallen buiten `./...`
// van de parent.
//
// metal v1.11.1 en niet welcome's v1.8.3: vitals leest CtrlWakes/CtrlMemSys
// van de control-page en die woorden bestaan pas sinds de idle-telemetrie
// (06-08). v1.11.1 is de nieuwste metal-tag ZONDER pad-replaces in zijn
// go.mod, dus de enige die met GOWORK=off (release.sh) reproduceerbaar
// bouwt; de v1.12-beta's zijn keten-beta's en bouwen alleen op deze Mac.
// Sibling-dev (go.work) bouwt gewoon tegen de werkboom.
require github.com/xinix00/HopOS/metal/v2 v2.2.2

require (
	github.com/ncruces/go-sqlite3 v0.35.4
	github.com/xinix00/lean v1.1.0
)

require (
	github.com/ncruces/go-sqlite3-wasm/v5 v5.0.35304 // indirect
	github.com/ncruces/julianday v1.0.0 // indirect
	github.com/usbarmory/tamago v1.26.4 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/xinix00/HopOS/metal/v2 => ../../metal
