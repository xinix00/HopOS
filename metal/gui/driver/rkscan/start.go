package rkscan

import "fmt"

// Start brengt de hele beeldketen op: power-domein, VOP2-scanout, HDMI-TX. De
// framebuffer (base/w/h/stride) is board-kennis en komt als argument binnen —
// rk3566.FB() past exact op deze signatuur.
//
// Faalt er iets, dan zegt de melding welke laag en gaat de boot gewoon door —
// de buffer blijft bruikbaar over het netwerk (/kvm), en dát is de reden dat
// dit géén fout is die de node tegenhoudt.
//
// De volgorde is niet vrij: eerst VOP2 (die levert pixels en de dclk), dan pas
// de HDMI-TX. Andersom staat de frame composer geprogrammeerd tegen een
// stilstaande klok — dezelfde ordening die DRM aanhoudt (eerst alle CRTC's,
// dan de bridges).
func Start(base uintptr, w, h, stride int) {
	if err := PowerOnVO(); err != nil {
		fmt.Printf("display: %v — framebuffer stays network-only (/kvm)\n", err)
		return
	}
	if !VOPAlive() {
		fmt.Println("display: VOP2 does not answer — framebuffer stays network-only (/kvm)")
		return
	}
	if err := VOPScanout(base, w, h, stride); err != nil {
		fmt.Printf("display: %v — framebuffer stays network-only (/kvm)\n", err)
		return
	}
	if err := HDMIEnable(); err != nil {
		fmt.Printf("display: VOP2 scans but %v — framebuffer stays network-only (/kvm)\n", err)
		return
	}
	fmt.Printf("display: 1920x1080p60 on HDMI (sink attached: %v)\n", HDMIHotplug())
}
