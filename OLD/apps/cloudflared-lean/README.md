# cloudflared-lean — de Cloudflare-tunnel op onze eigen fundamenten

Een node achter NAT publiek bereikbaar maken, zonder inkomende poort en zonder
kernel. Zelfde doel als [apps/cloudflared](../cloudflared/) — dat draait
cloudflared's **eigen** CLI in een slot — maar dit programma praat het
tunnelprotocol zelf, op lean.

```
29,87 MB RAM   cloudflared (hun CLI, alle features)
 4,44 MB RAM   cloudflared-lean            85% kleiner
```

Dat verschil zit niet in de netwerklaag. `net/http` (381 kB), `crypto/tls`
(273 kB), `quic-go` (238 kB) en `x/net/http2` (169 kB) zijn samen ongeveer één
megabyte van die dertig: de netstack omruilen alléén levert bijna niets op. Het
verschil is hun featureboom, en daarin is één afhankelijkheid de olifant:
**`gopacket/layers` is 7,7 MB — 26% van het beeld — en dient de ICMP-proxy, die
op tamago al een stub is** (`ingress/icmp_generic.go` zegt letterlijk "not
implemented"). cloudflared's eigen code is maar 741 kB van het totaal.

## Wat het is

| pakket | doet |
|---|---|
| `internal/h2` | HTTP/2 — als **server** (zie hieronder) |
| `internal/hpack` | HPACK: statische tabel, dynamische tabel, Huffman |
| `internal/capnp` | Cap'n Proto: één segment bouwen, één lezen, stream-omhulling |
| `internal/tunnel` | edge-discovery, TLS met Cloudflare's CA's, registratie, config-push |
| `internal/ingress` | de routeertabel die Cloudflare naar ons duwt |
| `internal/origin` | de poot naar de lokale dienst, via leanhttp |

Daarbuiten: `leantls` voor TLS 1.3 en `leanhttp` voor de oorsprong. Geen
gepinde vreemde module, geen patch-map, geen prepare-script — `tools/build.sh`
en klaar.

## Vier dingen die het protocol anders doet dan je zou denken

Alles hieronder is gemeten tegen `region1.v2.argotunnel.com:7844` (19-08), niet
uit documentatie overgenomen.

1. **De edge is de HTTP/2-client, wij zijn de server.** Wij bellen uit, maar
   daarna stuurt de edge óns de `PRI * HTTP/2.0`-preface en opent hij de
   streams. cloudflared doet `http2.Server.ServeConn` op een uitgaande
   verbinding. Er is dus geen h2-clientkant nodig.
2. **Geen ALPN.** De http2-transport zet alleen een SNI-naam
   (`h2.cftunnel.com`). Alleen de quic-transport eist ALPN `argotunnel`, en die
   tak zit hier niet in.
3. **De edge bewijst zich met een CloudFlare Origin Certificate**, niet publiek
   vertrouwd. Met de systeemroots faalt élke handshake; het moet tegen hun eigen
   drie CA's (`internal/tunnel/cfroots.pem`, dezelfde die cloudflared inbakt).
4. **Zonder PING-antwoord opent de edge nooit een stream.** Hij pingt eerst en
   meet er onze levendigheid mee. Dit kostte de eerste avond: TLS stond, h2
   stond, en er gebeurde niets.

Daarna opent de edge stream 1 met `GET /` en de kop
`cf-cloudflared-proxy-connection-upgrade: control-stream`. Daarover gaat de
Cap'n Proto-registratie: bootstrap, dan `registerConnection(auth, tunnelId,
connIndex, options)`, en het antwoord is de colo waar we landden.

## Config komt van Cloudflare

Een remote-managed tunnel krijgt zijn ingress geduwd: een verzoek met
`cf-cloudflared-proxy-connection-upgrade: update-configuration` en een JSON-body
`{"version": N, "config": {…}}`. `internal/ingress` past die toe als de versie
nieuwer is, en wisselt de hele tabel achter één atomic pointer om.

Waarom géén `leanhttp.Mux` daarvoor: die routeert op pad, kiest de meest
specifieke route onafhankelijk van registratie-orde, en staat vast zodra Serve
loopt. Cloudflare routeert eerst op hostname (exact of `*.suffix`), zijn pad is
een **regex**, hij neemt de **eerste** passende regel, en zijn config komt
binnen terwijl we draaien. Vier verschillen, dus een eigen tabel — die kleiner
is dan het verschil zou zijn.

## Draaien

```sh
tools/build.sh                 # host-tests + beide slot-images in out/
TUNNEL_TOKEN=… out/cloudflared-lean-host   # tegen de echte edge, vanaf een host
```

| env | wat |
|---|---|
| `TUNNEL_TOKEN` | verplicht: de named tunnel uit het dashboard |
| `TUNNEL_URL` | waar verkeer heen gaat vóór de eerste config-push (default `http://$HOPOS_HOST`) |
| `TUNNEL_CONNECTIONS` | aantal edge-verbindingen, 1..8 (default 4, zoals cloudflared) |

Jobspec — 32 MB is ruim (het beeld is 4,44 MB):

```json
{"name":"cloudflared-lean","driver":"hop","memory_limit":33554432,
 "tags":{"sharegroup":"huis"},
 "env":{"TUNNEL_TOKEN":"…","TUNNEL_URL":"http://10.100.0.2:80"},
 "artifacts":[
   {"url":"…/cloudflared-lean-arm64-tamago.elf","match":{"node.arch":"arm64"}},
   {"url":"…/cloudflared-lean-riscv64-tamago.elf","match":{"node.arch":"riscv64"}}]}
```

## Wat het (nog) niet draagt

Bewust weigeren met een reden is duidelijker dan half werken:

- **Websockets** — het pad bestaat in h2, maar zonder test tegen een echte
  websocket-oorsprong beloven we het niet.
- **Kale TCP-stromen** (WARP, `cloudflared access`) — antwoordt 502.
- **https-oorsprongen** — vraagt leanhttps plus een keuze over
  certificaatverificatie; Cloudflare's default is "niet verifiëren" en dat is
  geen keuze om stil te maken.
- **De quic-transport** — http2 is genoeg en scheelt een QUIC-stack.
- **Metrics- en management-endpoints, auto-update, diagnostiek.**

## Licentie en herkomst

Deze code is nieuw. Wat eruit is overgenomen zijn feiten, niet regels: de
kop-namen, de interface- en methode-id's van `RegistrationServer`, de
struct-layouts en de drie CA-certificaten — alle uit de gepinde
[cloudflared](https://github.com/cloudflare/cloudflared) (Apache 2.0) en uit
[capnproto2](https://github.com/zombiezen/go-capnproto2)'s `rpc.capnp`. De
Huffman-tabel in `internal/hpack` is RFC 7541 bijlage B.

De naam is `cloudflared-lean` en niet `cloudflared`: Apache 2.0 geeft geen
merkrechten, en dit is niet hun build.
