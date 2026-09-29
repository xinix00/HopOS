package tunnel

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/xinix00/HopOS/apps/cloudflared-lean/internal/capnp"
)

// De registratie is één Cap'n Proto-RPC over de control-stream:
//
//	bootstrap (vraag 0)  →  de RegistrationServer-capability
//	call (vraag 1)       →  registerConnection(auth, tunnelId, connIndex, options)
//	return (antwoord 1)  →  ConnectionResponse: details of een fout
//
// De tweede aanroep wacht niet op het antwoord van de eerste: hij richt zich op
// "het resultaat van vraag 0" (promisedAnswer). Dat is Cap'n Proto's pipelining
// en het is hoe cloudflared het zelf doet — het scheelt een rondgang, en zonder
// pipelining zouden we de capability-tabel van het bootstrap-antwoord moeten
// ontleden voor een verwijzing die we daarna één keer gebruiken.
//
// Alle onderstaande getallen komen uit de gepinde cloudflared en capnproto2,
// niet uit een gok:
//
//	rpc.capnp        Message{data 8, ptr 1}, which op Uint16(0):
//	                 unimplemented 0, abort 1, call 2, return 3, finish 4,
//	                 bootstrap 8
//	                 Bootstrap{data 8, ptr 1}: questionId Uint32(0)
//	                 Call{data 24, ptr 3}: questionId Uint32(0),
//	                 methodId Uint16(4), interfaceId Uint64(8),
//	                 target ptr0, params ptr1
//	                 MessageTarget{data 8, ptr 1}: which Uint16(4),
//	                 importedCap 0 / promisedAnswer 1 op ptr0
//	                 PromisedAnswer{data 8, ptr 1}: questionId Uint32(0),
//	                 transform ptr0
//	                 Payload{data 0, ptr 2}: content ptr0, capTable ptr1
//	                 Return{data 16, ptr 1}: answerId Uint32(0),
//	                 which Uint16(6) (results 0, exception 1), results ptr0
//	tunnelrpc.capnp  RegistrationServer @0xf71695ec7fe85497,
//	                 registerConnection = methode 0
//	                 Params{data 8, ptr 3}: connIndex Uint8(0), auth ptr0,
//	                 tunnelId ptr1, options ptr2
//	                 TunnelAuth{data 0, ptr 2}: accountTag ptr0, secret ptr1
//	                 ClientInfo{data 0, ptr 4}: clientId ptr0, features ptr1,
//	                 version ptr2, arch ptr3
//	                 ConnectionOptions{data 8, ptr 2}: client ptr0,
//	                 originLocalIp ptr1, replaceExisting bit 0,
//	                 compressionQuality Uint8(1), numPreviousAttempts Uint8(2)
//	                 ConnectionResponse{data 8, ptr 1}: which Uint16(0)
//	                 (error 0, connectionDetails 1) op ptr0
//	                 ConnectionDetails: uuid ptr0, locationName ptr1,
//	                 remotelyManaged bit 0
//	                 ConnectionError: cause ptr0, retryAfter Int64(0),
//	                 shouldRetry bit 64
const (
	registrationServerID = 0xf71695ec7fe85497
	methodRegisterConn   = 0
	methodUnregisterConn = 1

	msgCall      = 2
	msgReturn    = 3
	msgBootstrap = 8

	returnResults   = 0
	returnException = 1

	targetPromisedAnswer = 1
)

// Token is wat er in TUNNEL_TOKEN zit: base64 van een JSON met het account, het
// geheim en de tunnel-id.
type Token struct {
	AccountTag   string
	TunnelSecret []byte
	TunnelID     []byte // de 16 bytes van de UUID
}

// ParseToken ontleedt TUNNEL_TOKEN.
func ParseToken(s string) (Token, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// Sommige tokens komen in de URL-veilige variant voorbij.
		raw, err = base64.RawURLEncoding.DecodeString(s)
		if err != nil {
			return Token{}, fmt.Errorf("tunnel token is not base64: %w", err)
		}
	}
	var doc struct {
		AccountTag string `json:"a"`
		Secret     string `json:"s"`
		TunnelID   string `json:"t"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Token{}, fmt.Errorf("tunnel token is not the expected JSON: %w", err)
	}
	if doc.AccountTag == "" || doc.Secret == "" || doc.TunnelID == "" {
		return Token{}, errors.New("tunnel token misses a, s or t")
	}
	secret, err := base64.StdEncoding.DecodeString(doc.Secret)
	if err != nil {
		return Token{}, fmt.Errorf("tunnel secret is not base64: %w", err)
	}
	id, err := parseUUID(doc.TunnelID)
	if err != nil {
		return Token{}, err
	}
	return Token{AccountTag: doc.AccountTag, TunnelSecret: secret, TunnelID: id}, nil
}

// parseUUID leest de streepjes-vorm naar zestien bytes. Geen uuid-pakket voor
// één ontleding van een vaste vorm.
func parseUUID(s string) ([]byte, error) {
	var out []byte
	hi := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '-' {
			continue
		}
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			return nil, fmt.Errorf("tunnel id has a non-hex character %q", string(c))
		}
		if hi < 0 {
			hi = v
			continue
		}
		out = append(out, byte(hi<<4|v))
		hi = -1
	}
	if hi >= 0 || len(out) != 16 {
		return nil, fmt.Errorf("tunnel id is %d bytes, want 16", len(out))
	}
	return out, nil
}

// ConnectionDetails is wat de edge terugmeldt bij een geslaagde registratie.
type ConnectionDetails struct {
	UUID            []byte
	Location        string // de luchthavencode van de colo, bv. AMS
	RemotelyManaged bool
}

// RegisterError is een nette weigering van de edge: hij zegt waarom, en of het
// zin heeft het opnieuw te proberen.
type RegisterError struct {
	Cause       string
	RetryAfter  time.Duration
	ShouldRetry bool
}

func (e *RegisterError) Error() string {
	if e.ShouldRetry {
		return fmt.Sprintf("edge refused this connection: %s (retry after %s)", e.Cause, e.RetryAfter)
	}
	return fmt.Sprintf("edge refused this connection: %s (do not retry)", e.Cause)
}

// ClientInfo is wat we over onszelf melden. Het dashboard toont version en
// arch, dus daar hoort iets eerlijks te staan.
type ClientInfo struct {
	ClientID []byte
	Features []string
	Version  string
	Arch     string
}

// Register doet de registratie over een control-stream die al open staat. rw is
// die stream: schrijven gaat naar de edge, lezen komt eruit.
func Register(rw io.ReadWriter, tok Token, connIndex uint8, info ClientInfo, attempts uint8) (*ConnectionDetails, error) {
	// 1. bootstrap — vraag 0.
	boot := capnp.NewBuilder()
	msg := boot.Root(1, 1)
	msg.SetUint16(0, msgBootstrap)
	b := msg.NewStruct(0, 1, 1)
	b.SetUint32(0, 0) // questionId 0
	if err := capnp.WriteMessage(rw, boot.Message()); err != nil {
		return nil, fmt.Errorf("sending bootstrap: %w", err)
	}

	// 2. call — vraag 1, gericht op het resultaat van vraag 0.
	call := capnp.NewBuilder()
	cm := call.Root(1, 1)
	cm.SetUint16(0, msgCall)
	c := cm.NewStruct(0, 3, 3)
	c.SetUint32(0, 1)                    // questionId
	c.SetUint16(4, methodRegisterConn)   // methodId
	c.SetUint64(8, registrationServerID) // interfaceId

	target := c.NewStruct(0, 1, 1)
	target.SetUint16(4, targetPromisedAnswer)
	answer := target.NewStruct(0, 1, 1)
	answer.SetUint32(0, 0) // het antwoord op vraag 0
	answer.SetEmptyList(0) // transform: leeg = de capability zelf

	payload := c.NewStruct(1, 0, 2)
	params := payload.NewStruct(0, 1, 3)
	params.SetUint8(0, connIndex)

	auth := params.NewStruct(0, 0, 2)
	auth.SetText(0, tok.AccountTag)
	auth.SetData(1, tok.TunnelSecret)

	params.SetData(1, tok.TunnelID)

	options := params.NewStruct(2, 1, 2)
	client := options.NewStruct(0, 0, 4)
	client.SetData(0, info.ClientID)
	client.SetTextList(1, info.Features)
	client.SetText(2, info.Version)
	client.SetText(3, info.Arch)
	// originLocalIp blijft leeg: wij weten ons LAN-adres wel, maar de edge
	// gebruikt het alleen voor weergave en een slot-IP zegt niemand iets.
	options.SetBool(0, true)      // replaceExisting: een wees van een vorige start moet wijken
	options.SetUint8(1, 0)        // compressionQuality 0 = uit; de tunnel draagt al gzip
	options.SetUint8(2, attempts) // numPreviousAttempts

	if err := capnp.WriteMessage(rw, call.Message()); err != nil {
		return nil, fmt.Errorf("sending registerConnection: %w", err)
	}

	// 3. antwoorden lezen tot dat van vraag 1 erbij is. De bootstrap-return komt
	// als eerste voorbij en die slaan we over.
	for i := 0; i < 8; i++ {
		seg, err := capnp.ReadMessage(rw)
		if err != nil {
			return nil, fmt.Errorf("reading the registration answer: %w", err)
		}
		r := capnp.NewReader(seg)
		root, err := r.RootStruct()
		if err != nil {
			return nil, err
		}
		if root.Uint16(0) != msgReturn {
			continue // abort/unimplemented/finish: niet ons antwoord
		}
		ret, err := root.StructPtr(0)
		if err != nil {
			return nil, err
		}
		if ret.Uint32(0) != 1 {
			continue // het antwoord op de bootstrap
		}
		switch ret.Uint16(6) {
		case returnException:
			exc, err := ret.StructPtr(0)
			if err != nil {
				return nil, err
			}
			reason, _ := exc.Text(0)
			return nil, fmt.Errorf("edge raised an exception: %s", reason)
		case returnResults:
			results, err := ret.StructPtr(0)
			if err != nil {
				return nil, err
			}
			content, err := results.StructPtr(0)
			if err != nil {
				return nil, err
			}
			return readConnectionResponse(content)
		default:
			return nil, fmt.Errorf("edge answered with kind %d, which this client does not handle", ret.Uint16(6))
		}
	}
	return nil, errors.New("edge never answered the registration")
}

// readConnectionResponse ontleedt het resultaat: de union is óf een fout óf de
// details van deze verbinding.
func readConnectionResponse(v capnp.View) (*ConnectionDetails, error) {
	// De results-struct van registerConnection heeft één veld (result), en dat
	// is de ConnectionResponse.
	resp, err := v.StructPtr(0)
	if err != nil {
		return nil, err
	}
	if resp.IsNull() {
		return nil, errors.New("edge answered without a result")
	}
	switch resp.Uint16(0) {
	case 0: // error
		e, err := resp.StructPtr(0)
		if err != nil {
			return nil, err
		}
		cause, _ := e.Text(0)
		return nil, &RegisterError{
			Cause:       cause,
			RetryAfter:  time.Duration(e.Int64(0)),
			ShouldRetry: e.Bool(64),
		}
	case 1: // connectionDetails
		d, err := resp.StructPtr(0)
		if err != nil {
			return nil, err
		}
		uuid, err := d.Bytes(0, false)
		if err != nil {
			return nil, err
		}
		loc, err := d.Text(1)
		if err != nil {
			return nil, err
		}
		return &ConnectionDetails{UUID: uuid, Location: loc, RemotelyManaged: d.Bool(0)}, nil
	default:
		return nil, fmt.Errorf("edge answered with an unknown result kind %d", resp.Uint16(0))
	}
}

// Unregister meldt de verbinding netjes af. De edge haalt hem dan meteen uit de
// rotatie in plaats van te wachten tot hij stilvalt — dat is het verschil
// tussen een herstart zonder en met een gat van tientallen seconden.
func Unregister(rw io.ReadWriter) error {
	call := capnp.NewBuilder()
	cm := call.Root(1, 1)
	cm.SetUint16(0, msgCall)
	c := cm.NewStruct(0, 3, 3)
	c.SetUint32(0, 2)
	c.SetUint16(4, methodUnregisterConn)
	c.SetUint64(8, registrationServerID)
	target := c.NewStruct(0, 1, 1)
	target.SetUint16(4, targetPromisedAnswer)
	answer := target.NewStruct(0, 1, 1)
	answer.SetUint32(0, 0)
	answer.SetEmptyList(0)
	payload := c.NewStruct(1, 0, 2)
	payload.NewStruct(0, 0, 0) // geen parameters
	return capnp.WriteMessage(rw, call.Message())
}
