// mkcard schrijft een compleet, dd-baar SD-kaart-image voor de LicheeRV Nano:
// MBR + één FAT16-bootpartitie met de bestanden erin. Eén bestand naar de
// kaart, klaar — geen donor-image van 1,6GB meer, geen fip.bin handmatig
// kopiëren, geen mount-gedoe.
//
//	mkcard -o hopos-licheerv.img fip.bin[=naam] [meer bestanden...]
//
// Waarom zelf een FAT bouwen en niet newfs_msdos + hdiutil: dat vraagt een
// Mac, root-rechten en een loop-device, en het resultaat is niet
// reproduceerbaar. Dit is ~200 regels bit-schuiven, werkt op elke machine
// (ook in CI) en levert byte-voor-byte hetzelfde image — zelfde keuze als
// image/mkkernel en image/licheerv/mkmonitor.
//
// De geometrie is die van het Sipeed donor-image, want dat is wat de BROM en
// de FSBL van dit silicium aantoonbaar lezen (gemeten 30-07): partitie 1 van
// het type 0x0C (FAT32 LBA — de vendor gebruikt dat label óók voor een FAT16),
// beginnend op LBA 1, met 512-byte sectoren, 2KB-clusters, 2 FAT-kopieën en
// 512 root-entries. Alleen de grootte is van ons: de rootfs van de vendor
// (~1,6GB, met een Linux-kernel die HopOS nooit boot) laten we weg.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	sectorSize   = 512
	partStartLBA = 1 // wat de BROM verwacht (donor-geometrie)

	// FAT16-geometrie, 1:1 de donor: 2KB-clusters, 4 gereserveerde sectoren
	// (boot + 3), 2 FAT-kopieën, 512 root-entries.
	secPerClus   = 4
	reservedSecs = 4
	numFATs      = 2
	rootEntries  = 512
	mediaByte    = 0xF8

	dirEntrySize = 32
)

func main() {
	out := flag.String("o", "hopos-licheerv.img", "uitvoer-image")
	sizeMB := flag.Int("size", 64, "grootte van de bootpartitie in MB")
	label := flag.String("label", "boot", "volumelabel (max 11 tekens)")
	flag.Parse()
	if flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: mkcard [-o img] [-size MB] <bestand[=naam]>...")
		os.Exit(1)
	}

	img, err := build(*sizeMB, *label, flag.Args())
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkcard:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, img, 0644); err != nil {
		fmt.Fprintln(os.Stderr, "mkcard:", err)
		os.Exit(1)
	}
	fmt.Printf("mkcard: %s (%d MB) — %d bestand(en)\n", *out, len(img)>>20, flag.NArg())
}

// build zet het hele image in geheugen: MBR, dan de FAT16-partitie.
func build(sizeMB int, label string, args []string) ([]byte, error) {
	partSecs := sizeMB * 1024 * 1024 / sectorSize
	total := (partStartLBA + partSecs) * sectorSize
	img := make([]byte, total)

	writeMBR(img, partSecs)
	part := img[partStartLBA*sectorSize:]
	if err := writeFAT16(part, partSecs, label, args); err != nil {
		return nil, err
	}
	return img, nil
}

// writeMBR zet één partitie-entry (type 0x0C, actief) plus de boot-signature.
// De CHS-velden zijn legacy-vulling; alles wat dit silicium leest is LBA.
func writeMBR(img []byte, partSecs int) {
	e := img[446:462]
	e[0] = 0x80 // actief (zoals de donor)
	e[1], e[2], e[3] = 0x00, 0x02, 0x00
	e[4] = 0x0C // FAT32 LBA — het type dat de vendor ook voor zijn FAT16 zet
	e[5], e[6], e[7] = 0x0A, 0x09, 0x02
	binary.LittleEndian.PutUint32(e[8:], partStartLBA)
	binary.LittleEndian.PutUint32(e[12:], uint32(partSecs))
	img[510], img[511] = 0x55, 0xAA
}

// writeFAT16 bouwt de bootsector, de FAT-kopieën, de rootdirectory en de
// clusterketens van de meegegeven bestanden.
func writeFAT16(p []byte, partSecs int, label string, args []string) error {
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
	bs[0], bs[1], bs[2] = 0xEB, 0x3C, 0x90 // jump (nooit uitgevoerd: de BROM leest de FAT)
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
	binary.LittleEndian.PutUint32(bs[28:], partStartLBA)
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

	next := 2 // eerste vrije cluster
	for i, arg := range args {
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
		nclus := (len(data) + secPerClus*sectorSize - 1) / (secPerClus * sectorSize)
		if next+nclus > clusters+2 {
			return fmt.Errorf("%s past niet: %d clusters nodig, %d vrij",
				name, nclus, clusters+2-next)
		}
		start := next
		copy(p[dataOff+(start-2)*secPerClus*sectorSize:], data)
		for c := range nclus {
			v := uint16(next + c + 1)
			if c == nclus-1 {
				v = 0xFFFF // eind van de keten
			}
			binary.LittleEndian.PutUint16(fat0[(next+c)*2:], v)
		}
		next += nclus
		if err := writeDirEntry(p[rootOff+i*dirEntrySize:], name, start, len(data)); err != nil {
			return err
		}
	}

	// De tweede FAT is een exacte kopie — dat is wat numFATs=2 belooft, en een
	// driver mag er zonder waarschuwing op terugvallen.
	copy(p[(reservedSecs+secPerFAT)*sectorSize:], p[reservedSecs*sectorSize:(reservedSecs+secPerFAT)*sectorSize])
	return nil
}

// writeDirEntry schrijft één 8.3-rootdirectory-entry.
func writeDirEntry(e []byte, name string, cluster, size int) error {
	base, ext, _ := strings.Cut(strings.ToUpper(name), ".")
	if len(base) > 8 || len(ext) > 3 {
		return fmt.Errorf("%q past niet in 8.3 — de BROM leest geen lange namen", name)
	}
	copy(e[0:8], fmt.Sprintf("%-8s", base))
	copy(e[8:11], fmt.Sprintf("%-3s", ext))
	e[11] = 0x20 // archief
	// Vaste tijdstempel: een image dat twee keer bouwen twee verschillende
	// bytes geeft is niet te verifiëren (zelfde reden als -trimpath en
	// gzip -n elders in de build).
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fatTime := uint16(t.Hour()<<11 | t.Minute()<<5 | t.Second()/2)
	fatDate := uint16((t.Year()-1980)<<9 | int(t.Month())<<5 | t.Day())
	binary.LittleEndian.PutUint16(e[14:], fatTime)
	binary.LittleEndian.PutUint16(e[16:], fatDate)
	binary.LittleEndian.PutUint16(e[22:], fatTime)
	binary.LittleEndian.PutUint16(e[24:], fatDate)
	binary.LittleEndian.PutUint16(e[26:], uint16(cluster))
	binary.LittleEndian.PutUint32(e[28:], uint32(size))
	return nil
}
