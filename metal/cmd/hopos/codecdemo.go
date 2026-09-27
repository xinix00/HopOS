//go:build media

package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xinix00/HopOS/metal/v2/cpu/memattr"
	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/codec"
	"github.com/xinix00/HopOS/metal/v2/kern/hopfs"
	"github.com/xinix00/HopOS/metal/v2/kern/slots"
	"github.com/xinix00/HopOS/metal/v2/media/driver/vpu/mve"
)

// codecdemo.go — het meetinstrument voor de codec-bring-up. Eén bestand door
// de decoder en de frames naar schijf, met de tijd erbij:
//
//	hopos.codecdemo=/data/clip.hevc
//
// Het bewijst op ijzer wat we op de ontwikkelmachine alleen tegen een model
// konden bewijzen: dat de firmware start, dat de page tables kloppen, dat de
// frames werkelijk in ons geheugen landen en hoe snel dat gaat. Vandaar dat
// het geen app is maar een pad in HOP zelf: geen ABI, geen slot, geen kooi —
// als dit werkt, ligt de fout daarna nooit meer hier.
//
// De codec komt uit de bestandsnaam: .h264/.264, .hevc/.265, .av1, .vp9,
// .mpeg2/.m2v, .vc1, .jpg. Twee knoppen erbij:
//
//	hopos.codecdemo.pixel=p010|nv12   uitvoerformaat (default p010)
//	hopos.codecdemo.yuv=1             frames ook naar <pad>.yuv wegschrijven
//
// Zonder die laatste blijft de meting de DECODER meten. Een 1080p-beeld per
// regel naar NVMe schrijven kost meer tijd dan het decoderen ervan, en dan
// meet je je opslag. Elke frame krijgt wél een hash op de console: daarmee is
// de uitvoer te vergelijken met wat ffmpeg op dezelfde stream maakt, zonder
// dat er een byte van de node af hoeft.
const (
	codecDemoChunk   = 256 << 10 // bitstream-hap per keer (default)
	codecDemoBuffers = 12        // framebuffers die we de decoder lenen
	codecDemoArenaMB = 320       // in- en uitvoerbuffers samen (4K P010 = 25MB/beeld)

	// Zo lang wacht het meetinstrument hoogstens op een teken van leven van
	// de hardware. Elk event zet de klok terug: een lange clip mag rustig
	// langer duren, een stilgevallen firmware niet.
	codecDemoDeadline = 20 * time.Second

	// Zoveel invoerbuffers tegelijk bij de decoder; zie de pijplijn hieronder.
	codecDemoInBufs = 4
)

func codecDemo(path string) {
	fs := slots.FS()
	if fs == nil || !slots.HasCodec() {
		fmt.Println("codecdemo: needs both a volume and codec hardware")
		return
	}
	kind := codecFromName(path)
	if kind == codec.Unknown {
		fmt.Printf("codecdemo: cannot tell the codec from %q\n", path)
		return
	}
	pixel := codec.P010
	if bootParam("hopos.codecdemo.pixel") == "nv12" {
		pixel = codec.NV12
	}
	toDisk := bootParam("hopos.codecdemo.yuv") == "1"
	// hopos.codecdemo.chunk=<KB>: hoe groot de happen bitstream zijn. Een hap
	// die middenin een beeld eindigt geeft een KAPOT beeld — de firmware
	// stikt niet, hij meldt CORRUPT en gaat door. Met deze knop is dat te
	// meten: één hap voor het hele bestand hoort nul corrupte beelden te
	// geven, 26 happen gaven er 25 (gemeten 22-09).
	//
	// De invoerbuffers staan vooraan in de demo-arena, de beeldbuffers
	// erachter. Meer dan de helft voor invoer laat geen 4K-beelden meer over.
	const chunkMaxKB = codecDemoArenaMB << 10 / 2 / codecDemoInBufs
	chunk := codecDemoChunk
	if v := bootParam("hopos.codecdemo.chunk"); v != "" {
		if kb, err := strconv.Atoi(v); err == nil && kb > 0 {
			if kb > chunkMaxKB {
				fmt.Printf("codecdemo: chunk capped at %d KB (the demo arena is %d MB)\n", chunkMaxKB, codecDemoArenaMB)
				kb = chunkMaxKB
			}
			chunk = kb << 10
		}
	}
	// Hashen kost meer tijd dan decoderen (6MB per beeld byte voor byte),
	// dus standaard uit: anders meet de fps ons eigen optelwerk.
	doHash := bootParam("hopos.codecdemo.hash") == "1"
	size, dir, err := fs.Stat(path)
	if err != nil || dir || size == 0 {
		fmt.Printf("codecdemo: %s: %v\n", path, err)
		return
	}

	// Eigen geheugen voor de buffers: de arena van de driver is van de
	// firmware, en de app-partities zijn van de taken. Dit blok is van HOP
	// zolang de node draait — het meetinstrument geeft het niet terug, want
	// een tweede meting in dezelfde boot wil hetzelfde blok.
	base, err := slots.ReserveDevice(codecDemoArenaMB << 20)
	if err != nil {
		fmt.Printf("codecdemo: %v\n", err)
		return
	}
	// Dezelfde reden als bij de arena van de driver: de codec is niet
	// cache-coherent, dus onze bitstream en zijn frames moeten allebei
	// rechtstreeks in DRAM staan.
	if err := memattr.NormalNC(uintptr(base), codecDemoArenaMB<<20); err != nil {
		fmt.Printf("codecdemo: cannot make the buffers uncached: %v\n", err)
		return
	}

	// Elk bericht op de console zetten was onmisbaar toen de firmware zweeg,
	// maar het is nu de traagste schakel in de keten én het duwt elke andere
	// regel uit de consolering. Dus: alleen op verzoek.
	if bootParam("hopos.codecdemo.trace") == "1" {
		mve.Trace = func(code uint16, body []byte) {
			fmt.Printf("codecdemo: firmware says %d (%d bytes)\n", code, len(body))
		}
		defer func() { mve.Trace = nil }()
	}

	ses, err := slots.OpenCodec(0, codec.Config{Codec: kind, Dir: codec.Decode, Pixel: pixel})
	if err != nil {
		fmt.Printf("codecdemo: open %s: %v\n", kind, err)
		return
	}
	defer slots.CloseCodec(ses)

	// Een pijplijn van invoerbuffers, geen enkele. Een decoder geeft zijn
	// eerste hap pas terug als hij hem echt verwerkt heeft, en dat kan hij
	// niet vóór hij beeldbuffers heeft — dus wie op de teruggave wacht
	// voordat hij verder voert, wacht voor altijd.
	var inFree []codec.Buffer
	for i := 0; i < codecDemoInBufs; i++ {
		inFree = append(inFree, codec.Buffer{
			PA:   uintptr(base) + uintptr(i*chunk),
			Size: uint64(chunk),
		})
	}
	pool := uintptr(base) + uintptr(codecDemoInBufs*chunk)
	poolEnd := uintptr(base) + codecDemoArenaMB<<20
	out := path + ".yuv"
	if toDisk {
		_ = fs.Truncate(out, 0)
	}

	// Een waakhond. Dit pad praat met firmware die we voor het eerst
	// aanroepen, en dat kan stilvallen op een manier waar geen enkel event
	// meer uit komt. Zonder waakhond houdt het meetinstrument dan de hele
	// node tegen — gemeten 22-09: de agent kwam niet op en de watchdog moest
	// eraan te pas komen.
	deadline := time.Now().Add(codecDemoDeadline)

	var (
		fed, frames uint64
		offset, put uint64 // gelezen uit de bitstream, geschreven naar de yuv
		layout      codec.Layout
		start       = time.Now()
		buf         = make([]byte, chunk)
		free        []codec.Buffer
		busy        int
		eos, done   bool
		skipped     int
	)
	for {
		if time.Now().After(deadline) {
			fmt.Printf("codecdemo: gave up after %v without an event — %d frames, %d input buffer(s) outstanding, layout %dx%d\n",
				codecDemoDeadline, frames, busy, layout.Width, layout.Height)
			if st, ok := slots.CodecEngine().(codec.Stateful); ok {
				fmt.Printf("codecdemo: hardware says %s\n", st.State())
			}
			return
		}
		// Bitstream bijvoeren zolang er een vrije invoerbuffer is.
		for len(inFree) > 0 && !eos && !done {
			in := inFree[0]
			inFree = inFree[1:]
			n := uint64(len(buf))
			if offset+n > size {
				n = size - offset
			}
			if n > 0 {
				if _, err := fs.ReadAt(path, offset, buf[:n]); err != nil {
					fmt.Printf("codecdemo: read: %v\n", err)
					return
				}
				dev.Copy(in.PA, buf[:n])
				offset += n
				fed += n
			}
			flag := codec.Flag(0)
			if offset >= size {
				flag, eos = codec.EOS, true
			}
			if err := ses.Feed(in, n, flag, offset); err != nil {
				fmt.Printf("codecdemo: feed: %v\n", err)
				return
			}
			busy++
		}
		// Lege framebuffers aanbieden zodra we weten hoe groot ze moeten zijn.
		for layout.FrameSize > 0 && len(free) > 0 && !done {
			b := free[0]
			free = free[1:]
			if err := ses.Offer(b); err != nil {
				fmt.Printf("codecdemo: offer: %v\n", err)
				return
			}
		}
		ev, ok := ses.Poll()
		if !ok {
			// Na Done pas stoppen als de wachtrij écht leeg is: de laatste
			// beelden staan er dan vaak nog in, en die horen bij de stream.
			if done {
				break
			}
			// Niets te doen: even wachten in plaats van de core opstoken.
			// De echte dienst hangt straks aan de interruptlijn (INTID 358);
			// hier pollen we, want dit pad moet ook werken vóór die lijn
			// bedraad is.
			time.Sleep(200 * time.Microsecond)
			continue
		}
		deadline = time.Now().Add(codecDemoDeadline)
		switch ev.Kind {
		case codec.Format:
			// Een resolutiewissel midden in een stream geeft dit event
			// opnieuw; het meetinstrument begint dan gewoon met verse
			// buffers. Uitstaande buffers komen nog terug als Produced en
			// worden dan weer aangeboden — met de nieuwe maat.
			layout = ev.Layout
			fmt.Printf("codecdemo: %dx%d %s, %d bytes per frame, %d buffers wanted\n",
				layout.Width, layout.Height, layout.Pixel, layout.FrameSize, layout.MinBuffers)
			// De firmware noemt een MINIMUM; dat is wat hij nodig heeft om
			// niet vast te lopen, niet wat hij nodig heeft om niets te
			// missen. Een herordenende stream (B-piramide) houdt er meer
			// tegelijk vast, dus geef er zoveel als er passen.
			n := codecDemoBuffers
			if v := bootParam("hopos.codecdemo.bufs"); v != "" {
				if k, err := strconv.Atoi(v); err == nil && k > 0 {
					n = k
				}
			}
			if n < layout.MinBuffers+1 {
				n = layout.MinBuffers + 1
			}
			// Elke buffer op een eigen paginagrens: de VPU krijgt hem via
			// zijn page tables te zien, en die kennen niets fijners dan een
			// pagina. Een framegrootte is zelden een veelvoud daarvan
			// (1920x1080 NV12 is 3110400 bytes = 759,375 pagina's), dus de
			// stap moet omhoog afgerond worden.
			step := (layout.FrameSize + 4095) &^ 4095
			free = free[:0]
			for i := 0; i < n; i++ {
				pa := pool + uintptr(uint64(i)*step)
				if pa+uintptr(step) > poolEnd {
					break
				}
				free = append(free, codec.Buffer{PA: pa, Size: step})
			}
			if len(free) == 0 {
				fmt.Println("codecdemo: frames do not fit the demo arena")
				return
			}
		case codec.Consumed:
			busy--
			inFree = append(inFree, ev.Buf)
		case codec.Produced:
			if ev.Bytes == 0 {
				// Gedecodeerd, maar niet om te tonen. Buffer terug in de
				// pool en verder — dit telt niet als beeld.
				skipped++
				free = append(free, ev.Buf)
				continue
			}
			frames++
			if frames == 1 {
				// De eerste zestien luma-bytes, onbewerkt. Daarmee is de
				// bit-indeling van een 10-bit beeld te toetsen zonder een
				// heel frame te vergelijken: staat de waarde links in het
				// 16-bit woord (P010) of rechts (10 bit in de lage bits)?
				var head [16]byte
				dev.CopyOut(head[:], ev.Buf.PA)
				fmt.Printf("codecdemo: first luma bytes %x (%s, stride %d)\n",
					head, ev.Layout.Pixel, ev.Layout.Planes[0].Stride)
			}
			if doHash && (frames <= 64 || frames%50 == 0) {
				fmt.Printf("codecdemo: frame %d %dx%d hash %016x\n",
					frames, ev.Layout.Width, ev.Layout.Height, hashFrame(ev))
			}
			if toDisk {
				if err := writeFrame(fs, out, &put, ev); err != nil {
					fmt.Printf("codecdemo: write: %v\n", err)
					return
				}
			}
			free = append(free, ev.Buf)
		case codec.Done:
			done = true
		case codec.Fault:
			fmt.Printf("codecdemo: %v — after %d frame(s)\n", ev.Err, frames)
			if st, ok := slots.CodecEngine().(codec.Stateful); ok {
				fmt.Printf("codecdemo: hardware says %s\n", st.State())
			}
			return
		}
	}
	d := time.Since(start)
	fps := 0.0
	if d > 0 {
		fps = float64(frames) / d.Seconds()
	}
	fmt.Printf("codecdemo: %d frames from %d MB in %v (%.1f fps) HOPOS_CODEC_DEMO\n",
		frames, fed>>20, d.Round(time.Millisecond), fps)
	if skipped > 0 {
		fmt.Printf("codecdemo: %d frame(s) came back empty (decode-only or rejected)\n", skipped)
	}
	if st, ok := slots.CodecEngine().(codec.Stateful); ok {
		fmt.Printf("codecdemo: hardware says %s\n", st.State())
	}
}

// hashFrame vat één beeld samen: FNV-1a over de zichtbare bytes van beide
// vlakken. Dezelfde som is op de Mac uit ffmpeg te halen, dus dit is de
// goedkoopste manier om "pixelperfect" hard te maken — geen 22MB over het net
// per beeld, en het meet de decoder niet kapot.
func hashFrame(ev codec.Event) uint64 {
	const (
		fnvOffset = 14695981039346656037
		fnvPrime  = 1099511628211
	)
	l := ev.Layout
	bytesPerPixel := 1
	if l.Pixel == codec.P010 {
		bytesPerPixel = 2
	}
	sum := uint64(fnvOffset)
	row := make([]byte, l.Planes[0].Stride)
	eat := func(b []byte) {
		for _, c := range b {
			sum = (sum ^ uint64(c)) * fnvPrime
		}
	}
	n := l.Width * bytesPerPixel
	for y := 0; y < l.Height; y++ {
		dev.CopyOut(row, ev.Buf.PA+uintptr(y*l.Planes[0].Stride))
		eat(row[:n])
	}
	if l.Planes[1].Stride == 0 {
		return sum
	}
	for y := 0; y < l.Height/2; y++ {
		dev.CopyOut(row, ev.Buf.PA+uintptr(l.Planes[1].Off)+uintptr(y*l.Planes[1].Stride))
		eat(row[:n])
	}
	return sum
}

// writeFrame schrijft één gedecodeerd frame weg. De VPU schreef in fysiek
// geheugen dat wij niet als Go-slice hebben, dus het gaat per regel over een
// scratch — en alleen de zichtbare kolommen, zodat het bestand een gewone
// NV12-stroom is die elke speler leest.
func writeFrame(fs *hopfs.FS, path string, off *uint64, ev codec.Event) error {
	l := ev.Layout
	if l.FrameSize == 0 {
		return nil
	}
	// Een 10-bit beeld is twee bytes per sample: wie hier de breedte in
	// pixels neemt schrijft de halve regel weg en krijgt een bestand dat er
	// als ruis uitziet terwijl de decoder het goed deed.
	n := l.Width
	if l.Pixel == codec.P010 {
		n *= 2
	}
	row := make([]byte, l.Planes[0].Stride)
	for y := 0; y < l.Height; y++ {
		dev.CopyOut(row, ev.Buf.PA+uintptr(y*l.Planes[0].Stride))
		if err := fs.WriteAt(path, *off, row[:n]); err != nil {
			return err
		}
		*off += uint64(n)
	}
	if l.Planes[1].Stride == 0 {
		return nil
	}
	for y := 0; y < l.Height/2; y++ {
		dev.CopyOut(row, ev.Buf.PA+uintptr(l.Planes[1].Off)+uintptr(y*l.Planes[1].Stride))
		if err := fs.WriteAt(path, *off, row[:n]); err != nil {
			return err
		}
		*off += uint64(n)
	}
	return nil
}

// codecFromName raadt de codec uit de bestandsnaam.
func codecFromName(path string) codec.Codec {
	i := strings.LastIndex(path, ".")
	if i < 0 {
		return codec.Unknown
	}
	switch strings.ToLower(path[i+1:]) {
	case "h264", "264", "avc":
		return codec.H264
	case "hevc", "265", "h265":
		return codec.HEVC
	case "av1", "obu":
		return codec.AV1
	case "vp9":
		return codec.VP9
	case "vp8":
		return codec.VP8
	case "mpeg2", "m2v":
		return codec.MPEG2
	case "vc1":
		return codec.VC1
	case "jpg", "jpeg", "mjpeg":
		return codec.JPEG
	}
	return codec.Unknown
}
