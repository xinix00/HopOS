//go:build ignore

// netboot-sign — teken een kern-image voor netboot, of maak het sleutelpaar.
//
//	go run tools/netboot-sign.go -keygen                 # eenmalig
//	go run tools/netboot-sign.go metal/out/hopos-apple.img
//
// De privésleutel staat buiten de repo (~/.hopos/netboot_key), net als de
// release-sleutel: wie hem heeft kan een node een willekeurige kern laten
// booten. De publieke helft gaat in de platform-config van de node
// (hopos.netboot.key=), zodat élke node zelf kan controleren wat hij binnenhaalt.
//
// Ruw ed25519 over de hele image, en geen SSH- of PGP-omhulsel: de node moet
// dit met crypto/ed25519 kunnen verifiëren zonder één regel parser erbij. Een
// formaat dat je op bare metal moet uitpakken vóór je iets kunt vertrouwen, is
// een formaat dat je op bare metal niet wilt.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	home, _ := os.UserHomeDir()
	key := filepath.Join(home, ".hopos", "netboot_key")

	if len(os.Args) == 2 && os.Args[1] == "-keygen" {
		if _, err := os.Stat(key); err == nil {
			die("%s bestaat al — weghalen doe je met de hand, expres", key)
		}
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		check(err)
		check(os.MkdirAll(filepath.Dir(key), 0o700))
		check(os.WriteFile(key, priv, 0o600))
		fmt.Printf("sleutel: %s\n", key)
		fmt.Printf("zet dit in de config van de node:\n\n  hopos.netboot.key=%s\n\n",
			base64.StdEncoding.EncodeToString(pub))
		return
	}
	if len(os.Args) != 2 {
		die("gebruik: netboot-sign.go [-keygen] <image>")
	}

	priv, err := os.ReadFile(key)
	if err != nil {
		die("geen sleutel op %s — draai eerst -keygen", key)
	}
	if len(priv) != ed25519.PrivateKeySize {
		die("%s is %d bytes, verwacht %d", key, len(priv), ed25519.PrivateKeySize)
	}
	img, err := os.ReadFile(os.Args[1])
	check(err)
	sig := ed25519.Sign(ed25519.PrivateKey(priv), img)
	out := os.Args[1] + ".sig"
	check(os.WriteFile(out, sig, 0o644))
	fmt.Printf("%s: %d bytes getekend → %s\n", os.Args[1], len(img), out)
}

func check(err error) {
	if err != nil {
		die("%v", err)
	}
}

func die(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(1)
}
