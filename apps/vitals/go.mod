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
require github.com/xinix00/HopOS/metal v1.11.1

require github.com/xinix00/lean v0.1.0

require (
	github.com/google/btree v1.1.2 // indirect
	github.com/soypat/lneto v0.1.1-0.20260609173350-82f946154800 // indirect
	github.com/usbarmory/go-net v0.0.0-20260626130943-dad9ef39fd9b // indirect
	github.com/usbarmory/tamago v1.26.4 // indirect
	golang.org/x/sys v0.44.0 // indirect
	golang.org/x/time v0.7.0 // indirect
	gvisor.dev/gvisor v0.0.0-20250911055229-61a46406f068 // indirect
)

replace github.com/xinix00/HopOS/metal => ../../metal
