package applib

import (
	"fmt"
	"time"
	"unsafe"

	"github.com/xinix00/HopOS/metal/v2/abi/hopabi"
)

// De codec-kant van het app-contract: een taak haalt een stream door het
// codec-blok van de node.
//
// Het bijzondere aan deze calls is wat er NIET overheen gaat. Een 4K-beeld in
// P010 is 24MB; bij 24 beelden per seconde is dat 597MB/s en het interne LAN
// piekt op 550. De beelden blijven dus waar ze al staan — in het geheugen van
// deze app — en over de draad gaat alleen waar ze staan. HOP controleert dat
// zo'n plek binnen de eigen partitie valt en hangt hem in de page tables van
// het ijzer; buiten die partitie kan de codec per constructie niets zien.
//
// Praktisch betekent dat: buffers komen van AllocBuf, niet van make(). Die
// levert een stuk dat op een paginagrens ligt (fijner kent de MMU van de codec
// niet) plus de beschrijving die de calls nodig hebben.

// Codec-nummers, gelijk aan driver/codec. Ze staan hier opnieuw omdat een app
// niets van de driverboom importeert — dat is precies de scheiding die dit
// contract bewaakt.
const (
	CodecH264  = 1
	CodecHEVC  = 2
	CodecAV1   = 3
	CodecVP8   = 4
	CodecVP9   = 5
	CodecMPEG2 = 6
	CodecMPEG4 = 7
	CodecVC1   = 8
	CodecJPEG  = 9
)

// Richtingen.
const (
	Decode = 0 // bitstream in, beelden uit
	Encode = 1 // beelden in, bitstream uit
)

// Pixelformaten.
const (
	PixNV12 = 1 // Y + verweven CbCr, 8 bit
	PixNV21 = 2
	PixI420 = 3
	PixP010 = 4 // Y + verweven CbCr, 10 bit in 16-bit woorden
	PixY8   = 5
)

// Vlaggen op een ingevoerde buffer.
const (
	FlagEOS      = 1 << 0 // einde van de stream
	FlagHeaders  = 1 << 1 // alleen codec-configuratie, geen beeld
	FlagKeyFrame = 1 << 2 // bij Encode: maak hier een keyframe van
)

// Event-soorten, zie hopabi. Een Format-event draagt geen buffer: daar is
// Size de maat die elke uitvoerbuffer minstens moet hebben en Bytes het
// aantal buffers dat het ijzer tegelijk wil vasthouden. Width, Height, Pixel,
// Stride en Plane beschrijven het beeld.
const (
	CodecFormat   = hopabi.EventFormat
	CodecConsumed = hopabi.EventConsumed
	CodecProduced = hopabi.EventProduced
	CodecDone     = hopabi.EventDone
	CodecFault    = hopabi.EventFault
)

// codecPageSize is de paginamaat waarop het ijzer werkt.
const codecPageSize = 4096

// CodecConfig opent een sessie. Bij Decode zijn Width en Height hooguit een
// hint: de stream bepaalt de maat en die komt terug als Format-event.
type CodecConfig struct {
	Codec  uint8
	Dir    uint8
	Pixel  uint8
	Width  int
	Height int
}

// Buf is een stuk van de eigen partitie dat het ijzer mag lezen of vullen.
// Off is de afstand vanaf het begin van het eigen geheugen; Mem is hetzelfde
// stuk zoals de app het ziet.
type Buf struct {
	Off  uint64
	Size uint64
	Mem  []byte
}

// AllocBuf reserveert n bytes op een paginagrens en beschrijft ze zo dat ze
// aan het codec-blok aangeboden kunnen worden. Het geheugen blijft van de app;
// zolang de teruggegeven Buf leeft, leeft het.
//
// Waarom niet gewoon make(): de MMU van de codec kent niets fijner dan een
// pagina, dus een buffer die halverwege begint zou de buren meenemen. HOP
// weigert zo'n aanbod (terecht), en dan is het prettiger dat de fout hier
// ontstaat dan drie lagen verderop.
func (a *App) AllocBuf(n uint64) (Buf, error) {
	if n == 0 {
		return Buf{}, fmt.Errorf("codec buffer of zero bytes")
	}
	n = (n + codecPageSize - 1) &^ (codecPageSize - 1)
	raw := make([]byte, n+codecPageSize)
	start := uintptr(unsafe.Pointer(&raw[0]))
	pad := uintptr(0)
	if m := start & (codecPageSize - 1); m != 0 {
		pad = codecPageSize - m
	}
	mem := raw[pad : pad+uintptr(n)]
	off := uint64(start+pad) - a.RAMStart
	if uint64(start+pad) < a.RAMStart || off+n > a.RAMSize {
		return Buf{}, fmt.Errorf("codec buffer %#x+%d falls outside own memory", off, n)
	}
	return Buf{Off: off, Size: n, Mem: mem}, nil
}

// Codec is een geopende sessie. Niet veilig voor gelijktijdig gebruik vanuit
// meerdere goroutines: de calls gaan over één verbinding en de volgorde
// waarin buffers aangeboden worden doet ertoe.
type Codec struct {
	a      *App
	handle uint32
	closed bool
	// pending: events die HOP al opleverde maar die niet meer in de dst van
	// die Poll pasten. De volgende Poll geeft ze eerst.
	pending []hopabi.Event
}

// OpenCodec opent een codec-sessie op het ijzer van de node.
func (a *App) OpenCodec(cfg CodecConfig) (*Codec, error) {
	if cfg.Width < 0 || cfg.Width > 0xFFFF || cfg.Height < 0 || cfg.Height > 0xFFFF {
		return nil, fmt.Errorf("codec: %dx%d is outside the range this contract carries", cfg.Width, cfg.Height)
	}
	resp, err := a.rpcNoRetry(hopabi.Req{
		Op: hopabi.OpCodecOpen,
		Data: hopabi.EncodeOpen(hopabi.OpenArgs{
			Codec:  cfg.Codec,
			Dir:    cfg.Dir,
			Pixel:  cfg.Pixel,
			Width:  uint16(cfg.Width),
			Height: uint16(cfg.Height),
		}),
	})
	if err != nil {
		return nil, err
	}
	return &Codec{a: a, handle: uint32(resp.Size)}, nil
}

// Feed voert bitstream in (bij Decode) of een beeld (bij Encode). De buffer
// blijft bij het ijzer tot hij als Consumed-event terugkomt; tag komt
// ongewijzigd terug op het resultaat dat eruit volgt.
func (c *Codec) Feed(b Buf, filled uint64, flags uint32, tag uint64) error {
	if c.closed {
		return fmt.Errorf("codec: session closed")
	}
	_, err := c.a.rpcNoRetry(hopabi.Req{
		Op:  hopabi.OpCodecFeed,
		Off: b.Off,
		N:   b.Size,
		Data: hopabi.EncodeFeed(hopabi.FeedArgs{
			Handle: c.handle, Flags: flags, Filled: filled, Tag: tag,
		}),
	})
	return err
}

// Offer biedt een lege buffer aan voor het resultaat.
func (c *Codec) Offer(b Buf) error {
	if c.closed {
		return fmt.Errorf("codec: session closed")
	}
	_, err := c.a.rpcNoRetry(hopabi.Req{
		Op:   hopabi.OpCodecOffer,
		Off:  b.Off,
		N:    b.Size,
		Data: hopabi.EncodeBuf(hopabi.BufArgs{Handle: c.handle}),
	})
	return err
}

// Poll haalt op wat er klaarstaat, tot len(dst) gebeurtenissen tegelijk. Nul
// betekent: niets te melden, probeer het zo nog eens.
//
// Alles in één call, niet één per keer: bij 4K zou een round-trip per beeld
// precies de kosten terugbrengen die deze naad moest vermijden.
//
// Levert HOP er meer dan er in dst passen, dan houdt de sessie de rest vast en
// krijgt de volgende Poll die eerst. Weggooien kan niet: ze zijn aan de
// overkant al uit de sessie gehaald, en een Consumed of Produced die niet
// aankomt is een buffer die de app nooit terugziet.
func (c *Codec) Poll(dst []hopabi.Event) (int, error) {
	if c.closed {
		return 0, fmt.Errorf("codec: session closed")
	}
	if len(dst) == 0 {
		return 0, fmt.Errorf("codec: poll needs room for at least one event")
	}
	if len(c.pending) > 0 {
		n := copy(dst, c.pending)
		c.pending = append(c.pending[:0], c.pending[n:]...)
		return n, nil
	}
	resp, err := c.a.rpcNoRetry(hopabi.Req{
		Op:   hopabi.OpCodecPoll,
		Data: hopabi.EncodeBuf(hopabi.BufArgs{Handle: c.handle}),
	})
	if err != nil {
		return 0, err
	}
	if resp.Size > uint64(len(resp.Data)/hopabi.EventLen) {
		return 0, fmt.Errorf("codec: poll announced %d events but carried %d bytes", resp.Size, len(resp.Data))
	}
	n := int(resp.Size)
	for i := 0; i < n; i++ {
		// Kan niet mislukken: de lengte is hierboven al getoetst.
		ev, _ := hopabi.DecodeEvent(resp.Data[i*hopabi.EventLen:])
		if i < len(dst) {
			dst[i] = ev
		} else {
			c.pending = append(c.pending, ev)
		}
	}
	return min(n, len(dst)), nil
}

// Close geeft de hardware-sessie terug. Er zijn er maar een handvol op de hele
// node, dus dit hoort in een defer.
func (c *Codec) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	_, err := c.a.rpcNoRetry(hopabi.Req{
		Op:   hopabi.OpCodecClose,
		Data: hopabi.EncodeBuf(hopabi.BufArgs{Handle: c.handle}),
	})
	return err
}

// rpcNoRetry doet één system call en herhaalt hem NIET als het transport
// wegviel.
//
// De opslag-ops mogen dat wel — een write is dezelfde bytes op dezelfde plek —
// maar deze niet: twee keer dezelfde Feed is twee happen bitstream, en twee
// keer hetzelfde Offer geeft dezelfde buffer twee keer aan het ijzer. Een
// kern-flip midden in een decode kost dus de sessie, en dat is de juiste
// prijs: het ijzer is dan sowieso opnieuw opgestart.
func (a *App) rpcNoRetry(req hopabi.Req) (hopabi.Resp, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rpcNoRetryLocked(req, codecTimeout)
}

// rpcNoRetryLocked is rpcNoRetry voor wie a.mu al vasthoudt en zijn eigen
// timeout kiest: een apparaatopdracht (DeviceCommand) is om dezelfde reden
// niet te herhalen.
func (a *App) rpcNoRetryLocked(req hopabi.Req, timeout time.Duration) (hopabi.Resp, error) {
	a.seq++
	req.Seq = a.seq
	if !a.sysReady {
		return hopabi.Resp{}, fmt.Errorf("system call: network is not ready; call appnet.Up first")
	}
	resp, err, _ := a.systemRPCOnce(req, timeout)
	return resp, err
}

// codecTimeout is ruimer dan de gewone call-timeout: een Open laadt firmware
// in het blok en dat kost meer dan een stat.
const codecTimeout = 10 * time.Second
