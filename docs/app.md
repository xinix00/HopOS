# Write & compile a Go app

HopOS runs Go programs compiled with [tamago](https://github.com/usbarmory/tamago-go)
— a Go toolchain that needs no OS underneath. Your app gets whole physical
cores, its own memory partition and its own network stack; `applib` handles
the node handshake (READY, heartbeats, the kill flag) so `main` only does
your work.

## 1. Install the toolchain (once)

```sh
git clone https://github.com/usbarmory/tamago-go ~/tamago-go
(cd ~/tamago-go/src && ./make.bash)
```

## 2. The smallest app

The repo carries it as [`metal/app/hello`](../metal/app/hello/main.go):

```go
package main

import (
    "time"

    "hop-os/metal/app/applib"
)

func main() {
    app := applib.Init() // READY + heartbeat + kill handling, all automatic
    app.Logf("hello from slot %d — %d MB RAM", app.Slot, app.RAMSize>>20)
    for i := 1; ; i++ {
        app.Logf("alive: round %d", i)
        time.Sleep(10 * time.Second)
    }
}
```

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
then submit through [HOP](https://github.com/xinix00/hop):

```sh
python3 -m http.server 8000 &
hop apply --name hello --driver hop \
    --artifact http://<your-ip>:8000/hello.elf --memory 96M
hop logs hello
```

## What your app gets

- **Whole cores** — `--cpu 2048` gives it two, SMP with a shared heap; Go
  just sees `GOMAXPROCS`.
- **Env** — job env plus `ER_PORT_<NAME>` for published ports and `ER_ATTR_*`
  node attributes, via `app.Env()`.
- **Network** — its own stack: `appnet.Up(app)` then plain `net.Listen`/`Dial`.
- **Files** — a private root plus the job's shared `/data` mounts:
  `app.ReadFile`/`app.WriteFile`/`app.Fetch`.
- **Telemetry for free** — cpu%, memory and heartbeat show up in `hop`
  without agents or exporters.

## What it doesn't get

No syscalls, no containers, no cgo, no other languages. Exit means the job
is done — a service that returns from `main` is treated as crashed by
design.
