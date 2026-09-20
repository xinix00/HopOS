//go:build o6n && gui

package main

import (
	"fmt"
	o6n "github.com/xinix00/HopOS/metal/v2/board/o6n/hop"
	"github.com/xinix00/HopOS/metal/v2/board/uefi"
	"github.com/xinix00/HopOS/metal/v2/gui/driver/usb/xhci"
	"github.com/xinix00/HopOS/metal/v2/gui/usbin"
)

func init() {
	hosts, err := o6n.USBHosts()
	if err != nil {
		fmt.Printf("usb: O6N discovery: %v — no USB input on this boot\n", err)
		return
	}
	// Halt every enabled controller before any persistent DMA slice is cleared.
	// This also makes a firmware/layout upgrade (six hosts to ten) safe. A
	// failure leaves all DMA memory untouched; the original carve stays owned.
	prepared := false
	var prepareErr error
	prepare := func() error {
		if prepared {
			return prepareErr
		}
		prepared = true
		for _, h := range hosts {
			if !h.Enabled {
				continue
			}
			if !uefi.MapHigh(h.Base, h.Size) {
				prepareErr = fmt.Errorf("%s register window is unreachable", h.Name)
				return prepareErr
			}
			hc := &xhci.HC{Name: h.Name, Base: uintptr(h.Base)}
			prepareErr = hc.Probe()
			if prepareErr == nil {
				prepareErr = hc.Reset()
			}
			if prepareErr != nil {
				return prepareErr
			}
		}
		return nil
	}
	for _, h := range hosts {
		usbin.Register(usbin.Host{Name: h.Name, Base: uintptr(h.Base), Prepare: func() (uintptr, error) {
			if !h.Enabled {
				return 0, fmt.Errorf("firmware disabled or device-role controller")
			}
			if err := prepare(); err != nil {
				return 0, err
			}
			return uintptr(h.Base), nil
		}})
	}
}
