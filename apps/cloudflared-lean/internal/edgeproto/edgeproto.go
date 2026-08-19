// Package edgeproto draagt de twee antwoordkoppen die de Cloudflare-edge eist.
//
// Dit is de val waar een eigen tunnel in loopt, en hij is stil: je stuurt de
// koppen van de oorsprong netjes als HTTP/2-koppen mee, je krijgt een 200, en
// de bezoeker ziet je HTML als platte tekst. GEMETEN 19-08: van
// `content-type: text/html; charset=utf-8` kwam bij de browser niets aan,
// terwijl onze eigen bytes aantoonbaar goed waren (x/net's HPACK-decoder leest
// ze correct). De edge negeert simpelweg wat hij niet kent.
//
// Zo doet cloudflared het (connection/header.go + http2.go, WriteRespHeaders):
//
//   - ALLE koppen van de oorsprong gaan in ÉÉN kop,
//     cf-cloudflared-response-headers, als base64(naam):base64(waarde) per paar,
//     gescheiden door puntkomma's. base64 is de RawStdEncoding — zónder '='-
//     opvulling. Dat omhulsel bestaat omdat HTTP/1-waarden dingen mogen bevatten
//     die HTTP/2-koppenvalidatie zou weigeren; door ze te coderen komt een
//     oorsprong-kop ongeschonden aan de andere kant uit.
//   - cf-cloudflared-response-meta zegt WAAR het antwoord vandaan komt:
//     {"src":"origin"} voor iets dat de oorsprong stuurde, {"src":"cloudflared"}
//     voor een antwoord dat de tunnel zelf maakte (een 502 omdat de dienst niet
//     opnam). De edge gebruikt dat voor zijn eigen foutpagina's en statistiek.
//   - Koppen die de edge zelf bestuurt gaan NIET in de bundel: alles met een
//     ':'-, cf-int-, cf-cloudflared- of cf-proxy-voorvoegsel.
package edgeproto

import (
	"encoding/base64"
	"strings"
)

// De koppen zelf. Dit zijn cloudflared's eigen namen, dus hun documentatie en
// hun edge-gedrag gelden hier ongewijzigd.
const (
	HeaderUserHeaders = "cf-cloudflared-response-headers"
	HeaderMeta        = "cf-cloudflared-response-meta"
)

// Source is wie het antwoord maakte. De edge onderscheidt dat, en een tunnel die
// altijd "origin" zegt liegt over zijn eigen foutpagina's.
type Source string

const (
	// FromOrigin: de lokale dienst antwoordde.
	FromOrigin Source = `{"src":"origin"}`
	// FromTunnel: dit antwoord komt van de tunnel zelf (geen route, dienst dood).
	FromTunnel Source = `{"src":"cloudflared"}`
)

// enc is RawStdEncoding: standaard-alfabet, géén opvulling. Dat is niet vrij te
// kiezen — de edge decodeert met exact deze variant.
var enc = base64.RawStdEncoding

// Headers bouwt de twee koppen voor één antwoord. user zijn de koppen van de
// oorsprong, met kleine letters als naam (HTTP/2-conventie).
//
// Wat er NIET in hoort en waarom:
//
//   - content-length: cloudflared stuurt die ook als gewone HTTP/2-kop omdat de
//     edge hem daar gebruikt, maar leanh2 weigert dat veld in een antwoord —
//     DATA plus END_STREAM zegt al precies waar de body ophoudt, en twee bronnen
//     voor één lengte is een bron die kan tegenspreken. Wij laten hem dus weg;
//     de edge leidt de lengte af uit het einde van de stream.
//   - de control-koppen van de edge (zie ControlHeader): die bestuurt hij zelf.
func Headers(user map[string][]string, src Source) map[string][]string {
	var b strings.Builder
	for name, values := range user {
		lower := strings.ToLower(name)
		if ControlHeader(lower) || lower == "content-length" {
			continue
		}
		for _, v := range values {
			if b.Len() > 0 {
				b.WriteByte(';')
			}
			b.WriteString(enc.EncodeToString([]byte(lower)))
			b.WriteByte(':')
			b.WriteString(enc.EncodeToString([]byte(v)))
		}
	}
	return map[string][]string{
		HeaderUserHeaders: {b.String()},
		HeaderMeta:        {string(src)},
	}
}

// ControlHeader zegt of de edge deze kop zelf bestuurt. Zelfde regel als
// cloudflared's IsControlResponseHeader.
func ControlHeader(lowerName string) bool {
	return strings.HasPrefix(lowerName, ":") ||
		strings.HasPrefix(lowerName, "cf-int-") ||
		strings.HasPrefix(lowerName, "cf-cloudflared-") ||
		strings.HasPrefix(lowerName, "cf-proxy-")
}

// Deserialize is de omgekeerde weg. Hij bestaat voor de tests: zonder hem is de
// enige controle op onze codering "de browser doet iets".
func Deserialize(value string) (map[string][]string, error) {
	out := map[string][]string{}
	if value == "" {
		return out, nil
	}
	for _, pair := range strings.Split(value, ";") {
		if pair == "" {
			continue
		}
		name, val, ok := strings.Cut(pair, ":")
		if !ok {
			return nil, errMalformed
		}
		n, err := enc.DecodeString(name)
		if err != nil {
			return nil, err
		}
		v, err := enc.DecodeString(val)
		if err != nil {
			return nil, err
		}
		out[string(n)] = append(out[string(n)], string(v))
	}
	return out, nil
}

var errMalformed = &malformedError{}

type malformedError struct{}

func (*malformedError) Error() string {
	return "edgeproto: serialized header pair without a separator"
}
