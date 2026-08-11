//go:build !tamago

// Host-stub zodat `go build ./...` op een gewone GOOS groen blijft; het echte
// programma (main.go) bestaat alleen onder GOOS=tamago.
package main

func main() {
	println("vitals is a HopOS app image — build with GOOS=tamago (see README.md)")
}
