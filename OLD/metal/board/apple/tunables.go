//go:build tamago && arm64

// tunables.go — de instelwaarden die de firmware in de boom achterliet.
//
// Apple levert per silicium-revisie een lijst registerpatches mee in de ADT:
// "zet op offset X, breedte N, deze bits op deze waarde". Ze staan er omdat ze
// per chip-revisie verschillen — het is de reden dat m1n1 en Linux de PCIe-PHY
// van deze machine kunnen opbrengen zonder de waarden te kennen. Wij lezen
// dezelfde lijsten.
//
// Formaat (m1n1 src/tunables.c, struct tunable_local): 24 bytes per regel —
// offset (u32), breedte (u32), masker (u64), waarde (u64). De u64's lezen we
// in twee helften: een record begint op een viervoud, niet op een achtvoud, en
// de boom ligt in device-geheugen.
package apple

import (
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/fw/adt"
)

const tunableRec = 24

// applyTunables past de lijst `prop` van node `n` toe op `base`. Geeft het
// aantal toegepaste regels en of de eigenschap bestond — ontbreken is géén
// fout: niet elke revisie heeft elke lijst, en m1n1 slaat ze dan ook over.
func applyTunables(t adt.Tree, n adt.Node, prop string, base uintptr) (int, bool) {
	addr, size, ok := t.Prop(n, prop)
	if !ok || size == 0 || size%tunableRec != 0 {
		return 0, false
	}
	count := 0
	for i := uint32(0); i < size/tunableRec; i++ {
		r := addr + uintptr(i)*tunableRec
		off := dev.Read32(r)
		width := dev.Read32(r + 4)
		mask := uint64(dev.Read32(r+8)) | uint64(dev.Read32(r+12))<<32
		val := uint64(dev.Read32(r+16)) | uint64(dev.Read32(r+20))<<32
		a := base + uintptr(off)
		switch width {
		case 1:
			dev.Write8(a, dev.Read8(a)&^uint8(mask)|uint8(val))
		case 2:
			dev.Write16(a, dev.Read16(a)&^uint16(mask)|uint16(val))
		case 4:
			dev.Write32(a, dev.Read32(a)&^uint32(mask)|uint32(val))
		case 8:
			dev.Write64(a, dev.Read64(a)&^mask|val)
		default:
			continue
		}
		count++
	}
	return count, true
}
