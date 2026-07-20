// Package hvs is de READ-ONLY dumptool voor de BCM2712-HVS (gen6/SCALER6) —
// fase P4, stap 1 van docs/gui-ontwerp.md §8: eerst kijken hoe de firmware
// de display-pipeline heeft opgezet, pas daarna (P4 stap 2) muteren. De
// firmware-pipeline wordt nóóit vervangen (derek-debug-methode: referentie
// eerst, meetinstrument eerst bewijzen).
//
// Referentie: Linux drivers/gpu/drm/vc4 (vc4_regs.h/vc4_hvs.c, rpi-6.12.y) en
// arch/arm64/boot/dts/broadcom/bcm2712.dtsi:
//
//	hvs@107c580000, "brcm,bcm2712-hvs", grootte 0x1a000
//	registers: VERSION @0x00, CONTROL @0x20,
//	  per kanaal x∈{0,1,2} @ 0x30+x*0x20: CTRL0, CTRL1, BGND, LPTRS,
//	  COB, STATUS, DL, RUN
//	display-list-SRAM: +0x2000, 16KB (4096 woorden)
//	dlist-entry: CTL0 met END(31), VALID(30), NEXT[29:24] (woorden naar de
//	  volgende entry), ADDR_MODE[22:20], UNITY(15), PIXEL_FORMAT[4:0]
//
// D0-stepping (SCALER6D, VERSION meldt het): zelfde CTRL/LPTRS-offsets,
// alleen BGND is daar gesplitst — voor een read-only dump irrelevant.
package hvs

import (
	"fmt"
	"strings"

	"hop-os/metal/dev"
)

// Registeroffsets (SCALER6, BCM2712).
const (
	regVersion = 0x00
	regControl = 0x20
	chanBase   = 0x30
	chanStride = 0x20

	dlistOff   = 0x2000
	dlistWords = 0x4000 / 4
)

// Channel is de registersnapshot van één display-FIFO.
type Channel struct {
	Ctrl0, Ctrl1, Bgnd, Lptrs, Cob, Status, DL, Run uint32
}

// Enabled: draait dit kanaal (CTRL0.ENB, bit 31)?
func (c Channel) Enabled() bool { return c.Ctrl0&(1<<31) != 0 }

// Head: het startwoord van de display-list van dit kanaal (LPTRS[11:0]).
func (c Channel) Head() int { return int(c.Lptrs & 0xFFF) }

// Dump is één read-only momentopname van de HVS.
type Dump struct {
	Version  uint32
	Control  uint32
	Channels [3]Channel
	DList    []uint32 // de volledige dlist-SRAM (4096 woorden)
}

// Read neemt de momentopname (alleen loads; niets wordt geschreven).
func Read(base uintptr) Dump {
	d := Dump{
		Version: dev.Read32(base + regVersion),
		Control: dev.Read32(base + regControl),
		DList:   make([]uint32, dlistWords),
	}
	for x := 0; x < 3; x++ {
		b := base + chanBase + uintptr(x)*chanStride
		d.Channels[x] = Channel{
			Ctrl0: dev.Read32(b), Ctrl1: dev.Read32(b + 4),
			Bgnd: dev.Read32(b + 8), Lptrs: dev.Read32(b + 0xc),
			Cob: dev.Read32(b + 0x10), Status: dev.Read32(b + 0x14),
			DL: dev.Read32(b + 0x18), Run: dev.Read32(b + 0x1c),
		}
	}
	for i := 0; i < dlistWords; i++ {
		d.DList[i] = dev.Read32(base + dlistOff + uintptr(i)*4)
	}
	return d
}

// Plane is één gedecodeerde dlist-entry (best effort: CTL0 is gedocumenteerd,
// de woordvolgorde erachter leiden we uit echte dumps af — dáárom dit
// instrument; de ruwe woorden reizen altijd mee).
type Plane struct {
	Index  int // woordindex in de dlist
	CTL0   uint32
	Valid  bool
	Format int
	Unity  bool
	Words  []uint32 // de hele entry, inclusief CTL0
}

// ParseList loopt een display-list vanaf head af via de NEXT-velden tot END
// (of een veiligheidsgrens — kapotte data mag de dump niet laten hangen).
func ParseList(words []uint32, head int) []Plane {
	var out []Plane
	idx := head
	for hops := 0; hops < 64; hops++ {
		if idx < 0 || idx >= len(words) {
			break
		}
		ctl := words[idx]
		if ctl&(1<<31) != 0 { // END
			break
		}
		next := int(ctl >> 24 & 0x3F)
		if next == 0 {
			next = 1 // nooit stilstaan op rommel
		}
		end := idx + next
		if end > len(words) {
			end = len(words)
		}
		out = append(out, Plane{
			Index: idx, CTL0: ctl,
			Valid:  ctl&(1<<30) != 0,
			Format: int(ctl & 0x1F),
			Unity:  ctl&(1<<15) != 0,
			Words:  words[idx:end],
		})
		idx = end
	}
	return out
}

// Text rendert de dump leesbaar (voor het debug-endpoint en de UART).
func (d Dump) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "hvs: version %#08x control %#08x\n", d.Version, d.Control)
	for x, c := range d.Channels {
		fmt.Fprintf(&b, "ch%d: ctrl0 %#08x ctrl1 %#08x bgnd %#08x lptrs %#08x cob %#08x status %#08x dl %#08x run %#08x enabled=%v head=%d\n",
			x, c.Ctrl0, c.Ctrl1, c.Bgnd, c.Lptrs, c.Cob, c.Status, c.DL, c.Run, c.Enabled(), c.Head())
	}
	for x, c := range d.Channels {
		if !c.Enabled() {
			continue
		}
		planes := ParseList(d.DList, c.Head())
		fmt.Fprintf(&b, "ch%d display list (%d entries from word %d):\n", x, len(planes), c.Head())
		for _, p := range planes {
			fmt.Fprintf(&b, "  @%d ctl0=%#08x valid=%v fmt=%d unity=%v words:", p.Index, p.CTL0, p.Valid, p.Format, p.Unity)
			for _, w := range p.Words {
				fmt.Fprintf(&b, " %08x", w)
			}
			b.WriteByte('\n')
		}
	}
	// De eerste 64 ruwe woorden altijd erbij: als de parser het formaat
	// verkeerd raadt, liegt de hexdump niet.
	fmt.Fprintf(&b, "dlist[0:64]:")
	for i := 0; i < 64 && i < len(d.DList); i++ {
		if i%8 == 0 {
			fmt.Fprintf(&b, "\n  %4d:", i)
		}
		fmt.Fprintf(&b, " %08x", d.DList[i])
	}
	b.WriteByte('\n')
	return b.String()
}
