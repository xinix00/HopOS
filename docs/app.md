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
node's IP and handed to the app as `ER_PORT_<NAME>`.

Today an app lives inside this repo's module (`metal/app/<name>`) so it can
import `applib` — copy `hello` as your starting point.

## 3. Build it

One command, one canonical link address (the node relocates it per slot):

```sh
cd HopOS/metal
GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
  ~/tamago-go/bin/go build -tags linkcpuinit -trimpath \
  -ldflags "-w -T 0x50010000 -R 0x1000" -o hello.elf ./app/hello
```

## 4. Run it as a job

Serve the ELF over HTTP (the app downloads its own image, on its own core),
then submit through [HOP](https://gethop.org/hop/docs/):

```sh
python3 -m http.server 8000 &
hop apply --name hello --driver hop \
    --artifact http://<your-ip>:8000/hello.elf --memory 96M
hop logs hello
```

## 5. Porting an existing Go service

Most plain-Go services port in minutes — it's a checklist, not a rewrite:

| in your service today | on HopOS |
|---|---|
| `func main()` starts working right away | first `app := applib.Init()`, then `appnet.Up(app)` |
| `os.Getenv("PORT")` / flags | `app.Env("ER_PORT_<NAME>")` and job-spec `env` |
| `log.Printf` / stdout | `app.Logf` — lands in `hop logs`, multiplexed per slot |
| reads/writes local files | private root + shared `/data` mounts: `app.ReadFile` / `app.WriteFile` / `app.Fetch` |
| `http.ListenAndServe`, `net.Dial`, TLS, … | unchanged — full Go net suite on your own stack |
| `os/exec`, cgo, C dependencies | won't port — there is no OS to exec into; keep it pure Go |
| graceful shutdown on SIGTERM | not needed: the kill flag parks the core; just don't exit `main` |

## What your app gets

- **Whole cores** — `--cpu 2048` gives it two, SMP with a shared heap; Go
  just sees `GOMAXPROCS`.
- **Isolation by silicon** — its own memory cage; see
  [Isolation](technical/isolation.md).
- **Telemetry for free** — cpu%, memory and heartbeat show up in `hop`
  without agents or exporters.

## What it doesn't get

No syscalls, no containers, no cgo, no other languages. Exit means the job
is done — a service that returns from `main` is treated as crashed by
design.
