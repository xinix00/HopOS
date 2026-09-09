# Applications

A HopOS application is a Go program with its own runtime and memory partition. A job selects its ELF artifact, memory allocation, core placement, ports, and storage access. Applications may request dedicated execution or share cores with an explicitly named group. See [lifecycle](lifecycle.md) for ownership and release rules.

## Minimal network service

Use `github.com/xinix00/HopOS/metal/v2`; the application modules in this tree pin `v2.2.2` and declare Go `1.26.4`. The build tool is the patched TamaGo compiler described in [development](development.md).

```go
package main

import (
    "fmt"
    "net/http"

    "github.com/xinix00/HopOS/metal/v2/app/applib"
    "github.com/xinix00/HopOS/metal/v2/app/applib/appnet"
)

func main() {
    app := applib.Init()
    if _, err := appnet.Up(app); err != nil {
        app.Logf("network: %v", err)
        app.Exit(1)
    }
    port := app.Env("ER_PORT_HTTP")
    if port == "" {
        app.Logf("the job must publish an http port")
        app.Exit(1)
    }
    handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        fmt.Fprintln(w, "Hello from HopOS")
    })
    app.Logf("server stopped: %v", http.ListenAndServe(":"+port, handler))
    app.Exit(1)
}
```

Call `applib.Init()` first in `main`. It connects the application to its ABI, reads its environment, and establishes lifecycle handling. Call `appnet.Up(app)` before using Go network listeners or connections; it installs the application's network stack in Go's `net` package. Compute-only applications do not need `appnet.Up`.

Use `app.Logf` for application logs and `app.Exit(code)` to report termination. `applib` imports the generic `hopslot` board. Applications do not import a Pi, Apple, NIC, or storage driver. The [networking page](networking.md) describes the application stack and system-service path.

## Build both architectures

These commands build the existing welcome application. Run them from the repository root, after preparing the toolchain. Substitute your module directory and command package when building your own application.

```sh
cd apps/welcome
GOWORK=off GOTOOLCHAIN=local GOOS=tamago \
  GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
  "$HOME/tamago-go/bin/go" build -tags linkcpuinit -trimpath \
  -ldflags '-w -T 0x50010000 -R 0x1000' \
  -o welcome-arm64-tamago.elf ./cmd/welcome

GOWORK=off GOTOOLCHAIN=local GOOS=tamago \
  GOOSPKG=github.com/usbarmory/tamago GOARCH=riscv64 \
  "$HOME/tamago-go/bin/go" build -tags 'linkramsize linkcpuinit' -trimpath \
  -ldflags '-w -T 0x88010000 -R 0x1000' \
  -o welcome-riscv64-tamago.elf ./cmd/welcome
```

Keep ELF symbols: use `-w`, without `-s`. The loader uses symbols for application ABI handling and placement. An ARM64 artifact can run on the supported ARM64 boards without board-specific application build tags; its ABI must still be compatible with the node. A RISC-V node requires the RISC-V artifact.

The commands use the application's pinned module dependencies. To build the complete application set against the current metal tree, use the application release script with publication disabled; its prerequisites and side effects are listed in [development](development.md).

## Job example

Host the artifacts on a server reachable by the node. The URLs below are placeholders.

```json
{
  "name": "my-service",
  "driver": "hop",
  "artifacts": [
    {"url": "https://artifacts.example/my-service-arm64.elf",
     "match": {"node.arch": "arm64"}},
    {"url": "https://artifacts.example/my-service-riscv64.elf",
     "match": {"node.arch": "riscv64"}}
  ],
  "memory_limit": 67108864,
  "cpu_shares": 1024,
  "ports": {"http": 18080},
  "env": {"MODE": "serve"}
}
```

The agent selects the first matching artifact. Put an artifact without `match` last if it is intended as a fallback. The Hop runner accepts one selected, unpacked ELF image; container images and archive extraction are not supported by this driver.

Submit the manifest through `POST /v1/jobs`; [operations](operations.md) covers endpoint selection and authentication. To seed it at boot, put the same JSON on one line after `hopos.init[]=`. Field types, units, defaults, and rounding rules are in [configuration](configuration.md).

## Placement examples

| Requested execution | Manifest fields |
| --- | --- |
| One dedicated core | `"cpu_shares":1024`, no `sharegroup` tag |
| One SMP application on two dedicated cores | `"cpu_shares":2048`, no `sharegroup` tag |
| One core shared by trusted applications | `"cpu_shares":1024,"tags":{"sharegroup":"trusted"}` |
| A pool of two cores shared by trusted applications | `"cpu_shares":2048,"tags":{"sharegroup":"trusted"}` |
| Restrict that pool to big cores | Add `"core-class":"big"` inside the same `tags` object |

Every application reserves one cage, including SMP applications. Cage IDs are allocated from the first free ID and are reusable after a completed stop; sharing does not reserve the low IDs for dedicated applications.

Without `sharegroup`, `cpu_shares` selects the application's dedicated core count. An SMP application has one application address space and a runtime using the assigned cores. Placement requires an available contiguous core run.

With `sharegroup`, `cpu_shares` selects the **pool size**. Each member itself runs on one core and keeps its own memory partition. Two members each requesting 2048 shares use one pool of two cores. They do not reserve four cores. Members must agree on pool size. Use sharing only for applications you intend to place together; separate jobs do not share cores implicitly.

`core-class` constrains physical cores. A member joining an existing pool cannot move its residents: every pool core must satisfy the requested class. Available classes depend on the board. An incompatible pool size or class is rejected; a lack of available capacity leaves the task waiting. There is no implemented `sharecores` setting: pool size comes from `cpu_shares`.

## Environment and storage

`app.Env("ER_PORT_HTTP")` reads the allocated port for the `http` publication, including dynamic allocation. `HOPOS_HOST` supplies the node address; `HOP_DNS` supplies the resolver used by `appnet.Up`. Set your own configuration under `env`, with string values.

Use application filesystem APIs such as `app.ReadFile` and `app.WriteFile` for paths available to the job. A volume mapping such as `{"/dataset":"/data"}` exposes a configured shared path at an application path. It does not expose an arbitrary file from the machine used to build HopOS. Storage setup and durability are described in [operations](operations.md).

Memory allocation, the largest free partition, core placement, and available cage IDs all affect admission. The compile-time cage cap of 128 is not a promise that every board can run 128 applications. Consult [boards and drivers](boards-drivers.md) and [status](status.md) for resource and test boundaries.

Source: [applib](../metal/app/applib/applib.go), [appnet](../metal/app/applib/appnet/up.go), [welcome](../apps/welcome/cmd/welcome/main.go), [application builder](../tools/apps-release.sh), [pool placement](../metal/kern/slots/pool.go).
