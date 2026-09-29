# welcome — the page that gives a node a face

One self-contained HTML page on the published port: the first thing anyone sees
when a fresh HopOS node is switched on. It ships in the default `hopos.init[]`
of the headless release (port 80), so a headless node does not feel empty but
says: this node is alive, this is HopOS, this is what runs on it.

All visible text (page and logs) is English — it is a public page. Comments in
the code are Dutch, as everywhere else.

```
GET /             the page (rendered per request, so the numbers are true)
GET /api/status   nine integers, polled by the page once a second
GET /healthz      "ok"
```

## What it shows — and what does not exist

A slot is handed `HOPOS_HOST` (the node IP), `HOP_DNS`, its own `ER_PORT_*` and
whatever the job spec passed. There is **no node name, no cluster name and no
node uptime** in that env, so the page does not show them: asking the agent API
would need HMAC with the cluster key, and a welcome page you cannot read without
a key is exactly the friction this app removes.

First-hand, and that list *is* the message: node address, its own IP, its slot,
`runtime.NumCPU()`, `app.RAMSize` (a hardware boundary, not a quota), heap from
`ReadMemStats`, `runtime.GOARCH` + `runtime.Version()`, its own uptime and its
own counters.

**Whole cores are the default, not a law — and sharing is allowed, never
imposed.** HOP stacks cages on one physical core only when a job asks for it with
a sharegroup, so nothing lands on your core behind your back. But that also means
"a whole core" is not a property to state: it is a measurement, and one that
changes while the app runs. The page reads `CtrlShared` from its own control page
(HOP is the only writer and keeps it current) and says `dedicated` or `shared`,
worded as *this moment* rather than a promise. That value sits in `/api/status`
and moves with the poll, so it flips as neighbours arrive and leave.

**And the slot is not the core.** `app.Slot` is the *cage* number: HOP patches it
into the image as `slotHint` at start, and `board/hopslot` only falls back to
MPIDR when that hint is missing (on servers MPIDR is not a slot number at all).
Which physical core the cage landed on is HOP's business — an app is not told, and
with a sharegroup several cages share one. The page said "the slot … is also the
index of the core it runs on", which was simply wrong; tests now pin both this and
the whole-core claim.

## Layout

| path | what |
|---|---|
| `cmd/welcome/main.go` | the tamago-only main: `applib.Init` → `appnet.Up` → `apphttp.ListenAndServe` |
| `cmd/welcome/main_other.go` | host stub, so `go build ./...` stays green |
| `internal/welcome/welcome.go` | `Node`, `Server`, routing, status JSON, formatting |
| `internal/welcome/page.go` | the page itself (one string, `__TOKEN__` substitution) |

`apphttp` instead of `net/http`: `net/http` links `crypto/tls` unconditionally,
which costs ~2.9 MB in an app image that speaks plain http behind the switch.
This binary carries zero `crypto/tls` symbols.

Everything is in the binary — no CDN, no webfont URL, no image from gethop.org.
A node can sit in a rack without internet and must still look like this.

## Build

`applib` is tamago-only, so the main does not compile on the host: the logic is
host-tested (`go test ./...`, including an end-to-end pass over a real
`apphttp.Serve`), and the main goes through the tamago gate that `release.sh`
runs.

```sh
# arm64 (Pi 4/5, UEFI) — canonical link address: one artifact runs in any slot
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
  ~/tamago-go/bin/go build -tags linkcpuinit -trimpath \
  -ldflags "-w -T 0x50010000 -R 0x1000" -o out/welcome-arm64-tamago.elf ./cmd/welcome

# riscv64 (LicheeRV Nano) — no second translation stage: the link address IS the
# partition, and the RAM plan comes from linkramsize. That tag goes ON TOP of
# linkcpuinit, not instead of it: without linkcpuinit tamago links its own entry
# assembly, which writes mie/mstatus — M-mode CSRs, while a slot on this board
# runs in S-MODE. That is an illegal instruction on the second instruction of the
# entry (measured on the board 2026-07-31: mcause 2, mtval 0x30429073 = csrw mie)
# and the app dies before its first log line.
GOWORK=off GOTOOLCHAIN=local GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=riscv64 \
  ~/tamago-go/bin/go build -tags "linkramsize linkcpuinit" -trimpath \
  -ldflags "-w -T 0x88010000 -R 0x1000" -o out/welcome-riscv64-tamago.elf ./cmd/welcome
```

`../../../release.sh release` builds both, publishes them with the version and
pins them to the `rolling-release` tag so job-spec URLs stay stable:

```
https://github.com/xinix00/hop/releases/download/rolling-release/welcome-arm64-tamago.elf
https://github.com/xinix00/hop/releases/download/rolling-release/welcome-riscv64-tamago.elf
```

## Run

One line for both architectures — list an artifact per arch and let the node pick
its own with `match` on `node.arch`:

```
hopos.init[]={"name":"welcome","driver":"hop","artifacts":[{"url":"…/welcome-arm64-tamago.elf","match":{"node.arch":"arm64"}},{"url":"…/welcome-riscv64-tamago.elf","match":{"node.arch":"riscv64"}}],"memory_limit":67108864,"ports":{"http":80}}
```

In QEMU (`image/uefi-run.sh agent` forwards 18080 to the published port) publish
on `"ports":{"http":18080}` and open `http://localhost:18080/`. `hop logs
welcome` shows the banner, which is the same line the page prints as example
output.

A `hop apply` cannot publish ports (there is no `--port` flag), so a job spec or
`hopos.init[]` is the way in:

```sh
hop apply --name welcome --driver hop --artifact <url> --memory 64M
```
