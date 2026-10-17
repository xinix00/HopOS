# welcome

The page that gives a node a face, and the first Rust app of HopOS v3. One
HTTP server (leanhttp over `applib::appnet`) on the port from
`ER_PORT_HTTP` (default 80):

```
GET /         the page, built per request (so the numbers are right)
GET /health   200, "ok"
```

The page shows the bunny in text, the node's name (`ER_ATTR_NODE_ID` from
Hop), the slot, the uptime, the number of requests, the partition and the
heap (from applib's counters), and the `Host` the browser used to find it.
Everything is in the binary: no CDN, no web font, no script. A node without
internet looks exactly the same.

Markers on the kernel console: `HOPOS_WELCOME_UP port=<n>` when the listener
is up, and every hundred requests `HOPOS_WELCOME_REQUESTS`. An accept error
is `HOPOS_WELCOME_ACCEPT`.

The acceptor runs until the kernel asks the app to stop (graceful stop,
`HOPOS_APP_STOP`); applib's main shell then closes every connection and exits
(`HOPOS_APP_SHUTDOWN`).

## Build

```sh
cargo build --release --target aarch64-unknown-none-softfloat -p welcome
rust-objcopy --strip-debug target/aarch64-unknown-none-softfloat/release/welcome welcome.elf
```

Without debug info (the symbols stay: placement reads them). One ELF for
every ARM64 board: linked canonically, the kernel puts it on the partition of
any slot.

## On a board

There is no `hop` CLI yet: the jobspec goes to Hop's leader with `curl`
(port 9080 on the node's address). The bench node runs with
`hopos.insecure=1` in `hopos.cfg`; with `hopos.apikey` the API requires an
`X-Hop-Auth` signature that curl does not set by itself.

1. Put `welcome.elf` on an HTTP server the node can reach, for example on the
   laptop next to the board:

   ```sh
   python3 -m http.server 8000 --bind 0.0.0.0
   ```

2. Place it (`NODE` is the address from `HOPOS_NET_UP`, `LAPTOP` the
   laptop's):

   ```sh
   curl -X POST -H 'Content-Type: application/json' \
     -d '{"name":"welcome","driver":"hop",
          "artifacts":[{"url":"http://LAPTOP:8000/welcome.elf"}],
          "memory_limit":33554432,"ports":{"http":80}}' \
     http://NODE:9080/v1/jobs
   ```

   On the console: `HOP_JOB_PLACED slot=2`, then `slot 2: 1 port(s) published
   tcp+udp on the uplink: :80 HOPOS_SLOT_PUBLISH` (the kernel forwards the
   node's port 80 to the slot), and `slot 2: welcome: serving http on
   10.100.0.3:80 ... HOPOS_WELCOME_UP port=80`.

3. Open `http://NODE/` in the browser: a dark page with the copper bunny,
   "You have reached a HopOS node.", four tiles (node, slot, uptime,
   requests) and a table with what the app knows about itself.
   `http://NODE/health` gives `ok`.

4. Remove: `curl -X DELETE http://NODE:9080/v1/jobs/welcome`. The kernel
   withdraws the port (`HOPOS_SLOT_UNPUBLISH`), the app hears the stop
   doorbell (`HOPOS_APP_STOP grace_ms=2500`: Hop's 3 s grace period minus
   applib's 500 ms margin), returns from main and closes its connections
   (`HOPOS_APP_SHUTDOWN`), and the kernel sees it quiet without a revoke
   (`HOPOS_SLOT_STOPPED`). After that `http://NODE/` no longer answers.

Permanent, so that a fresh node shows something right away: the same JSON on
one line after `hopos.init[]=` in `hopos.cfg`.

Port 80 is free because Hop itself has 8080 and 9080; a jobspec that asks for
one of those two fails loudly (`port 8080 is taken by slot 1`,
`HOPOS_SLOT_PUBLISH_FAIL`).

## On QEMU

```sh
sh tools/qemu-test-welcome.sh                      # the whole ring, green or red
WEBPORT=8081 ARTIFACT=welcome sh image/qemu-run.sh # by hand
```

The ring also checks the graceful stop: after `DELETE` it requires
`HOPOS_SLOT_UNPUBLISH`, `HOPOS_APP_STOP`, `HOPOS_APP_SHUTDOWN` and
`HOPOS_SLOT_STOPPED`; a `HOPOS_APP_STOP_FORCED` or `HOPOS_PART_QUARANTINE`
is red.

The second command prints the artifact URL and the `curl` command; the page
is then at `http://127.0.0.1:8081/`.

## Layout

- `src/main.rs`: the start, the acceptor and a fixed pool of four workers
  (handbook §2: no task per connection, the pool is `sync::Doors`), the
  routes via `leanhttp::Mux`, each connection as an `applib::tcp::TcpConn`
  with its timeouts on the timer wheel.
- `src/page.rs`: the page, pure and tested on the host; writes into a
  pre-reserved buffer and never grows.
