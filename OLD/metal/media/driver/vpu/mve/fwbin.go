package mve

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/xinix00/HopOS/metal/v2/dev"
)

// Elke codec is een eigen firmware-binary die wij in de VPU laden: hevcdec,
// h264dec, av1dec, mpeg2dec, vc1dec — de hele Blu-ray-verzameling, elk zo'n
// 300KB. Dat is het hele idee van dit blok: de bitstream-kennis zit in de
// firmware van Arm/CIX, niet bij ons. Wij laden bytes op de juiste virtuele
// adressen en praten daarna een berichtenprotocol.
//
// Het binaire formaat is een kop van 180 bytes gevolgd door de code. De kop
// vertelt waar de code heen moet, welke bss-pagina's er aangemaakt moeten
// worden, en welke versie van het host-protocol deze firmware spreekt (de
// Linlon V8 van de O6N levert major 3).
const (
	fwHeaderLen = 180
	fwTextBase  = 0x1000 // de nulpagina blijft expres onbemapt
	fwMagic     = 0x0000eb5e
)

// fwHeader is de kop van een .fwb-bestand.
type fwHeader struct {
	RascJmp       uint32
	ProtocolMinor uint8
	ProtocolMajor uint8
	Info          string // "HEVC Decoder"
	PartNumber    string // "56648002"
	Version       string // "HEVCDEC ... r0p0"
	TextLength    uint32
	BSSStart      uint32
	BSSBitmapSize uint32
	BSSBitmap     [16]uint32
	MasterRWStart uint32
	MasterRWSize  uint32
}

var errFWShort = errors.New("mve: firmware binary shorter than its header")

// parseFW leest de kop en controleert hem tegen de werkelijke bestandslengte.
// Alles wat niet klopt is hier een fout en geen aanname: een firmware die we
// verkeerd inladen hangt de VPU op een manier die alleen een reset oplost, en
// die kost op ijzer een hele cyclus.
func parseFW(bin []byte) (fwHeader, error) {
	var h fwHeader
	if len(bin) < fwHeaderLen {
		return h, errFWShort
	}
	le := binary.LittleEndian
	h.RascJmp = le.Uint32(bin[0:])
	h.ProtocolMinor = bin[4]
	h.ProtocolMajor = bin[5]
	h.Info = cstring(bin[8:64])
	h.PartNumber = cstring(bin[64:72])
	h.Version = cstring(bin[80:96])
	h.TextLength = le.Uint32(bin[96:])
	h.BSSStart = le.Uint32(bin[100:])
	h.BSSBitmapSize = le.Uint32(bin[104:])
	for i := range h.BSSBitmap {
		h.BSSBitmap[i] = le.Uint32(bin[108+4*i:])
	}
	h.MasterRWStart = le.Uint32(bin[172:])
	h.MasterRWSize = le.Uint32(bin[176:])

	if h.RascJmp&0xffff != fwMagic&0xffff {
		return h, fmt.Errorf("mve: not a firmware binary (jump %#x)", h.RascJmp)
	}
	if h.TextLength > uint32(len(bin)) {
		return h, fmt.Errorf("mve: text %d bytes but binary is %d", h.TextLength, len(bin))
	}
	if h.BSSBitmapSize > uint32(len(h.BSSBitmap))*32 {
		return h, fmt.Errorf("mve: bss bitmap %d bits, max %d", h.BSSBitmapSize, len(h.BSSBitmap)*32)
	}
	if h.BSSStart&(pageSize-1) != 0 {
		return h, fmt.Errorf("mve: bss start %#x is not page aligned", h.BSSStart)
	}
	if h.ProtocolMajor < 2 || h.ProtocolMajor > 3 {
		return h, fmt.Errorf("mve: host interface v%d.%d is not supported", h.ProtocolMajor, h.ProtocolMinor)
	}
	return h, nil
}

// textPages is het aantal pagina's dat het read-only deel kost.
func (h fwHeader) textPages() uint32 {
	return (h.TextLength + pageSize - 1) >> pageShift
}

// bssPage meldt of pagina i van het bss-gebied aangemaakt moet worden. De
// bitmap is dun: een codec-firmware reserveert een adresbereik dat veel groter
// is dan wat hij werkelijk aanraakt.
func (h fwHeader) bssPage(i uint32) bool {
	if i >= h.BSSBitmapSize {
		return false
	}
	return h.BSSBitmap[i/32]&(1<<(i%32)) != 0
}

// shared meldt of de pagina op dit virtuele adres gedeeld is tussen de cores
// van één sessie (het master_rw-venster). Met één core per sessie is dat
// alleen bookkeeping; het telt zodra we een sessie over meerdere cores spreiden
// voor 8K.
func (h fwHeader) shared(va uint32) bool {
	return h.MasterRWSize != 0 && va >= h.MasterRWStart && va < h.MasterRWStart+h.MasterRWSize
}

// loadFW schrijft een firmware-binary in de arena en hangt hem in de page
// table van een sessie: de code op 0x1000 (read-only, uitvoerbaar) en de
// bss-pagina's uit de bitmap (leesbaar/schrijfbaar, gewist).
//
// Eén core per sessie. De firmware kan over meerdere cores gespreid worden
// (dan komt hij nog een keer op instance 1..7 te staan, met eigen bss), en dat
// is wat je nodig hebt om 8K te halen — maar voor 4K Blu-ray is één core
// ruim, en dit pad is dan ook waar de tweede core later bijkomt.
func loadFW(m *mmu, bin []byte, h fwHeader) (uintptr, error) {
	text := h.textPages()
	pa, err := m.alloc(fwTextBase, text, accessExec)
	if err != nil {
		return 0, err
	}
	dev.Copy(pa, bin[:h.TextLength])

	va := h.BSSStart
	for i := uint32(0); i < h.BSSBitmapSize; i++ {
		if h.bssPage(i) || h.shared(va) {
			if _, err := m.alloc(va, 1, accessRW); err != nil {
				return 0, err
			}
		}
		va += pageSize
	}
	return pa, nil
}

// cstring leest een nul-getermineerde tekst uit een vast veld.
func cstring(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
