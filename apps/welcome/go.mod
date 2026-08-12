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
require github.com/xinix00/HopOS/metal v1.15.1

require github.com/xinix00/lean v0.5.1

require github.com/usbarmory/tamago v1.26.4 // indirect

replace github.com/xinix00/HopOS/metal => ../../metal
