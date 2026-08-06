//go:build !gui

package main

// startUSBInput is in de kale smaak leeg: zonder scherm is er niets om in te
// typen, dus er linkt geen regel USB-code mee. Zie usbinput.go voor de
// gui-kant en metal/gui/usbin voor waarom een toetsenbord een gui-apparaat is.
func startUSBInput() {}
