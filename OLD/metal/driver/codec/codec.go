// Package codec is het videocodec-contract van HopOS: het ijzer dat een
// bitstream in pixels omzet (of terug), losgekoppeld van welk blok dat doet.
// De O6N heeft een Arm China Linlon V8 (media/driver/vpu/mve), de Radxa een Hantro,
// Apple een AVD — drie totaal verschillende registerbeesten met hetzelfde
// gedrag: je voert er buffers in, je krijgt er buffers uit, en het ijzer
// vertelt onderweg wat het van de stream begrepen heeft.
//
// Waarom een eigen pakketje en geen methodes op het board: zo hangt de
// codec-dienst van HOP (kern) aan geen enkele driver, precies zoals netdev de
// netstack van de NIC's losknipt. Een board draagt dit contract optioneel
// (board.Codecs) en een node zonder codec-ijzer mist gewoon die dienst.
//
// Het model is de "stateful" vorm: de firmware/het ijzer parseert de bitstream
// zelf. Wij dragen bytes, geen NAL-units, geen slice-headers, geen
// referentielijsten. Een board waarvan het ijzer dat NIET kan (een stateless
// decoder zoals de Pi) brengt zijn eigen parser mee in zijn eigen driver — dat
// is precies de soort kennis die achter deze naad hoort te blijven.
package codec

import "errors"

// Codec is het bitstream-formaat. De nummering is die van HopOS zelf; elke
// driver vertaalt naar wat zijn ijzer spreekt.
type Codec uint8

const (
	Unknown Codec = iota
	H264
	HEVC
	AV1
	VP8
	VP9
	MPEG2
	MPEG4
	VC1
	JPEG
	AVS
	AVS2
	H263
	RV
)

// String geeft de korte naam zoals hij in logs en in de config staat.
func (c Codec) String() string {
	switch c {
	case H264:
		return "h264"
	case HEVC:
		return "hevc"
	case AV1:
		return "av1"
	case VP8:
		return "vp8"
	case VP9:
		return "vp9"
	case MPEG2:
		return "mpeg2"
	case MPEG4:
		return "mpeg4"
	case VC1:
		return "vc1"
	case JPEG:
		return "jpeg"
	case AVS:
		return "avs"
	case AVS2:
		return "avs2"
	case H263:
		return "h263"
	case RV:
		return "rv"
	}
	return "unknown"
}

// Parse geeft de codec bij een naam (zoals hij in een jobspec of een call
// staat). Onbekend → Unknown, en dat is een weigering bij Open, geen paniek.
func Parse(name string) Codec {
	for c := H264; c <= RV; c++ {
		if c.String() == name {
			return c
		}
	}
	return Unknown
}

// Pixel is een ongecomprimeerd frame-formaat. Alleen wat een transcode-keten
// werkelijk nodig heeft: 8-bit en 10-bit 4:2:0, twee- en drievlaks. Blu-ray is
// NV12 (AVC/VC-1/MPEG-2) of P010 (UHD, 10-bit HDR).
type Pixel uint8

const (
	NoPixel Pixel = iota
	NV12          // Y + verweven CbCr, 8 bit
	NV21          // Y + verweven CrCb, 8 bit
	I420          // Y + Cb + Cr, 8 bit
	P010          // Y + verweven CbCr, 10 bit in 16-bit woorden
	Y8            // alleen luma (mono)
)

// String geeft de korte naam van een pixelformaat.
func (p Pixel) String() string {
	switch p {
	case NV12:
		return "nv12"
	case NV21:
		return "nv21"
	case I420:
		return "i420"
	case P010:
		return "p010"
	case Y8:
		return "y8"
	}
	return "none"
}

// Direction is wat een sessie doet.
type Direction uint8

const (
	Decode Direction = iota // bitstream in, frames uit
	Encode                  // frames in, bitstream uit
)

// String geeft "decode" of "encode".
func (d Direction) String() string {
	if d == Encode {
		return "encode"
	}
	return "decode"
}

// Config opent een sessie. Bij Decode is Pixel het gewenste outputformaat en
// zijn Width/Height hooguit een hint: de stream bepaalt de werkelijke maat, en
// die komt terug als een Format-event. Bij Encode is Pixel het formaat van de
// frames die de aanroeper aanlevert en zijn Width/Height verplicht.
type Config struct {
	Codec  Codec
	Dir    Direction
	Pixel  Pixel
	Width  int
	Height int
}

// Buffer is een aaneengesloten stuk fysiek geheugen dat het codec-ijzer mag
// lezen of vullen. Fysiek, want het ijzer DMA't erin — wie hem aanbiedt staat
// ervoor in dat hij niet verhuist en dat de cache-staat klopt (op de O6N is de
// VPU niet coherent: _CCA=0 in de DSDT).
type Buffer struct {
	PA   uintptr
	Size uint64
}

// Flag beschrijft een aangeboden input-buffer.
type Flag uint8

const (
	// EOS markeert het einde van de stream: het ijzer loopt zijn interne
	// vertraging leeg en sluit af met een EOS-event. Een decoder houdt
	// frames vast (herordening), dus zonder dit signaal blijven de laatste
	// frames binnen.
	EOS Flag = 1 << iota
	// Headers markeert een buffer met alleen codec-configuratie (de
	// extradata/codec-private van een container: VPS/SPS/PPS). Het is geen
	// frame en levert dus geen Produced-event op.
	Headers
	// KeyFrame vraagt bij Encode om een IDR op dit frame.
	KeyFrame
)

// Plane is één vlak van een frame binnen een Buffer.
type Plane struct {
	Off    uint64 // vanaf Buffer.PA
	Stride int    // bytes per regel
}

// Layout is wat het ijzer van de stream begrepen heeft: hoe groot een
// outputbuffer moet zijn, hoeveel het er tegelijk nodig heeft, en waar de
// vlakken in zo'n buffer komen te liggen. Het komt als Format-event zodra de
// eerste headers gelezen zijn, en kan middenin een stream opnieuw komen
// (resolutiewissel). Wie output aanbiedt vóór dat event mag alsnog een tweede
// ronde krijgen; daarom is dit een event en geen returnwaarde van Open.
type Layout struct {
	Width       int // zichtbaar beeld
	Height      int
	AllocWidth  int // wat een buffer moet kunnen dragen (macroblok-afronding)
	AllocHeight int
	Pixel       Pixel
	Planes      [3]Plane
	FrameSize   uint64 // minimale buffergrootte voor één frame
	MinBuffers  int    // hoeveel het ijzer er tegelijk vasthoudt
}

// Kind is het soort event dat uit een sessie komt.
type Kind uint8

const (
	// Consumed: een ingevoerde buffer is verwerkt en weer van de aanroeper.
	Consumed Kind = iota + 1
	// Produced: een aangeboden buffer bevat nu een frame (Decode) of
	// bitstream (Encode).
	Produced
	// Format: Layout is bekend of veranderd.
	Format
	// Done: de stream is afgelopen (na een EOS-invoer).
	Done
	// Fault: het ijzer meldt een fout; Err vertelt welke. Een sessie met een
	// Fault is verloren — sluiten en opnieuw openen.
	Fault
)

// Event is één ding dat er in een sessie gebeurde. Buf is de buffer waar het
// over gaat (Consumed/Produced), Tag is wat de aanroeper bij Feed meegaf: zo
// vindt hij zijn eigen timestamp terug zonder dat wij er iets van hoeven weten.
type Event struct {
	Kind   Kind
	Buf    Buffer
	Tag    uint64
	Bytes  uint64 // Produced: gevulde bytes (Encode) of framegrootte (Decode)
	Key    bool   // Produced bij Encode: dit is een keyframe
	Layout Layout // Format (en meegegeven bij Produced na Decode)
	Err    error
}

// Session is één lopende codec-opdracht. Alle methodes zijn non-blocking: het
// ijzer werkt asynchroon en Poll is de enige plek waar voortgang vandaan komt.
// Een sessie hoort bij één aanroeper; gelijktijdig gebruik uit meerdere
// goroutines mag, maar de volgorde van Feed bepaalt de volgorde van de stream.
type Session interface {
	// Feed voert n bytes uit b in. De buffer blijft van het ijzer tot hij als
	// Consumed-event terugkomt.
	Feed(b Buffer, n uint64, f Flag, tag uint64) error

	// Offer biedt een lege buffer aan waar het ijzer zijn resultaat in zet.
	// Een decoder heeft er Layout.MinBuffers tegelijk nodig om door te kunnen
	// werken; minder betekent stilstand, niet verlies.
	Offer(b Buffer) error

	// Poll haalt het volgende event op. false = niets te melden.
	Poll() (Event, bool)

	// Close breekt de sessie af en geeft het ijzer vrij. Aangeboden buffers
	// zijn daarna weer van de aanroeper.
	Close() error
}

// FirmwareSource levert de firmware-blob die sommige codec-blokken nodig
// hebben (de Linlon V8 laadt een eigen binary per codec, ~300KB). De drivers
// kennen geen bestandssysteem — dat mogen ze niet, en ze hoeven het niet: HOP
// hangt hier zijn eigen bron onder, van hopfs, uit de object-store of uit een
// cache in RAM. Een board waarvan het ijzer geen firmware nodig heeft kan een
// nil-bron krijgen.
type FirmwareSource interface {
	// Load geeft de binary voor een naam als "hevcdec" of "h264enc".
	Load(name string) ([]byte, error)
}

// Stateful wordt optioneel geïmplementeerd door een engine die zijn
// hardwarestaat kan beschrijven. Puur voor diagnose: als er niets uit een
// sessie komt is de eerste vraag of het ijzer het werk heeft aangenomen.
type Stateful interface {
	State() string
}

// Engine is het codec-ijzer van een node.
type Engine interface {
	// Describe geeft één regel voor de bootlog (wat, welke revisie, hoeveel
	// cores/sessies).
	Describe() string

	// Supports meldt of deze combinatie te openen is.
	Supports(c Codec, d Direction) bool

	// Open start een sessie. ErrBusy als al het ijzer bezet is: dat is geen
	// fout maar een "straks nog eens".
	Open(cfg Config) (Session, error)
}

// De fouten die een aanroeper apart moet kunnen herkennen.
var (
	ErrBusy        = errors.New("codec: all hardware sessions in use")
	ErrUnsupported = errors.New("codec: codec/direction not supported by this hardware")
	ErrClosed      = errors.New("codec: session closed")
)
