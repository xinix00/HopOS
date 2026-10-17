# Go apps on HopOS

An app image that speaks the slot ABI runs, whatever language it was built
in. An app whose source is Go (cloudflared) builds with tamago against the
Go SDK, the module `github.com/xinix00/HopOS/metal/v2`. Slot ABI 10 of that
SDK (`abi/layout`) is byte for byte ABI 1 of `abi/` (the 2 MB tail, the
control page offsets, the ring geometry, HVC numbers 0, 1, 4, 5 and 6, the
systemapi frame and the hopabi ops). The kernel therefore accepts the Go
stamp (`abi::place::GO_ABI_VERSION`) and translates the Go linker's section
table, which sits in the first PT_LOAD, back to the partition
(`kern::system`).

Proven 2026-10-02 on QEMU virt: a Go welcome, built with tamago, placed by
Hop, network up with the RX doorbell as interrupt, the page with the bunny
via the uplink DNAT, and the stop withdraws the port.

## What you need

| What | Where | Why |
| --- | --- | --- |
| the tamago toolchain | `~/tamago-go/bin/go` (Go 1.26.4, `GOOS=tamago`) | the compiler and the runtime |
| the three runtime patches | `go/tamago-go/*.patch`, `apply.sh` | the idle hook, the bound addresses in `net`, the `findTimer` bound; without the first an app never parks and without the third the doorbell faults on the M4 |
| the Go SDK | module `github.com/xinix00/HopOS/metal/v2` v2.2.8, from GitHub | `app/applib`, `abi`, `cpu/idle`, `board/hopslot`, `dev`: the app side of the ABI |
| the app modules | `go/<name>`, the SDK pinned in their `go.mod` | cloudflared |

The toolchain on this machine already carries the patches (`git -C ~/tamago-go
status` shows the five modified runtime files). On a fresh machine:

```sh
git clone https://github.com/usbarmory/tamago-go ~/tamago-go
git -C ~/tamago-go checkout --detach "$(cat go/tamago-go/BASE_REVISION)"
(cd ~/tamago-go/src && ./make.bash)
TAMAGO_SRC=~/tamago-go sh go/tamago-go/apply.sh
```

The compiler does not need a rebuild after the patches: it compiles the
runtime from its own source tree on every build.

## Building

One artifact runs in every slot: arm64 links at the canonical address
`SlotBase(1) + 0x10000`, just like a Rust app (`applib/link.ld`). The
`-R 0x1000` keeps the segments on pages, `-w` drops DWARF and keeps the
symbol table (the kernel reads `RamStart`, `RamSize`, the slot hint and the
stamp from it; never `-s`). The tag `linkcpuinit` picks the bare EL1 branch
of `board/hopslot`.

```sh
cd go/cloudflared
sh tools/prepare-cloudflared.sh
GOWORK=off GOTOOLCHAIN=local \
GOOS=tamago GOOSPKG=github.com/usbarmory/tamago GOARCH=arm64 \
~/tamago-go/bin/go build -trimpath -tags linkcpuinit \
  -ldflags "-w -T 0x50010000 -R 0x1000" \
  -o /tmp/cloudflared-arm64-tamago.elf ./cmd/cloudflared-hopos
```

`prepare-cloudflared.sh` puts the pinned cloudflared with two platform
fallbacks in `build/cloudflared-patched`; until then every go command in
that module fails (see `go/cloudflared/README.md`). The image is 28 MB; the
kernel streams it straight into the partition and looks up its symbols there
in chunks, not on the heap.

`go/apps-release.sh` does the same for all Go apps at once and tests that no
gvisor symbol is in them; `METAL=<tag>` builds them against another tag of
the SDK:

```sh
PUBLISH=0 sh go/apps-release.sh
```

## Running

A jobspec is the same as for a Rust app; Hop sees no difference
(`jobs/job-cloudflared.json`):

```json
{"name":"cloudflared","driver":"hop",
 "artifacts":[{"url":"https://.../cloudflared-arm64-tamago.elf","match":{"node.arch":"arm64"}}],
 "memory_limit":268435456,"env":{"TUNNEL_TOKEN":"..."}}
```

On QEMU the welcome test takes a Go welcome as artifact:

```sh
GO_ELF=/tmp/welcome-arm64-tamago.elf sh tools/qemu-test-welcome.sh
```

The console shows the Go app via its outbox, like any app:

```
slot 2: mem: Go memory limit 23MB, GOGC 25 (window 30MB, image+bss 2MB)
slot 2: appnet: RX doorbell served as interrupt
slot 2: welcome dev: slot 2, 1 core, 30.0 MB RAM, serving http://10.100.0.3
```

The kernel refuses a Go image with a stamp older than 10 with `image speaks
slot ABI <n>, this HopOS speaks 1`; rebuild it against `metal/v2` v2.2.8.
The Spin releases (`jobs/job-spin.json`) therefore need to be checked for
which metal they were built against.

## What the Go app does differently from a Rust app

- It maps its whole tail Normal write-back, the control page included
  (`memattr.NormalWB`); a Rust app sets the control page to Normal-NC
  (`applib/src/mmu.rs`). On QEMU that makes no difference; on hardware this
  is the first place to look if a Go app boots but does not log.
- It does not write the write-back word in the ring header
  (`abi::ring::WB_WORD`); the kernel then promises nothing and does
  push/pull per record.
- It reads its env up to `CTRL_ENV_LEGACY_MAX`; the kernel never writes
  more than `CTRL_ENV_MAX`, so it does not touch the top words (RNG seed,
  timebase, app fault report).
- It has its own RNG (RNDR or jitter DRBG) and its own clock; it does not
  read the kernel's seed on the control page.
- It does not know the stop doorbell: it reads the kill flag (since
  2026-10-05 with the grace period in ms in it) as "not 0" and stops
  immediately, without a network goodbye, as every app did before that date
  (apps.md, The stop). The grace period still applies on the kernel side:
  the kernel waits for the slot to go quiet before it revokes.
- SMP: the `CTRL_SMP_*` words are the Go runtime's M/g0 handoff and are still
  in ABI 1; a Go app with `cores: 2` has not been run yet.

## Still to prove

- On hardware: the tail on Normal-WB on the M4, the Radxa and the Pis; the
  doorbell as vFIQ on the M4 (HVC 5).
- riscv64: a Go app links on the physical partition there (`-T 0x88010000`,
  tag `linkramsize`); the cage places apps in the canonical window.
  `go/apps-release.sh` therefore builds arm64 only.
- cloudflared itself (28 MB) and Spin.

## The price

This costs the kernel two small pieces of code. The price is outside it:
ABI 1 stays equal to Go ABI 10. If anything in the tail moves,
`GO_ABI_VERSION` goes away or the Go SDK gets a new tag with the new layout.
