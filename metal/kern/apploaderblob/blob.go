//go:build embedloader

// Package apploaderblob draagt de universele apploader-image ín de node-binary:
// wat HOP in élk slot laadt om de app zijn eigen image te laten downloaden
// (metal/app/apploader). Ingebakken i.p.v. van een URL gehaald, zodat de node
// self-contained is — geen externe afhankelijkheid, geen config, geen
// storm-fetch. Bouw met -tags embedloader nadat image/*-run.sh de
// board-passende apploader hierheen heeft gebouwd (apploader.elf.gz, gitignored).
//
// Gecomprimeerd ingebakken (gzip -9: 8,4 → 3,1MB, gemeten 14-07): de blob zit
// 6× in de zelfkiezende Altra-PE en 1× in elke Pi-image, dus elke MB telt er
// zesvoudig. Eén keer lazy uitgepakt in de kern-heap (eerste jobstart) en
// daarna gedeeld door alle starts — de heap heeft de ruimte (piek 14MB van
// 64MB, gemeten), de flash/PE niet.
package apploaderblob

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/binary"
	"io"
	"sync"
)

//go:embed apploader.elf.gz
var packed []byte

var (
	once   sync.Once
	loader []byte
)

// Loader geeft de uitgepakte apploader-image (nil als het uitpakken faalt —
// StartLoader weigert dan luid, net als bij een niet-ingebakken loader).
func Loader() []byte {
	once.Do(func() {
		if len(packed) < 8 {
			return
		}
		zr, err := gzip.NewReader(bytes.NewReader(packed))
		if err != nil {
			return
		}
		// De gzip-trailer (ISIZE, laatste 4 bytes) draagt de uitgepakte maat:
		// één exacte allocatie i.p.v. de verdubbelende groei van io.ReadAll
		// (die joeg de kern-heap-piek ~10MB extra op, gemeten 14-07). Liegt
		// de trailer (corrupt artifact), dan faalt ReadFull of de EOF-check
		// hieronder — loader blijft dan nil.
		img := make([]byte, binary.LittleEndian.Uint32(packed[len(packed)-4:]))
		if _, err := io.ReadFull(zr, img); err != nil {
			return
		}
		// Tot EOF doorlezen: dat laat gzip de CRC van de stream verifiëren.
		if _, err := zr.Read(make([]byte, 1)); err != io.EOF {
			return
		}
		loader = img
	})
	return loader
}
