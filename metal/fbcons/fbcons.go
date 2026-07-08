// Package fbcons is een minimale tekstconsole op een 32bpp-framebuffer —
// het HDMI-levensteken voor boards zonder bereikbare UART (Pi 5 zonder
// JST-kabeltje, zie docs/rpi5.md "Boot-splash"). Geen input, geen scrollback:
// alleen tekens tekenen, regel voor regel, met wrap-naar-boven als het
// scherm vol is.
//
// Putc is runtime-veilig (hangt onder de printk-hook van het board): geen
// allocaties, geen locks, alleen dev-writes naar het framebuffer. Het
// framebuffer ligt buiten de RAM-declaratie en is dus device-gemapt
// (ongecachet) — elke write is meteen zichtbaar voor de scanout, nul
// cache-maintenance.
//
// Alleen voor GOOS=tamago GOARCH=arm64.
package fbcons

import "hop-os/metal/dev"

// scale: 8x8-glyphs op 2× = 16x16-cellen — leesbaar op een TV-foto.
const (
	scale = 2
	cellW = 8 * scale
	cellH = 8 * scale
)

var (
	base          uintptr // framebuffer (fysiek, device-gemapt)
	pitch         int     // bytes per pixelrij
	width, height int     // pixels
	cols, rows    int     // tekencellen
	x, y          int     // cursor (cel)
	fg            uint32  = 0xFFFFFFFF
	bg            uint32  = 0xFF101828 // donker blauwgrijs: "beeld doet het" ≠ zwart scherm
	ready         bool
)

// Init neemt een door de firmware toegewezen framebuffer in gebruik en veegt
// hem schoon. Vanaf hier is Ready() true en tekent Putc.
func Init(fb uintptr, w, h, p int) {
	base, width, height, pitch = fb, w, h, p
	cols, rows = w/cellW, h/cellH
	x, y = 0, 0
	for py := 0; py < h; py++ {
		row := base + uintptr(py*pitch)
		for px := 0; px < w; px++ {
			dev.Write32(row+uintptr(px*4), bg)
		}
	}
	ready = true
}

// Ready meldt of er een framebuffer actief is (false = Putc is een no-op).
func Ready() bool { return ready }

// SetColor zet de voorgrondkleur (0xAARRGGBB; groen is byte-order-neutraal).
func SetColor(argb uint32) { fg = argb }

// Putc tekent één byte. UTF-8-multibyte wordt gedegradeerd: vervolgbytes
// vallen weg, de leadbyte wordt '?' — de UART houdt de volle tekst.
func Putc(c byte) {
	if !ready {
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

// newline schuift de cursor; onderaan wrapt hij naar boven. De doelregel
// wordt eerst geveegd zodat oude tekst nooit door nieuwe heen schemert.
func newline() {
	x = 0
	y++
	if y >= rows {
		y = 0
	}
	clearRow(y)
}

func clearRow(row int) {
	top := row * cellH
	for py := top; py < top+cellH && py < height; py++ {
		p := base + uintptr(py*pitch)
		for px := 0; px < width; px++ {
			dev.Write32(p+uintptr(px*4), bg)
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
			row := base + uintptr((py0+gy*scale+sy)*pitch)
			for gx := 0; gx < 8; gx++ {
				col := bg
				if bits>>gx&1 != 0 {
					col = fg
				}
				for sx := 0; sx < scale; sx++ {
					dev.Write32(row+uintptr((px0+gx*scale+sx)*4), col)
				}
			}
		}
	}
}
