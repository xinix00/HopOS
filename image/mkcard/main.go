// mkcard schrijft een compleet, dd-baar SD-kaart-image: MBR + één
// FAT16-bootpartitie met de bestanden erin, plus (optioneel) raw blobs op
// vaste byte-offsets vóór de partitie — de plek waar een BootROM zijn
// firmware raw leest. Eén bestand naar de kaart, klaar — geen donor-image
// van gigabytes, geen mount-gedoe.
//
//	mkcard -o hopos-licheerv.img fip.bin[=naam] [meer bestanden...]
//	mkcard -o hopos-radxa-zero3.img -start 32768 -raw donor-boot.bin@32768 \
//	    hopos-radxa.img hopos.cfg extlinux.conf=extlinux/extlinux.conf
//
// Waarom zelf een FAT bouwen en niet newfs_msdos + hdiutil: dat vraagt een
// Mac, root-rechten en een loop-device, en het resultaat is niet
// reproduceerbaar. Dit is een paar honderd regels bit-schuiven, werkt op elke
// machine (ook in CI) en levert byte-voor-byte hetzelfde image — zelfde keuze
// als image/mkkernel en image/licheerv/mkmonitor.
//
// De FAT16-geometrie is die van het Sipeed donor-image, want dat is wat de
// BROM en de FSBL van dat silicium aantoonbaar lezen (gemeten 30-07):
// partitietype 0x0C (FAT32 LBA — de vendor gebruikt dat label óók voor een
// FAT16), 512-byte sectoren, 2KB-clusters, 2 FAT-kopieën en 512 root-entries.
// Datzelfde type 0x0C is meteen waarom macOS en Windows de partitie gewoon
// mounten — op een kaart uit dit gereedschap is hopos.cfg dus ná het flashen
// bewerkbaar, wat met het EFI-getypeerde part2 van een Radxa-donorkaart niet
// kon.
//
// Namen die niet in 8.3 passen (extlinux.conf, bcm2712-rpi-5-b.dtb) krijgen
// VFAT-LFN-entries — U-Boot en de Pi-firmware lezen die allebei (elke
// standaard-kaart heeft ze). De LicheeRV blijft daar bewust buiten: fip.bin
// pást in 8.3, dus dat image krijgt geen enkele LFN-entry en blijft
// byte-identiek aan wat de BROM daar bewezen leest. Subdirectories kunnen één
// niveau diep (extlinux/, overlays/) — dieper heeft geen bootpad nodig.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	sectorSize = 512

	// FAT16-geometrie, 1:1 het Sipeed-donor-image: 2KB-clusters, 4
	// gereserveerde sectoren (boot + 3), 2 FAT-kopieën, 512 root-entries.
	secPerClus   = 4
	reservedSecs = 4
	numFATs      = 2
	rootEntries  = 512
	mediaByte    = 0xF8

	dirEntrySize = 32
	clusterBytes = secPerClus * sectorSize
)

// rawBlob is een bestand dat raw (buiten het filesystem) op een vast
// byte-offset in het image landt — firmware die de BootROM op een vaste LBA
// leest, zoals de Rockchip-idbloader op LBA 64 en u-boot.itb op LBA 16384.
type rawBlob struct {
	path string
	off  int64
}

type rawList []rawBlob

func (r *rawList) String() string { return fmt.Sprint(*r) }

func (r *rawList) Set(s string) error {
	k := strings.LastIndexByte(s, '@')
	if k < 0 {
		return fmt.Errorf("verwacht pad@byteoffset, kreeg %q", s)
	}
	off, err := strconv.ParseInt(s[k+1:], 0, 64)
	if err != nil {
		return fmt.Errorf("offset in %q: %v", s, err)
	}
	if off%sectorSize != 0 {
		return fmt.Errorf("offset %d is niet sector-uitgelijnd (512)", off)
	}
	*r = append(*r, rawBlob{path: s[:k], off: off})
	return nil
}

func main() {
	out := flag.String("o", "card.img", "uitvoer-image")
	sizeMB := flag.Int("size", 64, "grootte van de bootpartitie in MB")
	start := flag.Int("start", 1, "start-LBA van de partitie (donor-geometrie LicheeRV: 1)")
	label := flag.String("label", "boot", "volumelabel (max 11 tekens)")
	volEntry := flag.Bool("vollabel", false, "schrijf het label óók als root-entry (mooie mountnaam; NIET voor de LicheeRV — de BROM-parser is niet van ons)")
	var raws rawList
	flag.Var(&raws, "raw", "blob raw op een vast byte-offset vóór de partitie: pad@offset (herhaalbaar)")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: mkcard [-o img] [-size MB] [-start LBA] [-raw pad@off]... <bestand[=naam]>...")
		os.Exit(1)
	}

	img, err := build(*sizeMB, *start, *label, *volEntry, raws, flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkcard:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, img, 0644); err != nil {
		fmt.Fprintln(os.Stderr, "mkcard:", err)
		os.Exit(1)
	}
	fmt.Printf("mkcard: %s (%d MB totaal, partitie %d MB @ LBA %d) — %d bestand(en)\n",
		*out, len(img)>>20, *sizeMB, *start, flag.NArg())
}

// build zet het hele image in geheugen: MBR, raw blobs, dan de FAT16-partitie.
func build(sizeMB, startLBA int, label string, volEntry bool, raws rawList, args []string) ([]byte, error) {
	partSecs := sizeMB * 1024 * 1024 / sectorSize
	total := (startLBA + partSecs) * sectorSize
	img := make([]byte, total)

	writeMBR(img, startLBA, partSecs)
	for _, r := range raws {
		data, err := os.ReadFile(r.path)
		if err != nil {
			return nil, err
		}
		if r.off < sectorSize {
			return nil, fmt.Errorf("raw blob %s overlapt de MBR (offset %d)", r.path, r.off)
		}
		if r.off+int64(len(data)) > int64(startLBA)*sectorSize {
			return nil, fmt.Errorf("raw blob %s (%d bytes @ %d) steekt in de partitie (LBA %d)",
				r.path, len(data), r.off, startLBA)
		}
		copy(img[r.off:], data)
	}
	part := img[startLBA*sectorSize:]
	if err := writeFAT16(part, startLBA, partSecs, label, volEntry, args); err != nil {
		return nil, err
	}
	return img, nil
}

// writeMBR zet één partitie-entry (type 0x0C, actief) plus de boot-signature.
// De CHS-velden zijn legacy-vulling; alles wat firmware leest is LBA. Bij
// LBA-start 1 zijn het exact de donorbytes van de LicheeRV (bewezen image);
// elders de standaard "kijk naar LBA"-vulling 0xFE/0xFF/0xFF.
func writeMBR(img []byte, startLBA, partSecs int) {
	e := img[446:462]
	e[0] = 0x80 // actief
	e[4] = 0x0C // FAT32 LBA — het type dat vendors óók voor een FAT16 zetten
	if startLBA == 1 {
		e[1], e[2], e[3] = 0x00, 0x02, 0x00
		e[5], e[6], e[7] = 0x0A, 0x09, 0x02
	} else {
		e[1], e[2], e[3] = 0xFE, 0xFF, 0xFF
		e[5], e[6], e[7] = 0xFE, 0xFF, 0xFF
	}
	binary.LittleEndian.PutUint32(e[8:], uint32(startLBA))
	binary.LittleEndian.PutUint32(e[12:], uint32(partSecs))
	img[510], img[511] = 0x55, 0xAA
}

// node is één bestand of subdirectory in de FAT. Eén niveau diep is genoeg
// voor elk bootpad (extlinux/, overlays/).
type node struct {
	name     string // echte naam, case behouden (LFN draagt hem als dat moet)
	data     []byte // bestandsinhoud; nil voor een directory
	children []*node
	cluster  int // startcluster (toegewezen tijdens alloc)
}

// writeFAT16 bouwt de bootsector, de FAT-kopieën, de rootdirectory, de
// subdirectories en de clusterketens van de meegegeven bestanden.
func writeFAT16(p []byte, startLBA, partSecs int, label string, volEntry bool, args []string) error {
	rootSecs := rootEntries * dirEntrySize / sectorSize

	// sectoren per FAT: elke cluster kost 2 bytes in de tabel. Iteratief,
	// want de FAT-grootte bepaalt zelf hoeveel dataclusters er overblijven.
	secPerFAT := 1
	for {
		dataSecs := partSecs - reservedSecs - numFATs*secPerFAT - rootSecs
		clusters := dataSecs / secPerClus
		need := (clusters+2)*2/sectorSize + 1
		if need <= secPerFAT {
			break
		}
		secPerFAT = need
	}
	dataSecs := partSecs - reservedSecs - numFATs*secPerFAT - rootSecs
	clusters := dataSecs / secPerClus
	// FAT16 is per definitie 4085..65524 clusters; buiten dat bereik leest
	// een driver hem als FAT12 of FAT32 en vindt hij niets.
	if clusters < 4085 || clusters > 65524 {
		return fmt.Errorf("%d clusters valt buiten FAT16 (4085..65524) — kies een andere -size", clusters)
	}

	bs := p[:sectorSize]
	bs[0], bs[1], bs[2] = 0xEB, 0x3C, 0x90 // jump (nooit uitgevoerd: firmware parseert de FAT)
	copy(bs[3:11], "HOPOS   ")
	binary.LittleEndian.PutUint16(bs[11:], sectorSize)
	bs[13] = secPerClus
	binary.LittleEndian.PutUint16(bs[14:], reservedSecs)
	bs[16] = numFATs
	binary.LittleEndian.PutUint16(bs[17:], rootEntries)
	if partSecs < 0x10000 {
		binary.LittleEndian.PutUint16(bs[19:], uint16(partSecs))
	} else {
		binary.LittleEndian.PutUint32(bs[32:], uint32(partSecs))
	}
	bs[21] = mediaByte
	binary.LittleEndian.PutUint16(bs[22:], uint16(secPerFAT))
	binary.LittleEndian.PutUint16(bs[24:], 32) // sectoren/spoor (legacy)
	binary.LittleEndian.PutUint16(bs[26:], 2)  // koppen (legacy)
	binary.LittleEndian.PutUint32(bs[28:], uint32(startLBA))
	bs[36] = 0x80 // drive
	bs[38] = 0x29 // extended boot signature: volume-ID + label volgen
	binary.LittleEndian.PutUint32(bs[39:], 0x484F5000)
	copy(bs[43:54], fmt.Sprintf("%-11s", label))
	copy(bs[54:62], "FAT16   ")
	bs[510], bs[511] = 0x55, 0xAA

	fat0 := p[reservedSecs*sectorSize:]
	rootOff := (reservedSecs + numFATs*secPerFAT) * sectorSize
	dataOff := rootOff + rootSecs*sectorSize

	// Entry 0/1 zijn gereserveerd: mediabyte + eind-van-keten.
	binary.LittleEndian.PutUint16(fat0[0:], 0xFF00|uint16(mediaByte))
	binary.LittleEndian.PutUint16(fat0[2:], 0xFFFF)

	// De boom uit de argumenten: bestand[=naam], met hooguit één '/' in de
	// naam. In argument-volgorde, en een directory ontstaat waar hij het
	// eerst nodig is — dat houdt de clustertoewijzing deterministisch.
	root := &node{}
	dirs := map[string]*node{}
	for _, arg := range args {
		path, name := arg, ""
		if k := strings.IndexByte(arg, '='); k >= 0 {
			path, name = arg[:k], arg[k+1:]
		}
		if name == "" {
			name = filepath.Base(path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		parent := root
		if dir, base, found := strings.Cut(name, "/"); found {
			if strings.ContainsRune(base, '/') {
				return fmt.Errorf("%q: dieper dan één subdirectory heeft geen bootpad nodig", name)
			}
			if dirs[dir] == nil {
				dirs[dir] = &node{name: dir}
				root.children = append(root.children, dirs[dir])
			}
			parent, name = dirs[dir], base
		}
		parent.children = append(parent.children, &node{name: name, data: data})
	}

	// Clustertoewijzing: depth-first in boomvolgorde. Een directory krijgt
	// één cluster (64 entries — ruim voor elk bootpad) vóór zijn kinderen.
	next := 2
	alloc := func(data []byte) (int, int, error) {
		nclus := (len(data) + clusterBytes - 1) / clusterBytes
		if next+nclus > clusters+2 {
			return 0, 0, fmt.Errorf("past niet: %d clusters nodig, %d vrij", nclus, clusters+2-next)
		}
		start := next
		copy(p[dataOff+(start-2)*clusterBytes:], data)
		for c := range nclus {
			v := uint16(start + c + 1)
			if c == nclus-1 {
				v = 0xFFFF // eind van de keten
			}
			binary.LittleEndian.PutUint16(fat0[(start+c)*2:], v)
		}
		next += nclus
		return start, nclus, nil
	}
	var walk func(n *node) error
	walk = func(n *node) error {
		for _, c := range n.children {
			data := c.data
			if c.data == nil {
				data = make([]byte, clusterBytes) // de directory-tabel; inhoud volgt
			}
			start, _, err := alloc(data)
			if err != nil {
				return fmt.Errorf("%s %v", c.name, err)
			}
			c.cluster = start
			if c.data == nil {
				if err := walk(c); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root); err != nil {
		return err
	}

	// Directory-tabellen schrijven. De root heeft zijn vaste venster; een
	// subdirectory woont in zijn cluster en begint met "." en "..".
	rootDir := p[rootOff : rootOff+rootEntries*dirEntrySize]
	used := 0
	if volEntry {
		e := rootDir[:dirEntrySize]
		copy(e[0:11], fmt.Sprintf("%-11s", strings.ToUpper(label)))
		e[11] = 0x08 // volume-label
		stampTimes(e)
		used = 1
	}
	if err := writeDir(rootDir, &used, rootEntries, root.children); err != nil {
		return fmt.Errorf("root: %v", err)
	}
	for _, d := range root.children {
		if d.data != nil {
			continue
		}
		tbl := p[dataOff+(d.cluster-2)*clusterBytes : dataOff+(d.cluster-1)*clusterBytes]
		dot := tbl[:dirEntrySize]
		copy(dot[0:11], ".          ")
		dot[11] = 0x10
		stampTimes(dot)
		binary.LittleEndian.PutUint16(dot[26:], uint16(d.cluster))
		dotdot := tbl[dirEntrySize : 2*dirEntrySize]
		copy(dotdot[0:11], "..         ")
		dotdot[11] = 0x10
		stampTimes(dotdot)
		// ".."-cluster 0 = de root, per FAT-conventie.
		n := 2
		if err := writeDir(tbl, &n, clusterBytes/dirEntrySize, d.children); err != nil {
			return fmt.Errorf("%s/: %v", d.name, err)
		}
	}

	// De tweede FAT is een exacte kopie — dat is wat numFATs=2 belooft, en een
	// driver mag er zonder waarschuwing op terugvallen.
	copy(p[(reservedSecs+secPerFAT)*sectorSize:], p[reservedSecs*sectorSize:(reservedSecs+secPerFAT)*sectorSize])
	return nil
}

// writeDir schrijft de entries van één directory-tabel: per kind eventueel
// LFN-entries en dan de 8.3-entry. *used is de eerstvolgende vrije slot.
func writeDir(tbl []byte, used *int, capacity int, children []*node) error {
	seen := map[string]bool{}
	for _, c := range children {
		short, needLFN, err := shortName(c.name, seen)
		if err != nil {
			return err
		}
		n := 1
		var lfn []uint16
		if needLFN {
			lfn = utf16.Encode([]rune(c.name))
			n += (len(lfn) + 12) / 13
		}
		if *used+n > capacity {
			return fmt.Errorf("%q past niet meer: %d entries nodig, %d vrij", c.name, n, capacity-*used)
		}
		if needLFN {
			writeLFN(tbl[*used*dirEntrySize:], lfn, short)
		}
		e := tbl[(*used+n-1)*dirEntrySize:]
		copy(e[0:11], short[:])
		e[11] = 0x20 // archief
		if c.data == nil {
			e[11] = 0x10 // directory
		}
		stampTimes(e)
		binary.LittleEndian.PutUint16(e[26:], uint16(c.cluster))
		if c.data != nil {
			binary.LittleEndian.PutUint32(e[28:], uint32(len(c.data)))
		}
		*used += n
	}
	return nil
}

// shortName geeft de 11-byte 8.3-vorm van een naam. Past hij niet (te lang,
// meer punten, rare tekens), dan komt er een uniek NAAM~N-alias en meldt de
// tweede uitkomst dat er LFN-entries vóór moeten.
func shortName(name string, seen map[string]bool) (short [11]byte, needLFN bool, err error) {
	up := strings.ToUpper(name)
	base, ext, _ := strings.Cut(up, ".")
	fits := len(base) <= 8 && len(base) > 0 && len(ext) <= 3 &&
		strings.Count(up, ".") <= 1 && valid83(base) && valid83(ext)
	if !fits {
		// Alias: de eerste 6 bruikbare tekens + ~N, extensie na de láátste punt.
		base = sanitize83(strings.TrimSuffix(up, filepath.Ext(up)))
		ext = sanitize83(strings.TrimPrefix(filepath.Ext(up), "."))
		if len(base) > 6 {
			base = base[:6]
		}
		if len(ext) > 3 {
			ext = ext[:3]
		}
		for n := 1; ; n++ {
			cand := fmt.Sprintf("%s~%d", base, n)
			if !seen[cand+"."+ext] {
				base = cand
				break
			}
			if n > 99 {
				return short, false, fmt.Errorf("%q: geen vrij 8.3-alias", name)
			}
		}
		needLFN = true
	}
	if seen[base+"."+ext] {
		return short, false, fmt.Errorf("%q: 8.3-naam %s.%s botst met een eerder bestand", name, base, ext)
	}
	seen[base+"."+ext] = true
	copy(short[:], fmt.Sprintf("%-8s%-3s", base, ext))
	return short, needLFN, nil
}

func valid83(s string) bool {
	for _, r := range s {
		ok := r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("!#$%&'()-@^_`{}~", r)
		if !ok {
			return false
		}
	}
	return true
}

func sanitize83(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'()-@^_`{}~", r) {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		b.WriteByte('_')
	}
	return b.String()
}

// writeLFN schrijft de VFAT-lange-naam-entries (attr 0x0F) die vóór een
// 8.3-entry horen: 13 UTF-16-tekens per entry, in omgekeerde volgorde, de
// eerste met sequentiebit 0x40, elk met de checksum van het 8.3-alias.
func writeLFN(tbl []byte, name []uint16, short [11]byte) {
	var sum byte
	for _, c := range short {
		sum = (sum&1)<<7 + sum>>1 + c
	}
	// Terminator + 0xFFFF-vulling alléén als de laatste entry ruimte over
	// heeft: een naam van exact 13·n tekens krijgt per VFAT-spec géén
	// terminator (extlinux.conf is er zo een — 13 tekens). Eén entry te veel
	// schrijven laat de 8.3-entry over de eerste LFN-helft heen vallen.
	padded := append([]uint16{}, name...)
	if len(padded)%13 != 0 {
		padded = append(padded, 0)
		for len(padded)%13 != 0 {
			padded = append(padded, 0xFFFF)
		}
	}
	n := len(padded) / 13
	for i := range n {
		e := tbl[(n-1-i)*dirEntrySize:]
		seq := byte(i + 1)
		if i == n-1 {
			seq |= 0x40
		}
		e[0] = seq
		e[11] = 0x0F
		e[13] = sum
		chunk := padded[i*13:]
		for j, off := range []int{1, 3, 5, 7, 9, 14, 16, 18, 20, 22, 24, 28, 30} {
			binary.LittleEndian.PutUint16(e[off:], chunk[j])
		}
	}
}

// stampTimes zet de vaste tijdstempel: een image dat twee keer bouwen twee
// verschillende bytes geeft is niet te verifiëren (zelfde reden als -trimpath
// en gzip -n elders in de build).
func stampTimes(e []byte) {
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fatTime := uint16(t.Hour()<<11 | t.Minute()<<5 | t.Second()/2)
	fatDate := uint16((t.Year()-1980)<<9 | int(t.Month())<<5 | t.Day())
	binary.LittleEndian.PutUint16(e[14:], fatTime)
	binary.LittleEndian.PutUint16(e[16:], fatDate)
	binary.LittleEndian.PutUint16(e[22:], fatTime)
	binary.LittleEndian.PutUint16(e[24:], fatDate)
}
