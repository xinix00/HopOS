package hopslot

import (
	_ "unsafe" // voor go:linkname

	"github.com/xinix00/HopOS/metal/board/appboard"
)

// printk: een gekooide core heeft geen UART-MMIO in zijn kooi (een poke zou
// een cage-fault zijn), dus runtime-output kan maar één kant op — de haak die
// applib bij Init in appboard hangt: de hop-ABI-log-ring. Vóór Init (of in een
// image zonder applib) is dit stil, precies wat het altijd was.
//
// Waarom dit bestaat: app-lógs liepen al via de ring (applib.Logf), maar
// runtime-output niet — en het enige dat de runtime nog te zeggen heeft is een
// panic. Die verdween dus geluidloos: exit-code 2, geen regel reden (gemeten
// 31-07, de apploader-OOM). Nu is een panic gewoon de laatste task-logregel.
//
//go:linkname printk runtime/goos.Printk
func printk(c byte) {
	if f := appboard.PrintkSink; f != nil {
		f(c)
	}
}
