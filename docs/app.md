# Write & compile a Go app

HopOS runs Go programs compiled with [tamago](https://github.com/usbarmory/tamago-go)
— a Go toolchain that needs no OS underneath. Your app gets whole physical
cores, its own memory partition and **its own network stack**; `applib`
handles the node handshake (READY, heartbeats, the kill flag) so `main`
only does your work.

## 1. Install the toolchain (once)

```sh
git clone https://github.com/usbarmory/tamago-go ~/tamago-go
(cd ~/tamago-go/src && ./make.bash)
tools/tamago-go/apply.sh          # onze patches op de toolchain (tools/tamago-go/README.md)
```

## 2. A realistic app — with networking

You almost always want the network, so the starting point includes it. The
repo carries this as [`metal/app/hello`](../metal/app/hello/main.go):

```go
package main

import (
    "fmt"
    "net/http"

    "hop-os/metal/app/applib"
    "hop-os/metal/app/applib/appnet"
)

func main() {
    app := applib.Init()          // READY + heartbeat + kill, all automatic

    ip, err := appnet.Up(app)     // the app's own TCP/IP stack, own IP
    if err != nil {
        app.Logf("net: %v", err)
        app.Exit(1)
    }

    port := app.Env("ER_PORT_HTTP") // published port from the job spec
    if port == "" {
        port = "8080"
    }
    app.Logf("hello: serving on %s:%s", ip, port)

    http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        fmt.Fprintf(w, "hello from HopOS — slot %d\n", app.Slot)
    })
    app.Logf("http: %v", http.ListenAndServe(":"+port, nil))
    app.Exit(1) // a service that stops serving is a crash, by design
}
```

After `appnet.Up` the **full Go networking suite just works** on the app's
own stack: `net`, `net/http`, TLS, websockets, gRPC — `Listen` and `Dial`
like anywhere else. Ports you declare in the job spec are published on the
node's IP and handed to the app as `ER_PORT_<NAME>` — and that address is
true from the *inside* too: a neighbouring app dialing the node IP is
hairpinned through the switch without the frame ever leaving the machine
(see [networking](technical/networking.md)).

**No https? Save ~2.9 MB.** `net/http` links `crypto/tls` unconditionally —
in an app image that costs more than the whole netstack (measured 26-07 on
this `hello`: 4.70 MB with `appnet`, 7.99 MB once `net/http` is in, of which
~54% is TLS/PKI). [`leanhttp`](https://github.com/xinix00/lean) is plain HTTP/1.1 without it:
`Get`/`Do` as a client, `Serve` as a server, chunked and WebSocket-upgrade
included, `+0.36 MB` over the netstack floor. The SURF display, launcher and
taskman run on it and each lost ~2.9 MB; an app that needs https — a client
dialing an S3 endpoint, the SURF browser — stays on `net/http` with its x509
root bundle. See the package doc for the full trade-off.

Today an app lives inside this repo's module (`metal/app/<name>`) so it can
import `applib` — copy `hello` as your starting point.

## 3. Build it

One command per architecture, one canonical link address each (the node
relocates it per slot, so the same ELF runs in any slot on any board of that
architecture):

```sh
cd HopOS/metal
# arm64 — UEFI boxes, Raspberry Pi, QEMU
GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
  ~/tamago-go/bin/go build -tags linkcpuinit -trimpath \
  -ldflags "-w -T 0x50010000 -R 0x1000" -o hello.elf ./app/hello

# riscv64 — LicheeRV Nano
GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=riscv64 \
  ~/tamago-go/bin/go build -tags "linkramsize linkcpuinit" -trimpath \
  -ldflags "-w -T 0x88010000 -R 0x1000" -o hello-riscv64.elf ./app/hello
```

Same source, two artifacts — list both in one job with a `match` on
`node.arch` and a mixed fleet needs no second job spec (see
[Configure](config.md)).

## 4. Run it as a job

Serve the ELF over HTTP (the node streams it from there straight into the
slot's partition), then submit through [HOP](https://gethop.org/hop/docs/):

```sh
python3 -m http.server 8000 &
run apply --name hello --driver hop \
    --artifact http://<your-ip>:8000/hello.elf --memory 96M
run logs hello
```

**Prebuilt apps.** You don't have to build one to try HopOS — ready-made apps
(the SURF display, a browser, a clock, a dashboard) are published on the
[hop-os-surf releases page](https://github.com/xinix00/hop-os-surf/releases);
point a job's `artifacts` URL straight at one.

## 5. Porting an existing Go service

Most plain-Go services port in minutes — it's a checklist, not a rewrite:

| in your service today | on HopOS |
|---|---|
| `func main()` starts working right away | first `app := applib.Init()`, then `appnet.Up(app)` |
| `os.Getenv("PORT")` / flags | `app.Env("ER_PORT_<NAME>")` and job-spec `env` |
| `log.Printf` / stdout | `app.Logf` — lands in `run logs`, multiplexed per slot |
| reads/writes local files | private root + shared `/data` mounts: `app.ReadFile` / `app.WriteFile` / `app.Fetch` — after `appnet.Up`, see below |
| `http.ListenAndServe`, `net.Dial`, TLS, … | unchanged — full Go net suite on your own stack |
| `os/exec`, cgo, C dependencies | won't port — there is no OS to exec into; keep it pure Go |
| graceful shutdown on SIGTERM | not needed: the kill flag parks the core; just don't exit `main` |

**System calls.** Files, mounts, `Fetch` and logs go over your own stack to
the node's system service at `10.100.0.1:10100` (slot ABI 6): one persistent
connection per app, calls up to 1 MiB each, so a large file streams instead of
crawling. That is why the file layer needs `appnet.Up` first — before it, and
for crash logs, the mailbox in your partition still carries `Logf`. Both sides
run on the same ring transport as your traffic, woken by the ring itself rather
than polled. The node refuses an image built against another slot ABI
(`image speaks slot ABI 5, this HopOS speaks 6 — rebuild it`): rebuild your
apps whenever the kernel's ABI moves.

## What your app gets

- **Whole cores** — `--cpu 2048` gives it two, SMP with a shared heap; Go
  just sees `GOMAXPROCS`.
- **Isolation by silicon** — its own memory cage; see
  [Isolation](technical/isolation.md).
- **Telemetry for free** — cpu%, memory and heartbeat show up in HOP
  without agents or exporters.

## Packing apps together — sharegroups

By default each app owns whole cores. To run more apps than you have cores —
a dozen small services on a 4-core Pi — tag apps you **trust** with a
sharegroup: they cooperatively share a pool of whole cores, sized with
`--cpu`. The node spreads them across the pool and switches on idle (no
timer, no preemption).

```sh
run apply --name web    --driver hop --artifact … --tag sharegroup=site --cpu 2048
run apply --name worker --driver hop --artifact … --tag sharegroup=site --cpu 2048
```

Both land in the `site` pool of 2 whole cores (`--cpu 2048` = 2 cores; the
first job of a group sets the size). Each app keeps its **own memory cage
and network stack** — only the cores are shared, and only among apps you
named. The trade is timing isolation *inside* the group (see
[Isolation](technical/isolation.md)); apps without a sharegroup keep a whole
core to themselves. Safe by default, share when trusted.

## What it doesn't get

No syscalls, no containers, no cgo, no other languages. Exit means the job
is done — a service that returns from `main` is treated as crashed by
design.
