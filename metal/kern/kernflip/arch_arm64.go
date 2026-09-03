//go:build tamago && arm64

package kernflip

import (
	"fmt"
	"runtime"

	"github.com/xinix00/HopOS/metal/v2/abi/layout"
	"github.com/xinix00/HopOS/metal/v2/cpu/el2"
	"github.com/xinix00/HopOS/metal/v2/cpu/smp"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/kern/stage2"
)

// De ARM-helft van de flip-naad (docs/kern-flip.md). Alles wat hier staat is
// het antwoord op één vraag per architectuur; de flip zelf (flip.go) is
// gedeeld.

// De code die een app-core uitvoert woont op ARM in cpu/el2 en wordt door
// kern/stage2 naar de plan-regio gekopieerd.
func blobSymbols() [][2]string { return el2.BlobSymbols }
func maxBlobSize() uint64      { return el2.MaxBlobSize }
func switchCodeHash() uint64   { return stage2.SwitchCodeHash() }
func setAdopting(v bool)       { stage2.SetAdopting(v) }

// nodeCoresActive: hoeveel EXTRA cores draaien de node-runtime van deze kern
// (hopos.cores > 1). Die voeren Go-code uit die na de sprong niet meer bestaat,
// dus met zulke cores mag er niet geflipt worden.
func nodeCoresActive() int { return smp.NodeStarted() }

// firmwareArg is wat de nieuwe kern in x0 hoort te krijgen: de DTB-pointer die
// de firmware óns ooit gaf (cpuinit legde hem op de boot-scratch). Zo komt hij
// binnen op exact dezelfde conditie als bij een firmware-boot.
func firmwareArg() uint64 { return dev.Read64(layout.BootScratchPA() + 8) }

// archPreflight: een lege firmware-pointer is op een board dat er een NODIG
// heeft (apple: boot_args → MapDRAM) een gegarandeerd dode landing — zes
// ijzer-flips lang, gevonden 01-09 via de dockchannel ("hopos x0…0" + EL1
// exception). Maar op qemu-virt is 0 het eerlijke contract (vast plan, geen
// DTB), dus dit is een WAARSCHUWING en geen weigering: het board dat hem
// nodig heeft publiceert hem in zijn SetupPlan, en déze regel maakt het
// vergeten daarvan zichtbaar in plaats van fataal-en-stil.
func archPreflight() error {
	if firmwareArg() == 0 {
		fmt.Printf("kernflip: WARNING — firmware pointer at boot-scratch+8 is 0; fine on boards that need none (virt), fatal on boards that do (apple publishes it in SetupPlan)\n")
	}
	return nil
}

// chainload springt op EL2 in de nieuwe kern; keert nooit terug.
func chainload(entry, x0arg uint64) { el2.Chainload(entry, x0arg) }

// ownRamEnd is het einde van de RAM-declaratie van deze kern — en tegelijk de
// enige plek waar een handoff-blob kan liggen: de flip legt hem per
// constructie in de staart van het geleende venster, direct boven wat hij als
// RamSize patcht. Adopted toetst de scratch-pointer daartegen, zodat een
// verdwaalde of vergiftigde waarde nooit een wilde read wordt.
func ownRamEnd() uint64 {
	_, end := runtime.MemRegion()
	return uint64(end)
}
