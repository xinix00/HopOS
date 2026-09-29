package mve

// Het berichtenprotocol tussen host en firmware (host interface v2/v3). Alleen
// wat HopOS werkelijk stuurt of begrijpt: starten, stoppen, buffers heen en
// weer, en de drie antwoorden waar een decoder zijn beeldformaat mee aankondigt.
// De firmware kent er meer (encoder-afstemming, statistiek, OSD, rotatie); die
// codes zijn hier bewust weggelaten in plaats van als dode constanten bewaard.
const (
	// Verzoeken van host naar firmware.
	reqGo          = 1001 // begin te werken aan de aangeboden buffers
	reqStop        = 1002
	reqOutputFlush = 1004
	reqJob         = 1009 // cores + hoeveel frames deze beurt
	reqIdleAck     = 1012

	// Antwoorden van firmware naar host.
	respSwitchedIn     = 2001
	respSwitchedOut    = 2002
	respOptionConfirm  = 2003
	respJobDequeued    = 2004
	respInput          = 2005 // een invoerbuffer is verwerkt
	respOutput         = 2006 // een uitvoerbuffer is gevuld
	respInputFlushed   = 2007
	respOutputFlushed  = 2008
	respPong           = 2009
	respError          = 2010
	respStateChange    = 2011
	respIdle           = 2013
	respFrameAllocParm = 2014 // hoe groot een framebuffer moet zijn
	respSeqParams      = 2015 // wat de stream blijkt te zijn
	respEvent          = 2016
	respOptionFail     = 2017
	respRefFrameUnused = 2018

	// Buffers, in beide richtingen.
	bufFrame     = 3001
	bufBitstream = 3002
	bufGeneral   = 3004 // de hoogste code; recv weigert alles daarboven
)

// Gebeurtenissen uit respEvent. Deze gaan over de stroom, niet over één
// buffer: de firmware meldt hiermee dat hij iets aantrof wat hij niet kan of
// niet vertrouwt.
const (
	evStreamCorrupt     = 1
	evStreamUnsupported = 2
)

// Chroma-formaten zoals de firmware ze meldt in respSeqParams, en zoals ze in
// het pixelformaat-bitveld hieronder staan.
const (
	chromaMono   = 0
	chromaYUV420 = 1
)

// Vlaggen op een bitstream-buffer (invoer bij decode, uitvoer bij encode).
const (
	bsFlagEOS         = 0x00000001
	bsFlagEndOfFrame  = 0x00000010
	bsFlagSyncFrame   = 0x00000020
	bsFlagCodecConfig = 0x00000080
)

// Vlaggen op een frame-buffer (uitvoer bij decode, invoer bij encode).
const (
	frFlagForceIDR = 0x00000400
	frFlagRejected = 0x00001000
	frFlagCorrupt  = 0x00002000
	frFlagDecOnly  = 0x00004000
	frFlagRefFrame = 0x00008000
	frFlagEOS      = 0x00010000
)

// De pixelformaat-code van de firmware is een bitveld: chroma, bitdiepte,
// aantal vlakken en een variant-nummer. Zo hoeven wij geen tabel met magische
// getallen te onderhouden — het formaat rekenen we uit wat we willen.
//
//	bit 0-2  chroma-subsampling
//	bit 4-7  maximale bitdiepte min 8
//	bit 8-9  aantal vlakken
//	bit 12-13 variant
//	bit 15   AFBC
func fwFormat(chroma, depth, planes, variant int) uint16 {
	return uint16(chroma) | uint16(depth-8)<<4 | uint16(planes)<<8 | uint16(variant)<<12
}

// De formaten die HopOS aan een app aanbiedt. P010 is de belangrijkste van de
// twee: UHD Blu-ray is 10-bit, en wie dat naar 8 bit afvlakt gooit precies de
// dynamiek weg waar Dolby Vision op rust.
var (
	fmtNV12 = fwFormat(chromaYUV420, 8, 2, 0)
	fmtNV21 = fwFormat(chromaYUV420, 8, 2, 1)
	fmtI420 = fwFormat(chromaYUV420, 8, 3, 0)
	fmtP010 = fwFormat(chromaYUV420, 16, 2, 0)
	fmtY8   = fwFormat(chromaMono, 8, 1, 0)
)
