// Package hopabi definieert de compacte request/response-payload van HOP's
// system calls. Sinds slot-ABI v6 draagt systemapi deze payload over het
// gewone interne LAN; de oude mailbox-recordtypes 3/4 zijn alleen gereserveerd.
// Voor beide kanten geldt dezelfde encoder/decoder en een verplicht versieveld.
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
	OpRead   = 2 // read(path, off, n≤transportgrens) → data
	OpWrite  = 3 // write(path, off, data≤transportgrens); maakt bestand + ouder-dirs
	OpList   = 4 // list(path) → namen, "\n"-gescheiden ("naam/" = dir)
	OpRemove = 5 // remove(path) (bestand of lege dir)

	// 6 was OpFetch: HOP downloadde een URL naar het volume van de task. Dat is
	// gesloopt — elke app heeft zijn eigen netstack, dus hij haalt zijn bytes op
	// zijn eigen core. HOP hoefde daarvoor met zijn volle rechten een
	// app-opgegeven URL te openen (redirects incluis) vanaf core 0, en dat is een
	// SSRF-pad naar alles wat de node kan bereiken. Het nummer blijft LEEG: een
	// app-image van vóór deze sloop krijgt zo een nette "onbekende op" in plaats
	// van per ongeluk een ándere operatie.

	OpTruncate = 7 // truncate(path, n=lengte); maakt bestand + ouder-dirs

	// De store-ops: de persistente laag (S3) naast de vluchtige (hopfs).
	// Expliciete kopieën tussen "mijn map in de bucket" (apps/<cluster>/<job>/,
	// HOP dwingt de prefix af) en het eigen hopfs-zicht — nooit een sync-daemon
	// met een onzichtbaar verlies-window. De bytes lopen over HOP (die heeft de
	// creds en de TLS al); dit is bewust géén heropvoering van de gesloopte
	// fetch-op: de app kiest hier geen URL, alleen een naam binnen zijn map.
	OpStorePull = 8  // pull(path): object → eigen pad (vervangend) → size
	OpStorePush = 9  // push(path): eigen pad → object (vervangend) → size
	OpStoreList = 10 // list(path): keys onder eigen map + pad-prefix, "\n"-gescheiden
	OpStoreDrop = 11 // drop(path): object weg (idempotent)

	// 12 en 13 waren OpSurfGrant/OpSurfRevoke: een GUI-app liet de display
	// read-only in zijn vensterbuffer kijken (gui-ontwerp P3). Gesloopt op
	// 06-08, dezelfde dag als gebouwd — gemeten op ijzer bleek er precies één
	// app pixels te sturen (de rest gaat via de scene-laag, die alleen changes
	// stuurt), dus een lokaal-only mechanisme met stage-2-grants erin was de
	// verkeerde prijs. De nummers blijven LEEG: een app-image van vóór de sloop
	// krijgt zo een nette "onbekende op" i.p.v. per ongeluk een andere operatie.
)

// Status-codes (resp). Bij ≠ StatusOK bevat data de fouttekst.
const (
	StatusOK     = 0
	StatusError  = 1 // algemene fout (tekst in data)
	StatusNoEnt  = 2 // pad bestaat niet
	StatusDenied = 3 // buiten mounts/eigen root
)

// MaxChunk is de historische mailboxgrens. Nieuwe systemapi-verbindingen
// begrenzen hun payload met systemapi.MaxIOChunk; deze constante blijft voor
// compatibele payloadtests en voor callers die zelf een kleinere grens willen.
const MaxChunk = 8 << 10

const hdrLen = 24

// HdrLen is de lengte van de responskop: wie een respons in een eigen buffer
// opbouwt (EncodeRespInto) zet de data op HdrLen, en wie hem in delen leest
// (applib.ReadInto) leest eerst HdrLen bytes en dan de data.
const HdrLen = hdrLen

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

// DecodeReq parseert een requestpayload.
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

// EncodeRespInto schrijft de kop van r in dst[:HdrLen]; de n databytes staan
// er al, op dst[HdrLen:HdrLen+n] (de aanroeper liet hopfs daar direct in
// lezen). Geeft dst[:HdrLen+n]. Zelfde wire-vorm als EncodeResp, zonder de
// kopie en zonder een verse MiB per call.
func EncodeRespInto(dst []byte, r Resp, n int) []byte {
	dst[0], dst[1] = Version, r.Op
	le.PutUint16(dst[2:], r.Status)
	le.PutUint32(dst[4:], r.Seq)
	le.PutUint64(dst[8:], r.Size)
	return dst[:hdrLen+n]
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
