//go:build !media

package main

// Buiten de media-smaak is er geen optische drive: headless heeft geen
// USB-stack (usbin woont in de gui), en de gui-smaak laat opslag over USB
// bewust aan media over. Zie disc.go voor de andere helft.
func startDisc() {}

func discQuery() string { return "disc: this kernel is not the media flavour" }
