// cloudflared-lean: de Cloudflare-tunnel op onze eigen fundamenten.
//
// Een eigen module binnen de HopOS-repo, net als apps/cloudflared en
// apps/welcome — een app-image linkt appnet, en dat hoort niet in de
// dependency-graaf van de metal-module. Anders dan apps/cloudflared draagt
// deze module GEEN cloudflared: geen net/http, geen x/net/http2, geen
// quic-go, geen capnproto-runtime, geen gopacket. Alleen lean en de stdlib.
module github.com/xinix00/HopOS/apps/cloudflared-lean

go 1.26.4

require (
	github.com/xinix00/HopOS/metal/v2 v2.0.1
	github.com/xinix00/lean v1.1.0
	golang.org/x/net v0.58.0
)

require github.com/usbarmory/tamago v1.26.4 // indirect

replace github.com/xinix00/HopOS/metal/v2 => ../../metal

// Tot lean's volgende release: leanh2 is hier vandaan verhuisd (h2 + HPACK,
// één pakket volgens lean's derde regel).
