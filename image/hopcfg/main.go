// hopcfg leest en herschrijft de node-config (hopos.cfg) RAW — in een
// kaart-image vóór het flashen, of op de kaart/stick zelf erna. Geen mount,
// geen filesystem-code, geen afhankelijkheid van wat het OS van de partitie
// vindt (de Radxa-kaart mount bijvoorbeeld niet overal): één tool voor alles.
//
//	go run image/hopcfg/main.go show    hopos-radxa-zero3.img
//	go run image/hopcfg/main.go replace hopos-radxa-zero3.img mijn-node.cfg
//	sudo go run image/hopcfg/main.go replace /dev/rdisk4 mijn-node.cfg
//	go run image/hopcfg/main.go pad -window 1048576 hopos.cfg
//
// HOE HET WERKT — het config-venster. mkcard legt hopos.cfg met -cfgwindow
// als venster van een vaste maat op de kaart: een magic-kopregel, de echte
// config, en de rest volgeschreven met '#'-commentaarregels ("een hoop loze
// ruimte achter de file", Derek 06-08):
//
//	#HOPCFG1 window=1048576 len=0000000432
//	hopos.node=hop-1
//	...
//	################################################################ (padding)
//
// Drie eigenschappen maken dit raw-patchbaar zonder één regel
// filesystem-kennis:
//
//  1. de padding bestaat uit '#'-regels — voor élke cfg-parser in de boom
//     (fw/bootcfg: strings.Split op \n, '#' = commentaar) is het venster
//     gewoon een geldige config. Nul wijzigingen aan de node-kant.
//  2. mkcard alloceert clusters sequentieel, dus het bestand ligt
//     aaneengesloten in het image; in-place herschrijven van exact
//     `window` bytes verandert niets aan de FAT (grootte gelijk, clusters
//     gelijk, alleen de inhoud).
//  3. de magic-regel begint het bestand en clusters zijn sector-uitgelijnd,
//     dus een scan over 512-uitgelijnde offsets vindt het venster — in een
//     .img én op een raw device (dáár is de uitlijning ook nodig om
//     überhaupt te mogen schrijven).
//
// `len` telt de echte config-bytes ná de kopregel (vaste 10 cijfers, zodat de
// kopregel bij elke herschrijving even lang blijft); `window` is de totale
// venster-maat inclusief kopregel en padding, en moet een 512-voud zijn.
//
// DE LICHEERV DOET OOK MEE, met één extra draai. Dat board heeft geen
// SD-driver, dus zijn cfg is ongecomprimeerd in de kernel ge-embed
// (cmd/hopos/cfgblob) — het venster staat daardoor mídden in de
// monitor-payload van de Sophgo-FIP, op een willekeurige (niet
// sector-uitgelijnde) plek. Twee gevolgen, allebei hier opgelost:
//
//   - de scan zoekt op élke offset (bytes.Index), en schrijven gaat via
//     read-modify-write van het omliggende sector-uitgelijnde bereik;
//   - de FIP draagt checksums: MONITOR_CKSUM (CRC-16/XMODEM over de hele
//     monitor-payload) en PARAM2_CKSUM (zelfde CRC over param2 vanaf byte
//     12 — en MONITOR_CKSUM ligt dáárbinnen). Na een patch rekent replace
//     beide na en schrijft ze terug; laat je dat na, dan weigert de FSBL
//     de kernel. De layout komt 1:1 uit image/licheerv/fiptool.py:
//     param2 = "CVLD02\n\0" + cksum(4) + reserved(4) + 3×16 bytes
//     DDR/BLCP-velden, dan MONITOR_CKSUM/LOADADDR/SIZE/RUNADDR op +48..63,
//     waarbij LOADADDR de byte-offset van de monitor bínnen fip.bin is en
//     fip.bin zelf begint met "CVBL01\n\0".
//
// Zelfde filosofie als mkcard/mkkernel: één bestand, alleen stdlib, werkt op
// elke machine, en `go run` is genoeg.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
)

const (
	magic       = "#HOPCFG1 window="
	defaultWin  = 1 << 20   // 1MiB — configs zijn ~8KB, jobspecs groeien; ruimte is gratis
	scanLimit   = 256 << 20 // hoever show/replace maximaal zoeken (raw devices zijn groot)
	scanChunk   = 4 << 20   // leesblok tijdens de scan
	sectorAlign = 512       // het venster begint altijd op een sectorgrens (FAT-cluster)
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "pad":
		fs := flag.NewFlagSet("pad", flag.ExitOnError)
		win := fs.Int("window", defaultWin, "venstergrootte in bytes (512-voud)")
		fs.Parse(os.Args[2:])
		if fs.NArg() != 1 {
			usage()
		}
		err = pad(fs.Arg(0), *win)
	case "show":
		if len(os.Args) != 3 {
			usage()
		}
		err = show(os.Args[2])
	case "replace":
		if len(os.Args) != 4 {
			usage()
		}
		err = replace(os.Args[2], os.Args[3])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "hopcfg:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  hopcfg pad [-window N] <cfg-bestand>       maak van het bestand een venster van N bytes
  hopcfg show <image|device>                  vind het venster en print de config
  hopcfg replace <image|device> <cfg-bestand> herschrijf het venster in-place`)
	os.Exit(1)
}

// makeWindow bouwt exact win bytes: kopregel + config + '#'-padding. De config
// eindigt altijd op een newline zodat de padding op een eigen regel begint —
// de parsers zijn regel-gebaseerd.
func makeWindow(content []byte, win int) ([]byte, error) {
	if win%sectorAlign != 0 {
		return nil, fmt.Errorf("venster %d is geen %d-voud (raw devices eisen sector-uitgelijnde writes)", win, sectorAlign)
	}
	if len(content) > 0 && content[len(content)-1] != '\n' {
		content = append(append([]byte{}, content...), '\n')
	}
	header := fmt.Sprintf("%s%d len=%010d\n", magic, win, len(content))
	if len(header)+len(content) > win {
		return nil, fmt.Errorf("config van %d bytes past niet in een venster van %d (kies -window groter)", len(content), win)
	}
	buf := make([]byte, 0, win)
	buf = append(buf, header...)
	buf = append(buf, content...)
	// Padding: regels van 64 '#' + newline. Kort genoeg voor élke
	// regel-lezer, en in een editor onmiskenbaar "hier eindigt de config".
	for pad := win - len(buf); pad > 0; pad = win - len(buf) {
		n := 65
		if pad < n {
			n = pad
		}
		line := bytes.Repeat([]byte{'#'}, n)
		line[n-1] = '\n'
		buf = append(buf, line...)
	}
	return buf, nil
}

// parseHeader leest de kopregel: venstergrootte, config-lengte en de lengte
// van de kopregel zelf.
func parseHeader(b []byte) (win, clen, hlen int, ok bool) {
	if !bytes.HasPrefix(b, []byte(magic)) {
		return 0, 0, 0, false
	}
	rest := b[len(magic):]
	sp := bytes.IndexByte(rest, ' ')
	if sp < 1 {
		return 0, 0, 0, false
	}
	win, err := strconv.Atoi(string(rest[:sp]))
	if err != nil || !bytes.HasPrefix(rest[sp:], []byte(" len=")) {
		return 0, 0, 0, false
	}
	digits := rest[sp+5:]
	if len(digits) < 11 || digits[10] != '\n' {
		return 0, 0, 0, false
	}
	clen, err = strconv.Atoi(string(digits[:10]))
	if err != nil {
		return 0, 0, 0, false
	}
	hlen = len(magic) + sp + 5 + 11
	if win < hlen+clen || win%sectorAlign != 0 {
		return 0, 0, 0, false
	}
	return win, clen, hlen, true
}

// strip haalt een eventueel bestaand venster van een config af, zodat pad en
// replace idempotent zijn: een al-gepadde file opnieuw aanbieden is geen fout.
func strip(b []byte) []byte {
	if _, clen, hlen, ok := parseHeader(b); ok && hlen+clen <= len(b) {
		return b[hlen : hlen+clen]
	}
	return b
}

func pad(path string, win int) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	buf, err := makeWindow(strip(src), win)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, buf, 0644); err != nil {
		return err
	}
	fmt.Printf("hopcfg: %s → venster van %d bytes (config %d bytes)\n", path, win, len(strip(buf)))
	return nil
}

// scan zoekt een magic op élke byte-offset (de LicheeRV-cfg zit in de kernel
// en ligt dus niet uitgelijnd) en geeft alle vindplaatsen terug. verify mag de
// kandidaat afkeuren (het cfg-venster eist een parsebare kopregel; de
// FIP-magics niet).
func scan(f *os.File, needle []byte, verify func([]byte) bool) ([]int64, error) {
	var hits []int64
	buf := make([]byte, scanChunk+sectorAlign) // staart-overlap: kopregel/param2-kop < 512
	var off int64
	for off < scanLimit {
		n, err := f.ReadAt(buf, off)
		if n > 0 {
			// i blijft ónder scanChunk: de staart is er alleen zodat een
			// vondst op de chunkgrens volledig te toetsen is — wie hem óók
			// in de volgende ronde telt, ziet hem dubbel.
			for i := 0; i < scanChunk && i < n; {
				k := bytes.Index(buf[i:n], needle)
				if k < 0 || i+k >= scanChunk {
					break
				}
				i += k
				if verify == nil || verify(buf[i:n]) {
					hits = append(hits, off+int64(i))
				}
				i++
			}
		}
		if err == io.EOF || n < len(buf) {
			break
		}
		if err != nil {
			return nil, err
		}
		off += scanChunk
	}
	return hits, nil
}

// find zoekt hét config-venster. Eén hit is het venster; nul of meer dan één
// is een fout die zichzelf uitlegt.
func find(f *os.File) (int64, int, int, int, error) {
	hits, err := scan(f, []byte(magic), func(b []byte) bool {
		_, _, _, ok := parseHeader(b)
		return ok
	})
	if err != nil {
		return 0, 0, 0, 0, err
	}
	switch len(hits) {
	case 0:
		return 0, 0, 0, 0, fmt.Errorf("geen config-venster gevonden (is dit image met mkcard -cfgwindow gebouwd?)")
	case 1:
	default:
		return 0, 0, 0, 0, fmt.Errorf("%d config-vensters gevonden op offsets %v — dat hoort niet, patch geweigerd", len(hits), hits)
	}
	head := make([]byte, sectorAlign)
	if _, err := f.ReadAt(head, hits[0]); err != nil {
		return 0, 0, 0, 0, err
	}
	win, clen, hlen, _ := parseHeader(head)
	return hits[0], win, clen, hlen, nil
}

// writeAligned schrijft data op een willekeurige offset via read-modify-write
// van het omliggende sector-bereik — raw devices accepteren alleen
// sector-uitgelijnde I/O, een .img maakt het niet uit.
func writeAligned(f *os.File, off int64, data []byte) error {
	lo := off &^ (sectorAlign - 1)
	hi := (off + int64(len(data)) + sectorAlign - 1) &^ (sectorAlign - 1)
	buf := make([]byte, hi-lo)
	if n, err := f.ReadAt(buf, lo); err != nil && (err != io.EOF || int64(n) < off+int64(len(data))-lo) {
		return err
	}
	copy(buf[off-lo:], data)
	_, err := f.WriteAt(buf, lo)
	return err
}

func show(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	off, _, clen, hlen, err := find(f)
	if err != nil {
		return err
	}
	content := make([]byte, clen)
	if _, err := f.ReadAt(content, off+int64(hlen)); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "hopcfg: venster op offset %#x, config %d bytes\n", off, clen)
	os.Stdout.Write(content)
	return nil
}

func replace(path, cfgPath string) error {
	src, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("%v (een raw device vraagt sudo, en op macOS het r-device: /dev/rdiskN)", err)
	}
	defer f.Close()
	off, win, _, _, err := find(f)
	if err != nil {
		return err
	}
	buf, err := makeWindow(strip(src), win)
	if err != nil {
		return err
	}
	if err := writeAligned(f, off, buf); err != nil {
		return err
	}
	if err := fipFix(f, off, win); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	fmt.Printf("hopcfg: %s — venster op %#x herschreven met %s (%d bytes config, %d venster)\n",
		path, off, cfgPath, len(strip(buf)), win)
	return nil
}

// crcHqx is CRC-16/XMODEM (poly 0x1021, init 0) — exact binascii.crc_hqx,
// waarmee fiptool.py de FIP-checksums rekent.
func crcHqx(b []byte) uint16 {
	var crc uint16
	for _, c := range b {
		crc ^= uint16(c) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// fipCksum is het 4-byte veld zoals fiptool.image_crc het schrijft: de CRC
// little-endian, gevolgd door de vaste staart 0xFE 0xCA.
func fipCksum(b []byte) []byte {
	crc := crcHqx(b)
	return []byte{byte(crc), byte(crc >> 8), 0xFE, 0xCA}
}

// fipFix herstelt de Sophgo-FIP-checksums als het zojuist herschreven venster
// binnen een MONITOR-payload ligt (de LicheeRV: cfg ge-embed in de kernel).
// Geen FIP in het image (Radxa, Pi's — de cfg is daar een los FAT-bestand):
// stille no-op. De param2-layout staat in de kop van dit bestand.
func fipFix(f *os.File, winOff int64, winLen int) error {
	p2s, err := scan(f, []byte("CVLD02\n\x00"), func(b []byte) bool {
		if len(b) < 64 {
			return false
		}
		// Zelfde plausibiliteitstoets als licheerv-agent.sh: het echte
		// param2-blok heeft een DRAM-runaddr; de string komt óók los in
		// fiptool-kopieën voor.
		ra := le32(b[60:64])
		return ra == 0 || (ra >= 0x8000_0000 && ra < 0x9000_0000)
	})
	if err != nil || len(p2s) == 0 {
		return err
	}
	if len(p2s) > 1 {
		return fmt.Errorf("fip: %d param2-blokken gevonden op %v, verwacht 1 — checksum-fix geweigerd", len(p2s), p2s)
	}
	p2 := p2s[0]
	param2 := make([]byte, 4096)
	if _, err := f.ReadAt(param2, p2); err != nil {
		return err
	}
	fips, err := scan(f, []byte("CVBL01\n\x00"), nil)
	if err != nil {
		return err
	}
	var fipStart int64 = -1
	for _, s := range fips {
		if s <= p2 && s > fipStart {
			fipStart = s
		}
	}
	if fipStart < 0 {
		return fmt.Errorf("fip: wel een param2 (CVLD02) op %#x maar geen fip-kop (CVBL01) ervóór", p2)
	}
	monOff := fipStart + int64(le32(param2[52:56]))
	monSize := int(le32(param2[56:60]))
	if winOff < monOff || winOff+int64(winLen) > monOff+int64(monSize) {
		fmt.Fprintf(os.Stderr, "hopcfg: fip aanwezig maar het venster ligt buiten de monitor-payload — geen checksum-werk\n")
		return nil
	}
	monitor := make([]byte, monSize)
	if _, err := f.ReadAt(monitor, monOff); err != nil {
		return err
	}
	copy(param2[48:52], fipCksum(monitor))
	// PARAM2_CKSUM dekt param2 vanaf byte 12 — mét de zojuist ingevulde
	// MONITOR_CKSUM, dus eerst het veld, dan de blok-checksum.
	copy(param2[8:12], fipCksum(param2[12:]))
	if err := writeAligned(f, p2+8, param2[8:52]); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "hopcfg: fip-monitor op %#x (%d bytes) — MONITOR_CKSUM en PARAM2_CKSUM bijgewerkt\n", monOff, monSize)
	return nil
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
