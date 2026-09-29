//go:build gui

package main

import "github.com/xinix00/HopOS/metal/v2/gui/usbin"

// startUSBInput brengt de USB-controllers op die usb_<board>.go registreerde en
// begint te scannen. Na het netwerk, want HOP serveert de invoerstroom op het
// interne gateway-adres en dat bestaat pas na hopswitch.Up(). Doet niets op een
// board dat geen controllers registreert.
func startUSBInput() { usbin.Start() }
