//go:build !tamago

// Host-stub: de echte main (main.go) is tamago-only — applib bestaat alleen
// onder GOOS=tamago. Deze stub houdt `go build ./...` en `go vet ./...` op de
// host groen (zelfde patroon als de andere HopOS-apps). Wie cloudflared op een
// gewoon OS wil: dat is cloudflared zelf, ongewijzigd.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "cloudflared-hopos is een HopOS-slot-image; bouw met tools/build.sh (na tools/prepare-cloudflared.sh)")
	os.Exit(1)
}
