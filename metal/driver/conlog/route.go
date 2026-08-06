package conlog

import "github.com/xinix00/HopOS/metal/driver/fb"

// Route is de bestemmingenlijst van één console-byte, op één plek.
//
// WAAROM DIT BESTAAT (Derek, 06-08: "tis in beide toch een framebuffer die we
// generiek aansturen?"). De framebuffer-driver ÍS generiek — maar het
// routeringsbeleid stond vijf keer los, één keer per board dat een
// runtime/goos.Printk-hook heeft:
//
//	qemuvirt   ring + UART + fb
//	licheerv   ring + UART            (terecht: dat bord heeft geen scherm)
//	rpi4/rpi5  ring + UART + fb, met een core-guard en tijdstempels
//	rk3566     ring + UART            ← vergat de fb
//
// Dat laatste kostte een meting: op de monitor stond wél de bunny-header (die
// schrijft fb.Header rechtstreeks) en géén enkele logregel, want die lopen via
// de hook. "Half beeld" wees dus naar de scanout terwijl het een ontbrekende
// regel in de console was. Vijf kopieën van dezelfde lijst is precies hoe dat
// gebeurt: de zesde vergeet er weer één.
//
// Wat ECHT per board verschilt is één ding — hoe je één byte op de lijn zet.
// Dat is het argument. De rest is beleid en hoort hier:
//
//  1. de ring eerst, altijd. Hangt de UART-poll (kabel eruit, blok ongeklokt),
//     dan is de byte alsnog over het netwerk op te vragen;
//  2. dan de lijn;
//  3. dan het glas. fb.Putc is een no-op zolang fb.Init niet gedraaid heeft,
//     dus een board zonder scherm betaalt hier niets en hoeft niets te weten.
//
// De core-guard staat NIET hier: "ben ik de HOP-core" is arch-werk (MPIDR op
// arm64, hartid op RISC-V) en een board dat hem nodig heeft zet hem in één
// regel vóór de aanroep. Zie board/rk3566/console.go.
func Route(c byte, line func(byte)) {
	Put(c)
	if line != nil {
		line(c)
	}
	fb.Putc(c)
}
