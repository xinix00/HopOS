package origin

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/xinix00/HopOS/apps/cloudflared-lean/internal/edgeproto"
	"github.com/xinix00/lean/leanh2"
	"golang.org/x/net/http2/hpack"
)

// De hele consumer-keten in één test, zonder Cloudflare ertussen: een oorsprong
// die koppen zet, onze Proxy ertussen, en een h2-client die het antwoord
// ontleedt. Dit is de test die ontbrak toen een browser de interface als tekst
// toonde (Derek, 19-08): via de tunnel kwam er geen content-type mee, en curl
// klaagt daar niet over.
func TestProxyForwardsResponseHeaders(t *testing.T) {
	// 1. De oorsprong: precies de koppen die stulp ook stuurt.
	ol, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ol.Close()
	go http.Serve(ol, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Stulp-Version", "v0.4.1")
		w.Write([]byte("<html><title>Stulp</title></html>"))
	}))
	service := "http://" + ol.Addr().String()

	// 2. Onze h2-server met de Proxy als handler, op een echte socket.
	sl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sl.Close()
	go func() {
		c, err := sl.Accept()
		if err != nil {
			return
		}
		leanh2.NewConn(c, func(req *leanh2.Request, res *leanh2.Response) {
			if err := Proxy(service, req, res); err != nil {
				t.Errorf("Proxy: %v", err)
			}
		}, nil).Serve()
	}()

	// 3. De h2-client: preface, settings, één GET, en het antwoord ontleden.
	conn, err := net.Dial("tcp", sl.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	// De preface letterlijk: leanh2 exporteert hem niet meer (die constante
	// bestond voor Sniff, en dat is gemurderd).
	conn.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"))
	writeFrame(t, conn, 0x4, 0, 0, nil) // SETTINGS

	var block []byte
	enc := hpack.NewEncoder(&byteSink{&block})
	for _, f := range []hpack.HeaderField{
		{Name: ":method", Value: "GET"}, {Name: ":scheme", Value: "http"},
		{Name: ":path", Value: "/"}, {Name: ":authority", Value: "demo.test"},
	} {
		enc.WriteField(f)
	}
	writeFrame(t, conn, 0x1, 0x4|0x1, 1, block) // HEADERS, END_HEADERS|END_STREAM

	got := map[string]string{}
	for i := 0; i < 12; i++ {
		typ, _, _, body := readFrame(t, conn)
		if typ != 0x1 { // alleen het antwoord-HEADERS interesseert ons
			continue
		}
		dec := hpack.NewDecoder(4096, func(f hpack.HeaderField) { got[f.Name] = f.Value })
		if _, err := dec.Write(body); err != nil {
			t.Fatalf("antwoordkoppen niet te ontleden: %v", err)
		}
		break
	}
	if got[":status"] != "200" {
		t.Fatalf("status = %q, wil 200 (koppen: %v)", got[":status"], got)
	}
	// De edge verwacht de oorsprong-koppen in ÉÉN gebundelde kop, niet plat —
	// plat stuurde hij ze in de prullenbak en zag de bezoeker HTML als tekst
	// (19-08). Dus: ontbundelen en dán toetsen.
	if got[edgeproto.HeaderMeta] != string(edgeproto.FromOrigin) {
		t.Errorf("meta-kop = %q, wil %q", got[edgeproto.HeaderMeta], edgeproto.FromOrigin)
	}
	user, err := edgeproto.Deserialize(got[edgeproto.HeaderUserHeaders])
	if err != nil {
		t.Fatalf("bundel niet te ontleden: %v (koppen: %v)", err, got)
	}
	first := func(n string) string {
		if v := user[n]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	if want := "text/html; charset=utf-8"; first("content-type") != want {
		t.Errorf("content-type = %q, wil %q (bundel: %v)", first("content-type"), want, user)
	}
	for _, naam := range []string{"x-content-type-options", "x-stulp-version"} {
		if first(naam) == "" {
			t.Errorf("kop %q ontbreekt (bundel: %v)", naam, user)
		}
	}
	if first("content-length") != "" {
		t.Errorf("content-length lekte door: %q", first("content-length"))
	}
	// En plat mag hij er níet meer staan, anders doet de edge er alsnog niets mee.
	if got["content-type"] != "" {
		t.Errorf("content-type stond ook plat in de koppen: %q", got["content-type"])
	}
}

type byteSink struct{ p *[]byte }

func (b *byteSink) Write(v []byte) (int, error) { *b.p = append(*b.p, v...); return len(v), nil }

func writeFrame(t *testing.T, c net.Conn, typ, flags byte, stream uint32, body []byte) {
	t.Helper()
	head := []byte{byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body)), typ, flags, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(head[5:], stream)
	if _, err := c.Write(append(head, body...)); err != nil {
		t.Fatal(err)
	}
}

func readFrame(t *testing.T, c net.Conn) (typ, flags byte, stream uint32, body []byte) {
	t.Helper()
	head := make([]byte, 9)
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatalf("frame lezen: %v", err)
	}
	n := int(head[0])<<16 | int(head[1])<<8 | int(head[2])
	body = make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(c, body); err != nil {
			t.Fatalf("frame-body lezen: %v", err)
		}
	}
	return head[3], head[4], binary.BigEndian.Uint32(head[5:]) & 0x7fffffff, body
}

// Een PUT met een piepklein lichaam moet gewoon werken tegen een oorsprong die
// GEEN "Expect: 100-continue" ondersteunt — en leanhttp's server (die stulp
// draait) weigert élke Expect met 417, bewust.
//
// Dit was de bug van 19-08: de proxy koos de streamende weg zodra er een
// content-length stond, niet pas als het lichaam groot was. leanhttp's client
// zet bij een stroom altijd Expect (een niet-herhaalbare body vraagt eerst een
// verdict), dus kreeg Dereks PUT van vijftien bytes een 417 — en zijn
// backup-restore net zo.
func TestProxyPutSmallBodyToOriginWithoutExpect(t *testing.T) {
	var gotMethod, gotBody, gotExpect string
	ol, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ol.Close()
	go http.Serve(ol, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Zoals leanhttp's server: elke Expect is een 417 en de body wordt niet
		// gelezen. Go's net/http zou 100-continue anders zélf afhandelen, en dan
		// bewijst deze test niets.
		if e := r.Header.Get("Expect"); e != "" {
			gotExpect = e
			w.WriteHeader(417)
			return
		}
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotBody = r.Method, string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	service := "http://" + ol.Addr().String()

	sl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer sl.Close()
	go func() {
		c, err := sl.Accept()
		if err != nil {
			return
		}
		leanh2.NewConn(c, func(req *leanh2.Request, res *leanh2.Response) {
			if err := Proxy(service, req, res); err != nil {
				t.Errorf("Proxy: %v", err)
			}
		}, nil).Serve()
	}()

	conn, err := net.Dial("tcp", sl.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	conn.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"))
	writeFrame(t, conn, 0x4, 0, 0, nil)

	const body = `{"value":true}` // precies zo klein als Dereks capability-PUT
	var block []byte
	enc := hpack.NewEncoder(&byteSink{&block})
	for _, f := range []hpack.HeaderField{
		{Name: ":method", Value: "PUT"}, {Name: ":scheme", Value: "http"},
		{Name: ":path", Value: "/api/manager/devices/device/x/capability/onoff"},
		{Name: ":authority", Value: "demo.test"},
		{Name: "content-type", Value: "application/json"},
		{Name: "content-length", Value: itoa(len(body))},
	} {
		enc.WriteField(f)
	}
	writeFrame(t, conn, 0x1, 0x4, 1, block)        // HEADERS, END_HEADERS
	writeFrame(t, conn, 0x0, 0x1, 1, []byte(body)) // DATA, END_STREAM

	got := map[string]string{}
	for i := 0; i < 12; i++ {
		typ, _, _, payload := readFrame(t, conn)
		if typ != 0x1 {
			continue
		}
		dec := hpack.NewDecoder(4096, func(f hpack.HeaderField) { got[f.Name] = f.Value })
		if _, err := dec.Write(payload); err != nil {
			t.Fatalf("antwoordkoppen: %v", err)
		}
		break
	}
	if gotExpect != "" {
		t.Errorf("de oorsprong zag Expect %q — de proxy stuurde alsnog een stroom", gotExpect)
	}
	if got[":status"] != "200" {
		t.Fatalf("status = %q, wil 200 (koppen: %v)", got[":status"], got)
	}
	if gotMethod != "PUT" || gotBody != body {
		t.Errorf("oorsprong zag %s met body %q, wil PUT met %q", gotMethod, gotBody, body)
	}
	user, err := edgeproto.Deserialize(got[edgeproto.HeaderUserHeaders])
	if err != nil {
		t.Fatal(err)
	}
	if len(user["content-type"]) == 0 || user["content-type"][0] != "application/json" {
		t.Errorf("content-type in de bundel = %v, wil application/json", user["content-type"])
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
