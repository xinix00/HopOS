// Package mve bestuurt de videocodec van de Orion O6N: een Arm China Linlon V8,
// de doorontwikkelde Mali Video Engine. Acht codecs voor decode (waaronder alle
// vier die op een Blu-ray kunnen staan: MPEG-2, VC-1, H.264 en HEVC) plus AV1
// en VP9, tot 8K60.
//
// Het blok is firmware-gedreven en dat bepaalt de vorm van deze driver. Per
// sessie laden we een codec-firmware (hevcdec.fwb, ~300KB) in een eigen
// adresruimte, en daarna is het verkeer berichten en bufferbeschrijvingen in
// gedeeld geheugen. Wij dragen dus bytes en pixels, geen bitstream-kennis:
// geen NAL-units, geen slice-headers, geen referentielijsten. Dat is precies de
// verdeling die we willen — de hardware levert pixelperfect beeld, en HopOS
// hoeft er geen codec-implementatie naast te zetten.
//
// Twee dingen die je moet weten voor je hieraan sleutelt:
//
//   - De VPU heeft een EIGEN MMU. Die is onze isolatie: een sessie ziet alleen
//     wat wij in zijn tabel hangen. Daarom woont deze driver in HOP en krijgt
//     een app nooit de registers.
//   - De firmware van de O6N draagt "-sum" in zijn versie en eist dus een
//     lopende checksum achter elke berichtkop (zie queue.go). Zonder dat woord
//     komt er geen enkele sessie van de grond.
package mve

import (
	"errors"
	"fmt"
	"sync"

	"github.com/xinix00/HopOS/metal/v2/dev"
	"github.com/xinix00/HopOS/metal/v2/driver/codec"
)

// hwIDLinlonV8 is wat het HARDWARE_ID-register van de O6N hoort te geven. De
// 0x5664-familie deelt registerlayout en protocol met de v52/v76-generatie;
// oudere blokken (v500/v550/v61) hebben een andere formaattabel en staan hier
// bewust niet in: ongetest ijzer weigeren we liever dan half te bedienen.
const hwIDLinlonV8 = 0x5664

// Device is één VPU.
type Device struct {
	r     regs
	arena *Arena
	fw    codec.FirmwareSource

	hwID   uint32
	rev    uint32
	fuse   uint32
	ncores int
	nlsid  int

	mu    sync.Mutex
	lsids []*session // per hardware-sessie: wie hem heeft
}

// Probe neemt een VPU in gebruik: registers controleren, blok resetten,
// scheduler aanzetten. vpu en rcsu zijn de twee MMIO-vensters uit de firmware
// (op de O6N 0x14240000 en 0x14230000), arena is het fysieke geheugen dat de
// VPU mag zien.
//
// Let op de volgorde op ijzer: hiervóór moet het power-domein aan staan. Op de
// O6N gaat dat via een SCMI-call naar de TF-A, die niet alleen de stroom maar
// ook de interconnect-permissies zet — zonder die call geeft de eerste
// registerlees een SError die geen enkele handler nog opvangt.
func Probe(vpu, rcsu uintptr, arena *Arena, fw codec.FirmwareSource) (*Device, error) {
	d := &Device{r: regs{base: vpu, rcsu: rcsu}, arena: arena, fw: fw}

	// Het HARDWARE_ID-register draagt het modelnummer in zijn bovenste helft
	// en een variant in de onderste: op de O6N leest het 0x56648002, en
	// 0x5664 is het nummer dat ook in de firmware-blobs staat ("56648002").
	// Op de hele waarde vergelijken werkt dus niet.
	d.hwID = d.r.read(regHardwareID)
	if d.hwID>>16 != hwIDLinlonV8 {
		return nil, fmt.Errorf("mve: hardware id %#x (model %#x), expected model %#x",
			d.hwID, d.hwID>>16, hwIDLinlonV8)
	}
	d.rev = d.r.read(regSVNRev)
	d.fuse = d.r.read(regFuse)
	d.ncores = int(d.r.read(regNCores))
	d.nlsid = int(d.r.read(regNLSID))
	if d.ncores < 1 || d.nlsid < 1 || d.nlsid > 16 {
		return nil, fmt.Errorf("mve: implausible geometry: %d cores, %d sessions", d.ncores, d.nlsid)
	}
	d.lsids = make([]*session, d.nlsid)

	// Software-reset, daarna de scheduler aan met een lege job-queue. Een VPU
	// die bij boot nog een job van vóór een kernel-flip vasthoudt zou anders
	// meteen in het geheugen van de vorige wereld gaan graven.
	d.r.write(regReset, 1)
	d.r.write(regClkForce, 0)
	d.r.write(regJobQueue, emptyJobQueue)
	dev.MB()
	d.r.write(regEnable, 1)
	return d, nil
}

// Describe geeft de bootregel.
func (d *Device) Describe() string {
	return fmt.Sprintf("Linlon V8 (id %#x rev %#x): %d cores, %d sessions, fuse %#x",
		d.hwID, d.rev, d.ncores, d.nlsid, d.fuse)
}

// Cores en Sessions geven de gemeten geometrie.
func (d *Device) Cores() int    { return d.ncores }
func (d *Device) Sessions() int { return d.nlsid }

// fwName geeft de firmware-naam voor een codec en richting: "hevcdec",
// "h264enc". Leeg betekent: dit ijzer doet het niet.
func fwName(c codec.Codec, dir codec.Direction) string {
	dec := map[codec.Codec]string{
		codec.H264: "h264", codec.HEVC: "hevc", codec.AV1: "av1",
		codec.VP8: "vp8", codec.VP9: "vp9", codec.MPEG2: "mpeg2",
		codec.MPEG4: "mpeg4", codec.VC1: "vc1", codec.JPEG: "jpeg",
		codec.AVS: "avs", codec.AVS2: "avs2",
	}
	enc := map[codec.Codec]string{
		codec.H264: "h264", codec.HEVC: "hevc",
		codec.VP8: "vp8", codec.VP9: "vp9", codec.JPEG: "jpeg",
	}
	table := dec
	suffix := "dec"
	if dir == codec.Encode {
		table, suffix = enc, "enc"
	}
	if n, ok := table[c]; ok {
		return n + suffix
	}
	return ""
}

// Supports meldt of deze VPU de combinatie aankan. De fuse-bits tellen mee: een
// exemplaar met HEVC uitgezet bestaat, en dat willen we weten vóór we een
// firmware inladen die nergens heen kan.
func (d *Device) Supports(c codec.Codec, dir codec.Direction) bool {
	if fwName(c, dir) == "" {
		return false
	}
	switch {
	case c == codec.HEVC && d.fuse&fuseNoHEVC != 0:
		return false
	case (c == codec.VP8 || c == codec.VP9) && d.fuse&fuseNoVPX != 0:
		return false
	}
	return true
}

// Open start een sessie: firmware laden, adresruimte bouwen, een hardware-slot
// pakken en de zaak inplannen.
func (d *Device) Open(cfg codec.Config) (codec.Session, error) {
	name := fwName(cfg.Codec, cfg.Dir)
	if name == "" || !d.Supports(cfg.Codec, cfg.Dir) {
		return nil, codec.ErrUnsupported
	}
	px, ok := fwPixel(cfg.Pixel)
	if !ok {
		return nil, fmt.Errorf("mve: pixel format %s not supported", cfg.Pixel)
	}
	if d.fw == nil {
		return nil, errNoFirmware
	}
	bin, err := d.fw.Load(name)
	if err != nil {
		return nil, fmt.Errorf("mve: firmware %s: %w", name, err)
	}
	h, err := parseFW(bin)
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	lsid := -1
	for i, s := range d.lsids {
		if s == nil {
			lsid = i
			break
		}
	}
	if lsid < 0 {
		d.mu.Unlock()
		return nil, codec.ErrBusy
	}
	s := &session{d: d, cfg: cfg, lsid: lsid, pixel: px, bufs: map[uint64]*held{}}
	// De sessie staat vanaf hier in d.lsids en is dus zichtbaar voor State;
	// onder zijn eigen lock opstarten, zodat die geen halve ringen leest.
	s.mu.Lock()
	d.lsids[lsid] = s
	d.mu.Unlock()

	err = s.start(bin, h)
	if err != nil {
		s.release()
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return s, nil
}

// State beschrijft wat het ijzer op dit moment doet. Voor diagnose: als er
// niets terugkomt uit een sessie is de vraag of de hardware de job überhaupt
// heeft opgepakt, en dat staat in deze vier registers.
func (d *Device) State() string {
	s := fmt.Sprintf("enable=%d jobqueue=%#x corelsid=%#x irqve=%#x",
		d.r.read(regEnable), d.r.read(regJobQueue),
		d.r.read(regCoreLSID), d.r.read(regIRQVE))
	// Eerst de sessies verzamelen en dan pas elk onder zijn eigen lock lezen:
	// een sessie neemt d.mu terwijl hij s.mu houdt (schedule), dus andersom
	// nesten is een deadlock.
	d.mu.Lock()
	lsids := append([]*session(nil), d.lsids...)
	d.mu.Unlock()
	for i, ses := range lsids {
		if ses == nil {
			continue
		}
		ses.mu.Lock()
		if ses.lsid == i { // anders intussen gesloten: zijn ringen zijn weg
			s += fmt.Sprintf(" | lsid%d alloc=%d sched=%d irqhost=%d lirqve=%d mmu=%#x%s",
				i, d.r.readLSID(i, lsAlloc), d.r.readLSID(i, lsSched),
				d.r.readLSID(i, lsIRQHost), d.r.readLSID(i, lsLIRQVE),
				d.r.readLSID(i, lsMMUCtrl), ses.ringState())
		}
		ses.mu.Unlock()
	}
	return s
}

// release geeft een hardware-slot terug.
func (d *Device) release(lsid int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if lsid >= 0 && lsid < len(d.lsids) {
		d.lsids[lsid] = nil
	}
}

// fwPixel vertaalt het HopOS-pixelformaat naar de bitveldcode van de firmware.
func fwPixel(p codec.Pixel) (uint16, bool) {
	switch p {
	case codec.NV12:
		return fmtNV12, true
	case codec.NV21:
		return fmtNV21, true
	case codec.I420:
		return fmtI420, true
	case codec.P010:
		return fmtP010, true
	case codec.Y8:
		return fmtY8, true
	}
	return 0, false
}

var _ codec.Engine = (*Device)(nil)

// errNoFirmware maakt de fout van een ontbrekende firmware-bron herkenbaar
// voor HOP: dat is een installatieprobleem, geen hardwareprobleem.
var errNoFirmware = errors.New("mve: no firmware source configured")
