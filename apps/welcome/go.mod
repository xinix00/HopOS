module github.com/xinix00/HopOS/apps/welcome

go 1.26.4

// Een eigen module binnen de hop-repo, bewust niet één van de hop-module zelf:
// een HopOS-app-image linkt appnet (gVisor) en dat hoort niet in de
// dependency-graaf van `go install hop/cmd/cli` te sluipen. Nested modules
// vallen buiten `./...` van de parent, dus hop's CI (go 1.24) ziet deze map
// niet en de hop-module blijft op zijn eigen go-directive staan.
//
// hop-os/metal is een echte GitHub-dep (metal/vX.Y.Z-tag in de HopOS-
// repo), dus geen lokale replaces meer nodig; sibling-dev loopt via
// go.work.
require github.com/xinix00/HopOS/metal v1.12.2

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
