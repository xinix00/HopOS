package tunnel

import (
	"crypto/x509"
	_ "embed"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/xinix00/lean/leantls"
	"github.com/xinix00/lean/leantls/x509verify"
)

//go:embed cfroots.pem
var cfRootsPEM []byte

// edgeServerName is de SNI-naam voor de http2-transport. Er is bewust GEEN
// ALPN: de edge onderhandelt niets over de laag erboven (gemeten 19-08, en
// cloudflared's TLSSettings voor HTTP2 zet ook alleen ServerName). De
// quic-transport eist wél ALPN "argotunnel" — die tak zit hier niet in.
const edgeServerName = "h2.cftunnel.com"

// edgePort is waar origintunneld luistert.
const edgePort = "7844"

// edgeSRV is het SRV-record dat de regio's aanwijst. De namen erachter
// (region1/region2.v2.argotunnel.com) hebben meerdere A- en AAAA-records; per
// verbinding kiezen we een ander adres, zodat vier verbindingen niet allemaal
// op dezelfde edge-machine landen.
const edgeSRV = "_v2-origintunneld._tcp.argotunnel.com"

// edgeFallback is waar we naartoe gaan als SRV niet lukt. Een node zonder
// SRV-capabele resolver (of een tamago-stack die het niet kan) moet nog steeds
// een tunnel kunnen opzetten, en deze twee namen zijn de vaste ingangen die
// cloudflared zelf ook als regio's kent.
var edgeFallback = []string{"region1.v2.argotunnel.com", "region2.v2.argotunnel.com"}

// EdgeAddrs zoekt de adressen van de edge. Het resultaat is een lijst
// host:poort, in de volgorde waarin we ze willen proberen.
func EdgeAddrs(logf func(string, ...any)) ([]string, error) {
	hosts := edgeFallback
	if _, records, err := net.LookupSRV("", "", edgeSRV); err == nil && len(records) > 0 {
		hosts = nil
		for _, r := range records {
			name := r.Target
			if n := len(name); n > 0 && name[n-1] == '.' {
				name = name[:n-1]
			}
			hosts = append(hosts, name)
		}
	} else if err != nil {
		// Geen fout: de vaste namen doen het ook. Wel zeggen, want het verklaart
		// waarom een node altijd op dezelfde twee regio's uitkomt.
		logf("cloudflared-lean: SRV lookup failed (%v); using the fixed regions", err)
	}

	var addrs []string
	for _, host := range hosts {
		ips, err := net.LookupIP(host)
		if err != nil {
			logf("cloudflared-lean: cannot resolve %s: %v", host, err)
			continue
		}
		for _, ip := range ips {
			if ip.To4() == nil {
				// Het slot draait IPv4; leannet's v6-baan is opt-in en de edge is
				// via v4 volledig bereikbaar.
				continue
			}
			addrs = append(addrs, net.JoinHostPort(ip.String(), edgePort))
		}
	}
	if len(addrs) == 0 {
		return nil, errors.New("no edge addresses resolved")
	}
	return addrs, nil
}

// edgeRoots is de CertPool uit cfroots.pem, één keer ontleed.
var edgeRoots = func() *x509.CertPool {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cfRootsPEM) {
		panic("cloudflared-lean: cfroots.pem carries no usable certificate")
	}
	return pool
}()

// DialEdge opent TCP naar addr en doet de TLS-handshake met leantls.
func DialEdge(addr string, timeout time.Duration) (net.Conn, error) {
	raw, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	// De handshake zelf begrenzen: een edge die de TLS-dans halverwege stil laat
	// vallen mag geen goroutine en socket vasthouden.
	if err := raw.SetDeadline(time.Now().Add(timeout)); err != nil {
		raw.Close()
		return nil, err
	}
	conn, err := leantls.Client(raw, &leantls.Config{
		ServerName:          edgeServerName,
		VerifyPeer:          x509verify.Chain(edgeRoots),
		SignatureAlgorithms: x509verify.SignatureAlgorithms,
	})
	if err != nil {
		raw.Close()
		return nil, fmt.Errorf("tls handshake with %s: %w", addr, err)
	}
	// Deadline eraf: hierna leeft de verbinding zolang de tunnel leeft, en de
	// levendigheid meten we met PING en niet met een klok op de socket.
	if err := raw.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
