// Package vcmail spreekt de VideoCore-firmware-mailbox (property-interface,
// kanaal 8) — het universele Pi-kanaal voor alles wat de firmware beheert:
// temperatuur, klokken, de echte board-MAC. Zelfde blok op de Pi 4
// (0xFE00B880) en de Pi 5 (0x10_7C013880, DT mailbox@7c013880); alleen de
// basis verschilt (boardconstante).
//
// Protocol (brcm,bcm2835-mbox + de property-tags uit de firmware-wiki):
// schrijf het fysieke adres van een 16-byte-gealigneerde tag-buffer | 8 in
// MBOX1 (+0x20, status +0x38), poll MBOX0 (+0x00, status +0x18) tot het
// antwoord op kanaal 8 terugkomt; de firmware schrijft de respons in
// dezelfde buffer. Linux geeft op de BCM2711/2712 het rauwe fysieke adres
// door (geen 0xC0-alias: de mbox-node heeft geen dma-ranges); voor het
// geval een firmware anders beslist probeert Call de alias één keer als
// terugval. De buffer moet in laag DRAM liggen (VC-bereik) en ongecachet
// zijn — raspi.VCMailBuf (0x7E000) voldoet aan alle drie.
//
// Alleen voor GOOS=tamago GOARCH=arm64; alleen de HOP-core praat met de
// mailbox (apps hebben er geen mapping voor).
package vcmail

import (
	"time"

	"hop-os/metal/dev"
)

// Mailbox-registers (relatief aan Base) en statusbits.
const (
	mbox0Read   = 0x00 // VC → ARM
	mbox0Status = 0x18 // bit30 = leeg
	mbox1Write  = 0x20 // ARM → VC
	mbox1Status = 0x38 // bit31 = vol

	chProps     = 8
	statusFull  = 1 << 31
	statusEmpty = 1 << 30

	respSuccess = 0x80000000
)

// Property-tags (firmware-wiki "Mailbox property interface").
const (
	tagGetMAC       = 0x00010003
	tagGetClockRate = 0x00030002
	tagGetMaxClock  = 0x00030004
	tagGetTemp      = 0x00030006
	tagGetMinClock  = 0x00030007
	tagSetClockRate = 0x00038002

	// ClockARM is het klok-ID van de ARM-cores (het enige dat de
	// klokwachter aanraakt).
	ClockARM = 3
)

// Mbox is één mailbox-instantie. Buf: fysiek, 16-byte-gealigneerd, laag
// DRAM, buiten elke RAM-declaratie (ongecachet) — raspi.VCMailBuf.
type Mbox struct {
	Base uintptr
	Buf  uintptr

	aliasOr uint32 // 0xC0000000 als de firmware de oude bus-alias blijkt te eisen
	probed  bool
}

// Tag is één property-tag in een (mogelijk multi-tag) transactie: words is
// het payload-venster — verzoek erin, respons erover heen (in-place, zoals
// het protocol werkt).
type Tag struct {
	ID    uint32
	Words []uint32
}

// Call voert één property-tag uit. false = timeout of foutcode — meetdata,
// geen panic.
func (m *Mbox) Call(tag uint32, words []uint32) bool {
	return m.CallN([]Tag{{ID: tag, Words: words}})
}

// CallN voert meerdere tags in één transactie uit — verplicht voor de
// framebuffer-setup (de firmware finaliseert de configuratie per bericht,
// dus set-size/depth/allocate moeten samen reizen).
func (m *Mbox) CallN(tags []Tag) bool {
	if !m.probed {
		// Eerste call: rauw fysiek adres; faalt dat, één keer de oude
		// 0xC0000000-alias proberen en de uitkomst onthouden.
		m.probed = true
		if m.do(tags) {
			return true
		}
		m.aliasOr = 0xC0000000
		if m.do(tags) {
			return true
		}
		m.aliasOr = 0
		return false
	}
	return m.do(tags)
}

func (m *Mbox) do(tags []Tag) bool {
	// Eerst de inbox leegvegen: een eerder getimeout antwoord dat blijft
	// liggen zet anders álle volgende calls één respons achter (GEMETEN
	// 2026-07-11: na een trage SetClockRate tijdens HDMI-werk las elke call
	// het antwoord van zijn voorganger — 0.0°C, ARM 0 MHz).
	for dev.Read32(m.Base+mbox0Status)&statusEmpty == 0 {
		_ = dev.Read32(m.Base + mbox0Read)
	}

	// Buffer: size, code, {tag-id, payload-size, req-code, payload...}*, end.
	p := m.Buf + 8
	for _, t := range tags {
		dev.Write32(p+0, t.ID)
		dev.Write32(p+4, uint32(len(t.Words))*4)
		dev.Write32(p+8, 0)
		for i, w := range t.Words {
			dev.Write32(p+12+uintptr(i)*4, w)
		}
		p += 12 + uintptr(len(t.Words))*4
	}
	dev.Write32(p, 0) // end-tag
	dev.Write32(m.Buf+0, uint32(p+4-m.Buf))
	dev.Write32(m.Buf+4, 0)
	dev.MB()

	if !m.wait(mbox1Status, statusFull) {
		return false
	}
	dev.Write32(m.Base+mbox1Write, uint32(m.Buf)|m.aliasOr|chProps)

	// Antwoord op óns kanaal afwachten (andere kanalen komen hier niet voor,
	// maar overslaan is goedkoop en veilig).
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		if !m.wait(mbox0Status, statusEmpty) {
			return false
		}
		if dev.Read32(m.Base+mbox0Read)&0xF == chProps {
			break
		}
		if time.Now().After(deadline) {
			return false
		}
	}
	if dev.Read32(m.Buf+4) != respSuccess {
		return false
	}
	// Responsen terugkopiëren; elke tag moet zijn resp-bit hebben.
	p = m.Buf + 8
	for _, t := range tags {
		if dev.Read32(p+8)&respSuccess == 0 {
			return false
		}
		for i := range t.Words {
			t.Words[i] = dev.Read32(p + 12 + uintptr(i)*4)
		}
		p += 12 + uintptr(len(t.Words))*4
	}
	return true
}

// wait polt tot statusbit `bit` in register `reg` zakt (vol/leeg), begrensd.
func (m *Mbox) wait(reg uintptr, bit uint32) bool {
	deadline := time.Now().Add(500 * time.Millisecond)
	for dev.Read32(m.Base+reg)&bit != 0 {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Microsecond)
	}
	return true
}

// Temp geeft de SoC-temperatuur in milligraden Celsius.
func (m *Mbox) Temp() (mC int, ok bool) {
	w := []uint32{0, 0}
	if !m.Call(tagGetTemp, w) {
		return 0, false
	}
	return int(w[1]), true
}

// ClockRate geeft de actuele kloksnelheid van id (Hz).
func (m *Mbox) ClockRate(id uint32) (hz uint32, ok bool) {
	w := []uint32{id, 0}
	if !m.Call(tagGetClockRate, w) {
		return 0, false
	}
	return w[1], true
}

// MaxClockRate geeft het firmware-maximum van id (Hz) — de "vol"-stand.
func (m *Mbox) MaxClockRate(id uint32) (hz uint32, ok bool) {
	w := []uint32{id, 0}
	if !m.Call(tagGetMaxClock, w) {
		return 0, false
	}
	return w[1], true
}

// MinClockRate geeft het firmware-minimum van id (Hz) — lager klemt de
// firmware toch (GEMETEN 2026-07-11 op de Pi 5: SetClockRate(600M) werd
// stilzwijgend 1500M, de arm_freq_min-vloer).
func (m *Mbox) MinClockRate(id uint32) (hz uint32, ok bool) {
	w := []uint32{id, 0}
	if !m.Call(tagGetMinClock, w) {
		return 0, false
	}
	return w[1], true
}

// SetClockRate zet id op hz (derde woord: skip-setting-turbo = 0) en geeft
// de door de firmware gekozen werkelijke waarde terug.
func (m *Mbox) SetClockRate(id, hz uint32) (actual uint32, ok bool) {
	w := []uint32{id, hz, 0}
	if !m.Call(tagSetClockRate, w) {
		return 0, false
	}
	return w[1], true
}

// Framebuffer-tags (zelfde property-interface, het officiële pad voor een
// raw kernel — GEMETEN 2026-07-11: met HDMI erin activeert de Pi 5-firmware
// het scherm wél, maar laat hij geen simplefb-node in de DTB achter).
const (
	tagFBAlloc    = 0x00040001 // req: alignment → resp: busadres, grootte
	tagFBPhysSize = 0x00048003
	tagFBVirtSize = 0x00048004
	tagFBDepth    = 0x00048005
	tagFBPitch    = 0x00040008
)

// FB beschrijft het door de firmware toegekende framebuffer — de RESPONS
// telt, niet ons verzoek: de firmware mag afwijken (GEMETEN 2026-07-11 op
// de Pi 5: 32bpp gevraagd, 16bpp gekregen — de streepjes-salade op Dereks
// scherm was onze 32-bit render op een 16-bit scanout).
type FB struct {
	Base          uintptr
	Size, Pitch   uint32
	Width, Height uint32
	Depth         uint32 // bpp zoals de firmware hem écht zette
}

// AllocFB vraagt de firmware om een w×h×32bpp-framebuffer (één transactie:
// sizes + depth + allocate + pitch — de firmware finaliseert per bericht)
// en geeft terug wat er wérkelijk kwam. Het busadres kan 0xC0000000-
// gealiast terugkomen; dat masker eraf is het ARM-fysieke adres.
func (m *Mbox) AllocFB(w, h uint32) (FB, bool) {
	phys := []uint32{w, h}
	virt := []uint32{w, h}
	depth := []uint32{32}
	alloc := []uint32{4096, 0}
	pit := []uint32{0}
	if !m.CallN([]Tag{
		{ID: tagFBPhysSize, Words: phys},
		{ID: tagFBVirtSize, Words: virt},
		{ID: tagFBDepth, Words: depth},
		{ID: tagFBAlloc, Words: alloc},
		{ID: tagFBPitch, Words: pit},
	}) {
		return FB{}, false
	}
	if alloc[0] == 0 || pit[0] == 0 || depth[0] == 0 {
		return FB{}, false
	}
	return FB{
		Base:  uintptr(alloc[0] &^ 0xC0000000),
		Size:  alloc[1],
		Pitch: pit[0],
		Width: phys[0], Height: phys[1],
		Depth: depth[0],
	}, true
}

// MAC geeft het door de fabriek toegewezen board-MAC-adres.
func (m *Mbox) MAC() (mac [6]byte, ok bool) {
	w := []uint32{0, 0}
	if !m.Call(tagGetMAC, w) {
		return mac, false
	}
	mac[0], mac[1], mac[2], mac[3] = byte(w[0]), byte(w[0]>>8), byte(w[0]>>16), byte(w[0]>>24)
	mac[4], mac[5] = byte(w[1]), byte(w[1]>>8)
	return mac, true
}
