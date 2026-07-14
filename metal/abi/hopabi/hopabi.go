// Package hopabi definieert het wire-format van de hop-ABI-RPC over de
// mailbox-ringen (PLAN.md §3): app → HOP requests op de outbox
// (ring.TypeRPCReq), HOP → app responses op de inbox (ring.TypeRPCResp).
// Voor beide kanten dezelfde encoder/decoder, versie-veld verplicht.
//
// Frame (little-endian), 24-byte kop + variabel:
//
//	req:  ver u8 | op u8 | pathLen u16 | seq u32 | off u64 | n u64 | path | data
//	resp: ver u8 | op u8 | status u16 | seq u32 | size u64 | _ u64 | data
//
// De ops zijn de storage-laag van het plan: elke task heeft een eigen lege
// root; alleen gemounte volumes (shared → local) zijn daarbuiten zichtbaar.
// Stateless (paden, geen fd's): een app-crash laat bij HOP niets achter.
package hopabi

import (
	"encoding/binary"
	"fmt"
)

// Version van dit wire-format.
const Version = 1

// Ops.
const (
	OpStat   = 1 // stat(path) → size (dir: size 0, status Ok)
	OpRead   = 2 // read(path, off, n≤MaxChunk) → data
	OpWrite  = 3 // write(path, off, data≤MaxChunk); maakt bestand + ouder-dirs
	OpList   = 4 // list(path) → namen, "\n"-gescheiden ("naam/" = dir)
	OpRemove = 5 // remove(path) (bestand of lege dir)
	OpFetch  = 6 // fetch(url=path-veld, dst=data-veld): HOP downloadt → size
)

// Status-codes (resp). Bij ≠ StatusOK bevat data de fouttekst.
const (
	StatusOK     = 0
	StatusError  = 1 // algemene fout (tekst in data)
	StatusNoEnt  = 2 // pad bestaat niet
	StatusDenied = 3 // buiten mounts/eigen root
)

// MaxChunk is de maximale datalengte per read/write (ring-record ≤ cap/2).
const MaxChunk = 8 << 10

const hdrLen = 24

// Req is een hop-ABI-request.
type Req struct {
	Op   uint8
	Seq  uint32
	Off  uint64
	N    uint64
	Path string
	Data []byte
}

// Resp is een hop-ABI-response.
type Resp struct {
	Op     uint8
	Status uint16
	Seq    uint32
	Size   uint64
	Data   []byte
}

// Alle meervoudige velden zijn little-endian (zie het frame-commentaar boven):
// encoding/binary is de enige bron van waarheid, zodat een tikfout de wire-
// consistentie tussen HOP en apps niet stil kan breken.
var le = binary.LittleEndian

// EncodeReq serialiseert een request.
func EncodeReq(r Req) []byte {
	b := make([]byte, hdrLen+len(r.Path)+len(r.Data))
	b[0], b[1] = Version, r.Op
	le.PutUint16(b[2:], uint16(len(r.Path)))
	le.PutUint32(b[4:], r.Seq)
	le.PutUint64(b[8:], r.Off)
	le.PutUint64(b[16:], r.N)
	copy(b[hdrLen:], r.Path)
	copy(b[hdrLen+len(r.Path):], r.Data)
	return b
}

// DecodeReq parseert een request (payload van een TypeRPCReq-record).
func DecodeReq(b []byte) (Req, error) {
	if len(b) < hdrLen {
		return Req{}, fmt.Errorf("hopabi: request te kort (%d)", len(b))
	}
	if b[0] != Version {
		return Req{}, fmt.Errorf("hopabi: versie %d, verwacht %d", b[0], Version)
	}
	plen := int(le.Uint16(b[2:]))
	if hdrLen+plen > len(b) {
		return Req{}, fmt.Errorf("hopabi: pathLen %d past niet in %d", plen, len(b))
	}
	return Req{
		Op:   b[1],
		Seq:  le.Uint32(b[4:]),
		Off:  le.Uint64(b[8:]),
		N:    le.Uint64(b[16:]),
		Path: string(b[hdrLen : hdrLen+plen]),
		Data: b[hdrLen+plen:],
	}, nil
}

// EncodeResp serialiseert een response.
func EncodeResp(r Resp) []byte {
	b := make([]byte, hdrLen+len(r.Data))
	b[0], b[1] = Version, r.Op
	le.PutUint16(b[2:], r.Status)
	le.PutUint32(b[4:], r.Seq)
	le.PutUint64(b[8:], r.Size)
	copy(b[hdrLen:], r.Data)
	return b
}

// DecodeResp parseert een response (payload van een TypeRPCResp-record).
func DecodeResp(b []byte) (Resp, error) {
	if len(b) < hdrLen {
		return Resp{}, fmt.Errorf("hopabi: response te kort (%d)", len(b))
	}
	if b[0] != Version {
		return Resp{}, fmt.Errorf("hopabi: versie %d, verwacht %d", b[0], Version)
	}
	return Resp{
		Op:     b[1],
		Status: le.Uint16(b[2:]),
		Seq:    le.Uint32(b[4:]),
		Size:   le.Uint64(b[8:]),
		Data:   b[hdrLen:],
	}, nil
}
