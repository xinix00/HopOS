// Package fb is HopOS' universele tekst-log-console op een firmware-geleverde
// lineaire framebuffer — het beeld-kanaal voor iedereen zónder debug-kabel.
//
// BEWUST GEEN driver: HopOS zet geen display-controller op en doet geen
// mode-setting (dat is board-/GPU-specifiek — we bouwen geen gaming-PC). We
// gebruiken het beeld dat de firmware al aanzette, ontdekt via een universeel
// mechanisme (board.Framebuffer(): UEFI GOP, of de device-tree simple-
// framebuffer — dezelfde twee die Linux' efifb/simplefb gebruiken), en
// schrijven daar alleen pixels in. Geen van beide is board-specifiek: samen
// dekken ze zo'n beetje elk board dat een bootlogo kan tonen.
//
// Putc is runtime-veilig (hangt onder de printk-hook van het board): geen
// allocaties, geen locks, alleen dev-writes. De buffer ligt buiten elke
// RAM-declaratie → device-gemapt/ongecachet → elke write staat meteen op het
// scherm, nul cache-maintenance.
//
// Alleen voor GOOS=tamago GOARCH=arm64.
package fb

import "hop-os/metal/dev"

// Desc beschrijft een firmware-framebuffer: het lineaire adres + geometrie.
// board.Framebuffer() vult 'm uit GOP (UEFI) of de device-tree (simplefb).
type Desc struct {
	Base          uintptr // lineair framebuffer-adres (fysiek)
	Width, Height int     // pixels
	Stride        int     // bytes per pixelrij
	BPP           int     // 32 (x8r8g8b8) of 16 (r5g6b5)
}

// scale wordt bij Init uit de framebuffer-maat afgeleid (Derek, 2026-07-11:
// 2× beviel op 1080p): volwaardige schermen krijgen 16×16-cellen (120×67 op
// 1080p), kleine buffers (zelftests) rauwe 8×8-glyphs.
var (
	scale = 2
	cellW = 8 * scale
	cellH = 8 * scale
)

var (
	d          Desc
	bpx        int    // bytes per pixel (2 of 4)
	cols, rows int    // tekencellen
	x, y       int    // cursor (cel)
	top        int    // eerste log-rij (0, of ónder de vaste Header-regels)
	fg         uint32 = 0xFFFFFFFF
	bg         uint32 = 0xFF101828 // donker blauwgrijs: "beeld doet het" ≠ zwart scherm
	active     bool
)

// Init neemt een firmware-framebuffer in gebruik, veegt hem schoon en zet de
// console actief. Alleen 16- en 32-bpp; een lege/onbegrepen descriptor laat de
// console uit (Putc blijft dan een no-op — geen scherm is geen fout).
func Init(desc Desc) {
	if desc.Base == 0 || desc.Width <= 0 || desc.Height <= 0 || desc.Stride <= 0 {
		return
	}
	switch desc.BPP {
	case 32:
		bpx = 4
	case 16:
		bpx = 2
	default:
		return
	}
	d = desc
	// Schaal uit de buffermaat: ≥720 pixelrijen = een echt scherm → 2×.
	scale = 1
	if desc.Height >= 720 {
		scale = 2
	}
	cellW, cellH = 8*scale, 8*scale
	cols, rows = desc.Width/cellW, desc.Height/cellH
	if cols == 0 || rows == 0 {
		return // scherm kleiner dan één cel: niets te tekenen
	}
	x, y, top = 0, 0, 0
	active = true
	for py := 0; py < d.Height; py++ {
		for px := 0; px < d.Width; px++ {
			putpx(px, py, bg)
		}
	}
}

// Active meldt of er een framebuffer actief is (false = Putc is een no-op).
func Active() bool { return active }

// Header tekent vaste regels bovenaan die nooit mee-scrollen — zoals Linux
// zijn logo bovenin laat staan (Dereks bunny, 2026-07-11). De log begint en
// wrapt voortaan ónder de header. Aanroepen ná Init; een tweede Init reset.
func Header(lines ...string) {
	if !active {
		return
	}
	n := len(lines)
	if n > rows-1 {
		n = rows - 1
	}
	for i := 0; i < n; i++ {
		s := lines[i]
		for j := 0; j < len(s) && j < cols; j++ {
			c := s[j]
			if c < 0x20 || c >= 0x80 {
				c = '?'
			}
			glyph(j, i, int(c))
		}
	}
	top = n
	x, y = 0, top
}

// Disable ontkoppelt de console (bv. als de buffer ongeldig wordt, of na een
// zelftest); Putc is daarna weer een no-op.
func Disable() { active = false }

// SetColor zet de voorgrondkleur (0xAARRGGBB; groen is byte-order-neutraal).
func SetColor(argb uint32) { fg = argb }

// Putc tekent één byte. UTF-8-multibyte wordt gedegradeerd: vervolgbytes
// vallen weg, de leadbyte wordt '?' — de UART/log houdt de volle tekst.
func Putc(c byte) {
	if !active {
		return
	}
	switch {
	case c == '\n':
		newline()
		return
	case c == '\r':
		x = 0
		return
	case c == '\t':
		Putc(' ')
		Putc(' ')
		return
	case c&0xC0 == 0x80: // UTF-8-vervolgbyte
		return
	case c >= 0x80: // UTF-8-leadbyte
		c = '?'
	case c < 0x20:
		return
	}
	if x >= cols {
		newline()
	}
	glyph(x, y, int(c))
	x++
}

// newline schuift de cursor; onderaan wrapt hij terug naar de eerste
// log-rij (onder de header). De doelregel wordt eerst geveegd zodat oude
// tekst nooit door nieuwe heen schemert.
func newline() {
	x = 0
	y++
	if y >= rows {
		y = top
	}
	clearRow(y)
}

func clearRow(row int) {
	for py := row * cellH; py < row*cellH+cellH && py < d.Height; py++ {
		for px := 0; px < d.Width; px++ {
			putpx(px, py, bg)
		}
	}
}

// glyph tekent font8x8[c] op celpositie (cx, cy), met scale×scale pixels per
// fontbit. LSB van elke fontrij is de linkerpixel.
func glyph(cx, cy, c int) {
	px0, py0 := cx*cellW, cy*cellH
	for gy := 0; gy < 8; gy++ {
		bits := font8x8[c][gy]
		for sy := 0; sy < scale; sy++ {
			py := py0 + gy*scale + sy
			for gx := 0; gx < 8; gx++ {
				col := bg
				if bits>>gx&1 != 0 {
					col = fg
				}
				for sx := 0; sx < scale; sx++ {
					putpx(px0+gx*scale+sx, py, col)
				}
			}
		}
	}
}

// putpx schrijft één pixel (0xAARRGGBB) op (px, py); pakt naar r5g6b5 bij
// een 16-bpp-scherm. De device-store is gealigneerd (px*bpx, bpx ∈ {2,4}).
func putpx(px, py int, argb uint32) {
	off := uintptr(py*d.Stride + px*bpx)
	if bpx == 2 {
		r, g, b := (argb>>16)&0xff, (argb>>8)&0xff, argb&0xff
		dev.Write16(d.Base+off, uint16((r>>3)<<11|(g>>2)<<5|(b>>3)))
		return
	}
	dev.Write32(d.Base+off, argb)
}
