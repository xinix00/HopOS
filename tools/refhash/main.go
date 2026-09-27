// refhash geeft per beeld dezelfde som die codecdemo op de node print, maar
// dan van ffmpeg's decoder. Zo is de uitvoer van de VPU te toetsen zonder een
// beeld van de node af te halen: 6MB per frame over het net kost meer dan het
// hele experiment waard is, en een som van vier beelden zegt genoeg.
//
//	go run tools/refhash/main.go clip.hevc 1920 1080 [p010le|nv12]   (cwd = repo-root)
//
// De som is FNV-1a over de ZICHTBARE bytes: eerst het lumavlak, dan het
// chromavlak, allebei zonder de opvulling die een buffer op het ijzer wél
// heeft. p010le legt de 10 bits links in het 16-bit woord, precies zoals de
// Linlon V8 ze aflevert (gemeten 22-09: byte voor byte gelijk).
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "gebruik: refhash <stream> <breedte> <hoogte> [p010le|nv12]")
		os.Exit(64)
	}
	path := os.Args[1]
	w, err1 := strconv.Atoi(os.Args[2])
	h, err2 := strconv.Atoi(os.Args[3])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		fmt.Fprintln(os.Stderr, "refhash: breedte en hoogte moeten getallen zijn")
		os.Exit(64)
	}
	pix := "p010le"
	if len(os.Args) > 4 {
		pix = os.Args[4]
	}
	bpp := 2
	if pix == "nv12" {
		bpp = 1
	}
	frame := w * h * 3 * bpp / 2

	cmd := exec.Command("ffmpeg", "-v", "error", "-i", path,
		"-pix_fmt", pix, "-f", "rawvideo", "-")
	out, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(os.Stderr, "refhash:", err)
		os.Exit(1)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "refhash:", err)
		os.Exit(1)
	}

	r := bufio.NewReaderSize(out, 1<<20)
	buf := make([]byte, frame)
	for n := 1; ; n++ {
		if _, err := io.ReadFull(r, buf); err != nil {
			fmt.Printf("total %d frames\n", n-1)
			_ = cmd.Wait()
			return
		}
		var sum uint64 = 14695981039346656037
		for _, c := range buf {
			sum = (sum ^ uint64(c)) * 1099511628211
		}
		if n <= 4 || n%50 == 0 {
			fmt.Printf("frame %d %dx%d hash %016x\n", n, w, h, sum)
		}
	}
}
