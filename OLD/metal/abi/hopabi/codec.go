package hopabi

import "fmt"

// De payloads van de codec-ops. Ze staan apart van hopabi.go omdat ze iets
// anders doen dan de rest van de ABI: de opslag-ops dragen bytes, deze dragen
// AANWIJZINGEN naar bytes die al op hun plek liggen.
//
// Elke struct heeft een vaste lengte en wordt little-endian geschreven, net
// als de kop. Geen varint, geen optionele velden: HOP leest ze op de rand van
// zijn vertrouwensgrens, en daar hoort code die niet kan verrassen.

// Lengtes van de vaste records.
const (
	OpenArgsLen = 8
	FeedArgsLen = 24
	BufArgsLen  = 4
	EventLen    = 64
)

// OpenArgs opent een sessie. Codec, Dir en Pixel zijn de nummering van
// driver/codec; de app kent die namen via applib en hoeft niets van het ijzer
// te weten.
type OpenArgs struct {
	Codec  uint8
	Dir    uint8
	Pixel  uint8
	Width  uint16
	Height uint16
}

// EncodeOpen serialiseert de argumenten van OpCodecOpen.
func EncodeOpen(a OpenArgs) []byte {
	b := make([]byte, OpenArgsLen)
	b[0], b[1], b[2] = a.Codec, a.Dir, a.Pixel
	le.PutUint16(b[4:], a.Width)
	le.PutUint16(b[6:], a.Height)
	return b
}

// DecodeOpen parseert de argumenten van OpCodecOpen.
func DecodeOpen(b []byte) (OpenArgs, error) {
	if len(b) < OpenArgsLen {
		return OpenArgs{}, fmt.Errorf("hopabi: codec open args too short (%d)", len(b))
	}
	return OpenArgs{
		Codec:  b[0],
		Dir:    b[1],
		Pixel:  b[2],
		Width:  le.Uint16(b[4:]),
		Height: le.Uint16(b[6:]),
	}, nil
}

// FeedArgs hoort bij OpCodecFeed. De buffer zelf staat in Req.Off/Req.N.
type FeedArgs struct {
	Handle uint32
	Flags  uint32
	Filled uint64 // hoeveel bytes er werkelijk in staan
	Tag    uint64 // komt ongewijzigd terug op het resultaat
}

// EncodeFeed serialiseert de argumenten van OpCodecFeed.
func EncodeFeed(a FeedArgs) []byte {
	b := make([]byte, FeedArgsLen)
	le.PutUint32(b[0:], a.Handle)
	le.PutUint32(b[4:], a.Flags)
	le.PutUint64(b[8:], a.Filled)
	le.PutUint64(b[16:], a.Tag)
	return b
}

// DecodeFeed parseert de argumenten van OpCodecFeed.
func DecodeFeed(b []byte) (FeedArgs, error) {
	if len(b) < FeedArgsLen {
		return FeedArgs{}, fmt.Errorf("hopabi: codec feed args too short (%d)", len(b))
	}
	return FeedArgs{
		Handle: le.Uint32(b[0:]),
		Flags:  le.Uint32(b[4:]),
		Filled: le.Uint64(b[8:]),
		Tag:    le.Uint64(b[16:]),
	}, nil
}

// BufArgs is de kale sessieverwijzing van Offer, Poll en Close.
type BufArgs struct {
	Handle uint32
}

// EncodeBuf serialiseert een kale sessieverwijzing.
func EncodeBuf(a BufArgs) []byte {
	b := make([]byte, BufArgsLen)
	le.PutUint32(b, a.Handle)
	return b
}

// DecodeBuf parseert een kale sessieverwijzing.
func DecodeBuf(b []byte) (BufArgs, error) {
	if len(b) < BufArgsLen {
		return BufArgs{}, fmt.Errorf("hopabi: codec args too short (%d)", len(b))
	}
	return BufArgs{Handle: le.Uint32(b)}, nil
}

// Event-soorten. Bewust NIET de nummering van driver/codec.Kind (die begint
// bij Consumed): dit is een wire-formaat en dat mag niet verschuiven als er
// intern een constante bijkomt. De kern vertaalt expliciet, per soort
// (slots.wireEvent).
const (
	EventNone     = 0
	EventFormat   = 1 // de stream is herkend; de maten en vlakken staan erin
	EventConsumed = 2 // een invoerbuffer is vrij
	EventProduced = 3 // een resultaat staat in de buffer
	EventDone     = 4 // einde stream
	EventFault    = 5 // de sessie is stuk
)

// Event is wat een Poll oplevert: één gebeurtenis, met de buffer erbij waar
// hij over gaat. Off is weer de afstand vanaf RamStart, zodat de app hem
// herkent zonder iets over fysiek geheugen te weten.
//
// Een Format-event gaat over geen enkele buffer, en hergebruikt daarom twee
// velden: Size is de maat die elke uitvoerbuffer minstens moet hebben
// (Layout.FrameSize) en Bytes het aantal buffers dat het ijzer tegelijk wil
// vasthouden (Layout.MinBuffers).
//
//	0  kind u8 | key u8 | pixel u8 | _ u8
//	4  width u16 | height u16
//	8  off u64 | size u64 | bytes u64 | tag u64
//	40 stride[3] u16 | _ u16
//	48 plane[3] u32 | _ u32      (= 64)
type Event struct {
	Kind   uint8
	Key    bool   // alleen bij een bitstream-resultaat: is dit een keyframe?
	Pixel  uint8  // alleen bij Format
	Width  uint16 // zichtbaar beeld
	Height uint16
	Off    uint64 // buffer, vanaf RamStart
	Size   uint64 // lengte van de buffer; bij Format: minimale buffermaat
	Bytes  uint64 // bruikbare bytes erin (0 = niets bruikbaars); bij Format: minimaal aantal buffers
	Tag    uint64
	Stride [3]int    // bytes per regel, per vlak; 0 = dit vlak bestaat niet
	Plane  [3]uint64 // begin van elk vlak, vanaf het begin van de buffer
}

// EncodeEvent schrijft één event op een vaste plek in b.
func EncodeEvent(b []byte, e Event) {
	_ = b[EventLen-1]
	for i := range b[:EventLen] {
		b[i] = 0
	}
	b[0] = e.Kind
	if e.Key {
		b[1] = 1
	}
	b[2] = e.Pixel
	le.PutUint16(b[4:], e.Width)
	le.PutUint16(b[6:], e.Height)
	le.PutUint64(b[8:], e.Off)
	le.PutUint64(b[16:], e.Size)
	le.PutUint64(b[24:], e.Bytes)
	le.PutUint64(b[32:], e.Tag)
	for i, s := range e.Stride {
		le.PutUint16(b[40+2*i:], uint16(s))
	}
	for i, p := range e.Plane {
		le.PutUint32(b[48+4*i:], uint32(p))
	}
}

// DecodeEvent leest één event.
func DecodeEvent(b []byte) (Event, error) {
	if len(b) < EventLen {
		return Event{}, fmt.Errorf("hopabi: codec event too short (%d)", len(b))
	}
	e := Event{
		Kind:   b[0],
		Key:    b[1] != 0,
		Pixel:  b[2],
		Width:  le.Uint16(b[4:]),
		Height: le.Uint16(b[6:]),
		Off:    le.Uint64(b[8:]),
		Size:   le.Uint64(b[16:]),
		Bytes:  le.Uint64(b[24:]),
		Tag:    le.Uint64(b[32:]),
	}
	for i := range e.Stride {
		e.Stride[i] = int(le.Uint16(b[40+2*i:]))
	}
	for i := range e.Plane {
		e.Plane[i] = uint64(le.Uint32(b[48+4*i:]))
	}
	return e, nil
}
