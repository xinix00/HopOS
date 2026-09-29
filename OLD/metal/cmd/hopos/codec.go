//go:build media

package main

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/xinix00/HopOS/metal/v2/cmd/hopos/codecblob"
	"github.com/xinix00/HopOS/metal/v2/driver/codec"
	"github.com/xinix00/HopOS/metal/v2/kern/hopfs"
	"github.com/xinix00/HopOS/metal/v2/kern/slots"
)

// codec.go — het videocodec-blok van het board aanzetten. Dit is compute, geen
// beeld: de node decodeert een stream naar frames in het geheugen van een app
// (Blu-ray erin, frames eruit, encoder in software erachter), en er komt geen
// scherm aan te pas. Alleen in de media-smaak; codec_off.go is de rest.
//
// De arena is fysiek geheugen dat de codec zelf mag zien: page tables,
// firmware, en het geheugen dat die firmware onderweg opvraagt. Dat laatste
// domineert: een 4K HEVC-decoder houdt zijn referentieframes vast, zestien
// stuks. In 10 bit (UHD Blu-ray, waar de mediastack voor is) is dat 24MB per
// frame; met 256MB liep de arena leeg en eindigde dat in een MMU ABORT. 768MB
// draagt één 4K10-stream met werkruimte en is op een node van 16GB nog geen
// twintigste. Wie alleen 8 bit decodeert (12MB per frame) kan met
// hopos.codec=256 toe. Nul zet de dienst uit.
const defaultCodecArenaMB = 768

// codecClipPath is waar een meetbundel zijn teststream neerzet.
const codecClipPath = "/data/clip.hevc"

// codecFirmwareDir is waar de codec-blobs staan: één binary per codec
// (hevcdec.fwb, h264dec.fwb, ...), elk zo'n 300KB. Ze horen NIET in het
// kernimage: dan zou elke kern-flip ze meesjouwen terwijl ze bij de node
// horen, niet bij de kern. Ze zijn bovendien van CIX, niet van ons.
const codecFirmwareDir = "/firmware"

// codecProbe neemt het codec-blok van dit board in gebruik: stroom aan via het
// board, daarna de driver uit media/. Gezet door media_<board>.go (o6n: de
// Linlon V8); nil op een board zonder codec-ijzer. Het is een haak in cmd en
// geen methode op het board-contract om dezelfde reden als bij de gui: board
// mag media niet importeren, cmd is het knooppunt (indeling.md).
//
// De methode mag duur zijn (stroomdomein aanzetten, resetten, firmware
// laden) en wordt één keer bij het opstarten aangeroepen, niet per sessie. De
// arena is fysiek geheugen dat HOP levert: het board beheert registers, HOP
// beheert DRAM — zo kan een board niet stilletjes een kwart van het RAM
// opeisen.
var codecProbe func(arena uintptr, size uint64, fw codec.FirmwareSource) (codec.Engine, error)

// codecUp zet de codec aan als het board er een heeft en de node er geheugen
// voor over heeft. Alles is zacht: een node zonder codec-ijzer, zonder
// firmware of zonder ruimte draait gewoon door zonder de dienst.
func codecUp(fsys *hopfs.FS) {
	mb := defaultCodecArenaMB
	if v := bootParam("hopos.codec"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			fmt.Printf("codec: ignoring hopos.codec=%q\n", v)
		} else {
			mb = n
		}
	}
	if mb == 0 {
		return
	}
	if codecProbe == nil {
		return // dit board heeft geen codec-blok; niets te melden
	}
	if fsys == nil && codecblob.Firmware("hevcdec") == nil {
		fmt.Println("codec: no volume to load firmware from — video codec stays off")
		return
	}
	// Een meetbundel draagt zijn eigen teststream mee; zet hem klaar waar het
	// meetinstrument hem verwacht, zodat één bootparam genoeg is.
	if c := codecblob.Clip(); c != nil && fsys != nil {
		if n, _, err := fsys.Stat(codecClipPath); err != nil || n != uint64(len(c)) {
			if err := fsys.WriteAt(codecClipPath, 0, c); err != nil {
				fmt.Printf("codec: cannot stage the test clip: %v\n", err)
			} else {
				fmt.Printf("codec: test clip staged at %s (%d KB)\n", codecClipPath, len(c)>>10)
			}
		}
	}
	size := uint64(mb) << 20
	arena, err := slots.ReserveDevice(size)
	if err != nil {
		fmt.Printf("codec: %v — video codec stays off\n", err)
		return
	}
	engine, err := codecProbe(uintptr(arena), size, codecFirmware{fsys})
	if err != nil || engine == nil {
		if err != nil {
			fmt.Printf("codec: %v — video codec stays off\n", err)
		}
		// De arena teruggeven: hij telt af van wat apps kunnen krijgen, en een
		// blok van honderden MB kwijt zijn omdat het ijzer niet opkwam is zonde.
		slots.ReleaseDevice(uint64(arena), size)
		return
	}
	slots.UseCodec(engine)
}

// codecFirmware leest de codec-binaries van het eigen volume. fs is nil op een
// node zonder volume; dan komt de firmware alleen uit de kernbundel.
type codecFirmware struct{ fs *hopfs.FS }

var errNoVolume = errors.New("codec: no volume to load firmware from")

// Load geeft de firmware voor een codec ("hevcdec"). Elke sessie leest hem
// opnieuw: openen kost eenmalig een paar honderd kilobyte van de NVMe en dat
// weegt niet op tegen een cache die bij elke kern-flip opnieuw gevuld moet
// worden.
func (c codecFirmware) Load(name string) ([]byte, error) {
	path := codecFirmwareDir + "/" + name + ".fwb"
	var size uint64
	var dir bool
	err := errNoVolume
	if c.fs != nil {
		size, dir, err = c.fs.Stat(path)
	}
	if err != nil {
		// Niet op het volume: draagt deze bundel hem mee? Dat is het pad van
		// een meetbundel, niet van een gewone node.
		if b := codecblob.Firmware(name); b != nil {
			fmt.Printf("codec: %s from the kernel bundle (%d KB)\n", name, len(b)>>10)
			return b, nil
		}
		return nil, err
	}
	if dir || size == 0 || size > 4<<20 {
		return nil, fmt.Errorf("codec: %s is not a firmware binary (%d bytes)", path, size)
	}
	buf := make([]byte, size)
	if _, err := c.fs.ReadAt(path, 0, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

var _ codec.FirmwareSource = codecFirmware{}
