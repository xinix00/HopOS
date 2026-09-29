package tunnel

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// Het token is de enige invoer die van buiten komt vóór er een verbinding staat.
// Een verzonnen token met dezelfde vorm als een echt: account-tag, geheim,
// tunnel-id.
func TestParseToken(t *testing.T) {
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i)
	}
	doc := map[string]string{
		"a": "9c2b680da60a658926b3fe5b3bf5f8ee",
		"s": base64.StdEncoding.EncodeToString(secret),
		"t": "872875bb-e279-4c69-a767-f35286ef9d5d",
	}
	raw, _ := json.Marshal(doc)
	tok, err := ParseToken(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccountTag != doc["a"] {
		t.Errorf("accountTag = %q", tok.AccountTag)
	}
	if len(tok.TunnelSecret) != 32 || tok.TunnelSecret[31] != 31 {
		t.Errorf("geheim = %d bytes", len(tok.TunnelSecret))
	}
	// De UUID hoort zestien bytes te zijn, in wire-orde: 87 28 75 bb …
	want := []byte{0x87, 0x28, 0x75, 0xbb, 0xe2, 0x79, 0x4c, 0x69,
		0xa7, 0x67, 0xf3, 0x52, 0x86, 0xef, 0x9d, 0x5d}
	if len(tok.TunnelID) != 16 {
		t.Fatalf("tunnel-id = %d bytes, wil 16", len(tok.TunnelID))
	}
	for i := range want {
		if tok.TunnelID[i] != want[i] {
			t.Fatalf("tunnel-id = %x, wil %x", tok.TunnelID, want)
		}
	}
}

// Een kapot token hoort te weigeren mét een reden: dit is de fout die iemand op
// zijn eigen node leest als hij de verkeerde waarde in de jobspec plakte.
func TestParseTokenRefusals(t *testing.T) {
	ok := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.StdEncoding.EncodeToString(raw)
	}
	for naam, in := range map[string]string{
		"geen base64":      "dit-is-geen-base64!!",
		"geen json":        base64.StdEncoding.EncodeToString([]byte("hallo")),
		"velden ontbreken": ok(map[string]string{"a": "acc"}),
		"geheim niet b64":  ok(map[string]string{"a": "acc", "s": "!!", "t": "872875bb-e279-4c69-a767-f35286ef9d5d"}),
		"id te kort":       ok(map[string]string{"a": "acc", "s": base64.StdEncoding.EncodeToString([]byte("x")), "t": "872875bb"}),
		"id geen hex":      ok(map[string]string{"a": "acc", "s": base64.StdEncoding.EncodeToString([]byte("x")), "t": "zzzz75bb-e279-4c69-a767-f35286ef9d5d"}),
	} {
		if _, err := ParseToken(in); err == nil {
			t.Errorf("%s: geen fout", naam)
		}
	}
}

// De URL-veilige base64-variant komt in de praktijk voorbij (een token uit een
// dashboard-URL bijvoorbeeld) en hoort ook te werken.
func TestParseTokenURLSafe(t *testing.T) {
	doc, _ := json.Marshal(map[string]string{
		"a": "acc", "s": base64.StdEncoding.EncodeToString([]byte("geheim")),
		"t": "872875bb-e279-4c69-a767-f35286ef9d5d",
	})
	if _, err := ParseToken(base64.RawURLEncoding.EncodeToString(doc)); err != nil {
		t.Errorf("URL-veilig token geweigerd: %v", err)
	}
}
