//go:build media

package slots

import (
	"fmt"
	"sync"

	"github.com/xinix00/HopOS/metal/v2/driver/codec"
)

// De videocodec van de node. Hij hoort hier omdat hij hetzelfde soort ding is
// als een core of een partitie: schaars ijzer dat over de taken verdeeld moet
// worden. De hardware heeft een vast aantal sessies (de Linlon V8 van de O6N
// telt ze zelf in zijn NLSID-register), en wie er een opent houdt hem tot hij
// hem sluit.
//
// Deze laag is bewust dun: openen, tellen, en bij het opruimen van een slot
// alles teruggeven wat die taak nog vasthield. Het contract naar apps toe (de
// system-functies waarmee een taak een stream door de decoder haalt) zit
// erboven, in codecabi.go.
var (
	codecMu     sync.Mutex
	codecEngine codec.Engine
	codecOwner  map[codec.Session]int // sessie → slot, voor het opruimen
)

// UseCodec registreert het codec-blok van het board. Eenmalig bij het
// opstarten; een node zonder codec-ijzer roept dit nooit aan.
func UseCodec(e codec.Engine) {
	codecMu.Lock()
	defer codecMu.Unlock()
	codecEngine = e
	codecOwner = map[codec.Session]int{}
	fmt.Printf("codec: %s HOPOS_CODEC_UP\n", e.Describe())
}

// CodecEngine geeft het geregistreerde codec-blok (nil als er geen is). Voor
// diagnose; het normale pad gaat via OpenCodec.
func CodecEngine() codec.Engine {
	codecMu.Lock()
	defer codecMu.Unlock()
	return codecEngine
}

// HasCodec meldt of deze node video kan decoderen in hardware.
func HasCodec() bool {
	codecMu.Lock()
	defer codecMu.Unlock()
	return codecEngine != nil
}

// OpenCodec opent een sessie namens een slot. Slot 0 is HOP zelf (diagnose en
// het meetinstrument); alles daarboven is een taak.
func OpenCodec(slot int, cfg codec.Config) (codec.Session, error) {
	codecMu.Lock()
	e := codecEngine
	codecMu.Unlock()
	if e == nil {
		return nil, codec.ErrUnsupported
	}
	s, err := e.Open(cfg)
	if err != nil {
		return nil, err
	}
	codecMu.Lock()
	codecOwner[s] = slot
	codecMu.Unlock()
	return s, nil
}

// CloseCodec sluit een sessie en haalt hem uit de boekhouding.
func CloseCodec(s codec.Session) {
	if s == nil {
		return
	}
	codecMu.Lock()
	delete(codecOwner, s)
	codecMu.Unlock()
	_ = s.Close()
}

// ReleaseCodecs sluit alles wat een slot nog openhad. Aanroepen bij het
// opruimen van een taak: een app die omvalt met een open decoder zou anders
// een van de weinige hardware-sessies vasthouden tot de volgende kern-flip —
// precies de zombie-vorm die we bij stulp-bundels al eens gezien hebben.
func ReleaseCodecs(slot int) {
	codecMu.Lock()
	var mine []codec.Session
	for s, owner := range codecOwner {
		if owner == slot {
			mine = append(mine, s)
		}
	}
	for _, s := range mine {
		delete(codecOwner, s)
	}
	codecMu.Unlock()
	for _, s := range mine {
		_ = s.Close()
	}
}
