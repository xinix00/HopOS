// Package memattr verzet het geheugen-attribuut van een venster in de EIGEN
// stage-1-map van Device-nGnRnE naar Normal Non-Cacheable (write-combine).
//
// WAAROM (gemeten 2026-08-04, Pi 5): tamago's identity-map declareert alles
// buiten de eigen RAM-declaratie als Device-nGnRnE. Dat is juist voor
// registers, maar een framebuffer is geen register — het is DRAM. Onder
// nGnRnE mag het interconnect stores níet samenvoegen (nG), níet herordenen
// (nR) en niet vroeg bevestigen (nE), dus een 1920×1080×4-frame verlaat de
// core als ~1 miljoen losse, geordende transacties in plaats van ~130.000
// gatherbare bursts van 64 byte.
//
// Normal-NC is precies wat Linux een framebuffer geeft (write-combine): geen
// cache — dus geen cache-onderhoud nodig en de scanout leest gewoon DRAM — maar
// het fabric mag wél gatheren. Zelfde bytes, ~8× minder transacties.
//
// Waarom niet Normal WB (cacheable): dan zou elke tekenactie schoongeveegd
// moeten worden vóór de scanout hem ziet, en dat cache-onderhoud is precies een
// van de fabric-brede operaties die we juist willen vermijden.
//
// WAT WEL EN NIET BEWEZEN IS. Bewezen op ijzer: de map landt (de app meldt
// "mapped write-combine") en het beeld blijft correct — de desktop rendeert
// zoals voorheen. NIET bewezen: dat het meetbaar sneller is. De damage-stream
// gaf met en zonder write-combine hetzelfde (56642 tegen 56656 bytes per 30s),
// maar die meetlat verzadigt op de compositor-cadans en zegt dus niets over de
// blit zelf. En de freeze van 04-08 reproduceerde er ook mét write-combine —
// die had drie andere poten. Wie de winst hard wil maken, moet een blit-tijd ín
// de app meten; de redenering hierboven (transactieaantal) is de grond, niet
// een stopwatch.
//
// Alleen zinvol op arm64 (het attribuut ís een ARM-begrip); elders no-op, zodat
// aanroepers geen bouwtags nodig hebben. De aanroeper hoeft niets terug te
// draaien: het venster blijft Normal-NC tot de reboot.
package memattr
