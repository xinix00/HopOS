# cloudflared-lean

Makes a node behind NAT publicly reachable through a Cloudflare tunnel,
without an inbound port. The app dials out to Cloudflare's edge and speaks the
tunnel protocol itself, on applib and the lean crates. For the full
cloudflared (its own CLI in a slot) see `go/cloudflared`.

```
4.44 MB RAM   cloudflared-lean in Go (TamaGo, measured 2026-08-19)
  530 kB      this image, loaded part (.text, .rodata, .data; release, 2026-09-30)
```

## What it is

| layer | who | does |
| --- | --- | --- |
| TCP | `applib::appnet` | the slot's own netstack (leannet) |
| TLS 1.3 | leanhttps over leantls | SNI `h2.cftunnel.com`, no ALPN, chain checked against Cloudflare's own three CAs (`src/cfroots.pem`) |
| HTTP/2 as server | leanh2 | the edge is the client: it sends the preface, pings and opens the streams |
| registration | `src/register.rs`, `src/capnp.rs` | Cap'n Proto on the control stream: bootstrap, `registerConnection`, the reply |
| ingress | `src/ingress.rs`, `src/regex.rs` | the routing table: hostname exact or `*.x`, path as a regular expression, first rule wins |
| origin | leanhttp, `src/origin.rs` | each request as HTTP/1.1 to the local service, the headers back in the bundle the edge expects |

The four things the protocol does differently than you would expect (the edge
is the HTTP/2 client, no ALPN, a non-public certificate, no stream without a
PING reply) are in the README of the Go predecessor and in the `//!` headers
of `edge.rs` and `tunnel.rs`. On 2026-09-30 the protocol core (TLS with the
chain test, leanh2 as server, the control stream, the registration) ran from
the host against `region1.v2.argotunnel.com:7844` with a made-up token: the
edge replied cleanly with `Unauthorized: Invalid tunnel secret` and "do not
retry". It has not yet run with a real token (that can only be done on
hardware with the token from the dashboard).

A remote-managed tunnel gets its ingress pushed from the dashboard (a stream
with `update-configuration`); the table is then swapped as a whole, or stays
as it was if the push is broken (the edge is told why). Before the first push
`TUNNEL_INGRESS` applies, or a single rule to `TUNNEL_URL`.

## The env

| key | default | what |
| --- | --- | --- |
| `TUNNEL_TOKEN` | (required) | the token of the named tunnel from the dashboard (Zero Trust, Networks, Tunnels, "Install connector": the long base64 after `--token`) |
| `DNS` | (none) | the slot's DNS server, for the edge names and for services with a name; without `DNS` only IP addresses (see `TUNNEL_EDGE`) |
| `TUNNEL_URL` | `http://$HOPOS_HOST`, otherwise `http_status:503` | where traffic goes before the first push |
| `TUNNEL_INGRESS` | (none) | a custom table as JSON, in the dashboard's form: `{"ingress":[{"hostname":"…","path":"…","service":"http://…"},{"service":"http_status:404"}]}`; wins over `TUNNEL_URL`, and every push wins over this |
| `TUNNEL_CONNECTIONS` | `4` | number of edge connections, 1 to 8 (cloudflared's default is 4) |
| `TUNNEL_EDGE` | `region1.v2.argotunnel.com,region2.v2.argotunnel.com` | the edge, as names or IPv4 addresses separated by commas; connection `i` starts at name `i` and moves on with every new attempt |

The token is a secret: it is only in the jobspec that goes to the node, never
in this repo, and the app never puts it in a log line.

The service in a rule is `http://host[:port][/prefix]` (the host an IPv4
address or a name via `DNS`), or `http_status:<code>`. A service in another
slot on the same node is `http://10.100.0.<slot+1>:<port>`; a service on the
LAN uses its own address.

## Markers

| marker | when |
| --- | --- |
| `HOPOS_CFTUNNEL_UP edge=<ip>:7844 conn=<uuid> colo=<code> index=<i> up=<n>` | a connection is registered; `conn` is the edge's connection id |
| `HOPOS_CFTUNNEL_REQ n=<n>` | a forwarded request (method, host, path without query, service, status): each of the first sixteen, then every hundredth |
| `HOPOS_CFTUNNEL_REQ_FAIL n=<n>` | a request that did not reach the service (same rhythm) |
| `HOPOS_CFTUNNEL_CONFIG version=<v> rules=<n>` | the table at start, and every applied push, with the rules below it |
| `HOPOS_CFTUNNEL_CONFIG_FAIL version=<v>` | a rejected push; `v` is the version that stays |
| `HOPOS_CFTUNNEL_RETRY index=<i> up=<n>` | a connection dropped or did not come up, with the reason and the wait |
| `HOPOS_CFTUNNEL_GIVEUP index=<i>` | the edge said "do not retry" (a revoked or wrong token) |
| `HOPOS_CFTUNNEL_STOP` | the kernel asked the app to stop (graceful stop, `HOPOS_APP_STOP`): every connection closes and none is reopened |
| `HOPOS_CFTUNNEL_FAIL <reason>` | the app stops: the env is unusable, or the edge refused every connection with "do not retry" |
| `HOPOS_APP_RNG source=<source>` | once, from applib: where the TLS randomness comes from (`hardware` or `jitter`, the kernel's seed mixed with its own jitter); `HOPOS_APP_RNG_NONE` if the kernel provides no seed |

## Build and test

```sh
cargo test -p cloudflared-lean
cargo clippy --release --target aarch64-unknown-none-softfloat -p cloudflared-lean -- -D warnings
cargo build --release --target aarch64-unknown-none-softfloat -p cloudflared-lean
rust-objcopy --strip-debug target/aarch64-unknown-none-softfloat/release/cloudflared-lean cloudflared-lean.elf
```

The host tests pin down the protocol layers: the registration messages and
replies byte for byte against what the Go predecessor produced, the token and
the Cap'n Proto round trip from the Go tests, the ingress rules from the Go
tests, the header bundle against Go, and the three CAs through leantls.

## On the O6N

The O6N is at `192.168.1.205`, Hop listens on `:9080`. Without the `hop` CLI
the jobspec goes to the leader with `curl` (the bench node runs with
`hopos.insecure=1`; with `hopos.apikey` the API requires an `X-Hop-Auth`
signature that curl does not set by itself).

1. Put the image on an HTTP server the node can reach, for example on the
   laptop next to the board:

   ```sh
   python3 -m http.server 8000 --bind 0.0.0.0
   ```

2. Place it (`LAPTOP` is the laptop's address, the token from the dashboard,
   `DNS` the LAN's resolver or a public one):

   ```sh
   curl -X POST -H 'Content-Type: application/json' \
     -d '{"name":"cloudflared-lean","driver":"hop",
          "artifacts":[{"url":"http://LAPTOP:8000/cloudflared-lean.elf"}],
          "memory_limit":33554432,
          "env":{"TUNNEL_TOKEN":"eyJh…","DNS":"1.1.1.1",
                 "TUNNEL_URL":"http://10.100.0.3:80"}}' \
     http://192.168.1.205:9080/v1/jobs
   ```

   No `ports`: nothing comes in, the tunnel dials out. 32 MiB is plenty (the
   image is half a megabyte; the rest is the netstack, four connections and
   their HTTP/2 windows). `TUNNEL_URL` here points to welcome in slot 2; set
   it to the service you want to expose, or let the dashboard push the
   ingress.

3. On the console: `HOPOS_CFTUNNEL_CONFIG version=0`, then four times
   `HOPOS_CFTUNNEL_UP edge=…:7844 conn=… colo=AMS`, and after a push from the
   dashboard `HOPOS_CFTUNNEL_CONFIG version=<n>` with the rules. A visit to
   the public hostname gives `HOPOS_CFTUNNEL_REQ n=1`.

4. Remove: `curl -X DELETE http://192.168.1.205:9080/v1/jobs/cloudflared-lean`.
   The app gets a graceful stop (`HOPOS_APP_STOP`, then `HOPOS_CFTUNNEL_STOP`)
   and closes every connection; the edge sees them drop and takes them out
   of rotation.

A wrong token gives `HOPOS_CFTUNNEL_GIVEUP` per connection (the edge says
`Unauthorized: Invalid tunnel secret`) and then `HOPOS_CFTUNNEL_FAIL`. If the
kernel's wall clock is not set yet (no SNTP), `HOPOS_CFTUNNEL_RETRY` says the
edge's certificate cannot be checked yet, and it retries with backoff.

## What it does not (yet) carry

Refusing deliberately with a reason is clearer than half working:

- **Websockets**: a 502 with a reason; the path exists in leanh2, but without
  a test against a real websocket origin we do not promise it (as in Go).
- **Raw TCP streams** (WARP, `cloudflared access`): 502 (as in Go).
- **https origins**: needs leanhttps plus a decision about certificate
  verification (as in Go).
- **The quic transport**, metrics, auto-update, diagnostics (as in Go).
- **The SRV record** `_v2-origintunneld._tcp.argotunnel.com`: applib's
  resolver only asks for A records, and one address per name. The two fixed
  region names are what the SRV record points to; a new attempt asks again
  and then often gets a different address.
- **A clean unregistration** (`unregisterConnection`): on a graceful stop the
  app closes each connection (`HOPOS_CFTUNNEL_STOP`), but does not send
  `unregisterConnection` first. The Go predecessor had it, but never called
  it.
- **An idle timeout on the service's body**: after the response header the
  tunnel waits as long as the service takes (as in Go); a service that goes
  quiet in the middle of a body holds that one stream.

## Origin

This code is new. What was taken over are facts, not lines: the header names,
the interface and method ids of `RegistrationServer`, the struct layouts and
the three CA certificates, all from the pinned
[cloudflared](https://github.com/cloudflare/cloudflared) (Apache 2.0) and
capnproto2's `rpc.capnp`, via the Go predecessor. The name is
`cloudflared-lean` and not `cloudflared`: Apache 2.0 grants no trademark
rights, and this is not their build.
