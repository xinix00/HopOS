//go:build tamago && arm64

package apple

import "github.com/xinix00/HopOS/metal/v2/dev"

// De indeling van de eerste bladzijde van het platte image staat in bootstub.s
// (waar de stubs hem gebruiken) en in image/mkkernel/apple.go (waar hij gelegd
// wordt). Niet óók hier: de assembler en het host-gereedschap kunnen dit pakket
// niet importeren, dus een derde kopie zou alleen maar een derde plek zijn die
// uit de pas kan lopen.

// In bootstub.s. Ze worden nooit aangeroepen: mkkernel vist ze uit de ELF en
// legt ze vooraan in het image, waar de firmware ze vindt.
func stubReset()
func stubEntry()

// bootStubs bestaat om precies één reden: de Go-linker gooit weg wat niemand
// roept, en niemand roept deze twee. De init hieronder is die roeper — een
// pakket-init blijft altijd staan — en houdt beide stubs in de ELF, zodat
// mkkernel ze kan vinden. Zonder dit bouwt alles gewoon door en struikelt pas
// de imagestap, over een onvindbaar symbool.
var bootStubs []func()

func init() { bootStubs = []func(){stubReset, stubEntry} }

// StubSource is het adres waar de firmware dit bootobject neerzette, zoals de
// stub het opschreef vlak voor hij naar de kern sprong. Nul betekent: er is
// geen stub gedraaid (ouder image, of een loader die rechtstreeks naar de kern
// springt).
//
// Het is niet zomaar een weetje. iBoot zet RVBAR van élke core op het begin van
// het bootobject en vergrendelt het; dit getal is dus precies waar een core uit
// reset landt, en daarmee het antwoord op de vraag of de cores van ons zijn.
func StubSource() uint64 { return dev.Read64(StubSrc) }

// FirmwareX0 is de waarde die in x0 stond toen we binnenkwamen: iBoot's
// boot_args-blok als wij het bootobject zijn, m1n1's FDT als de proxy ons
// startte. Nul = de stub draaide niet.
func FirmwareX0() uint64 { return dev.Read64(StubX0) }
