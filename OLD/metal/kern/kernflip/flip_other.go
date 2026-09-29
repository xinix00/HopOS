//go:build !tamago

package kernflip

import "fmt"

// Op de ontwikkelmachine is er geen geheugen om een kern in te plaatsen en
// niets om in te springen. De bundel-parser en het handoff-blob zijn wél
// host-testbaar (bundle.go/handoff.go) — dít bestand dekt alleen de
// uitvoerende kant af.
func Flip(bundle []byte) error {
	return fmt.Errorf("kernflip: a kernel flip only runs on the target")
}

func FlipFromURL(url, sha string) error {
	return fmt.Errorf("kernflip: a kernel flip only runs on the target")
}
