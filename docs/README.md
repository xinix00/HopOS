# HopOS v3: the docs

What exists, how to build it, and what there is to prove per board. The
checklists per board are the bar for a day on hardware; each step names the
console line that belongs to it and what a deviation means.

The IPv6 slot path for Matter/Thread is in [ipv6.md](ipv6.md), including
the SDK API, the source copy and the remaining hardware proof.

## The QEMU tests (as of 2026-10-17)

The single list of all tests; the header of each script says which markers
it requires and which knobs it has. `sh image/qemu-run.sh` boots the kernel
on virt without a test.

`sh tools/qemu-all.sh` runs every ring with a name in the Ring column below
(31) one after another, each green or ROOD (red) with its last line, and one
final line; `sh tools/qemu-all.sh hop flip-cold` runs only those.
`LOGS=dir` keeps the consoles there (default: a new temporary directory,
which is kept). Every ring picks free host ports, so two runners do not
clash; two runners in the same worktree do share `target/`, so give each its
own worktree.

The rings that test a cold restart on the same disk (hop, hop-smp2,
hop-oscore1, riscv-hop, sync) boot with `hopos.storage=stateful`; the
default is stateless (`HOPOS_FS_STATELESS`, [boards.md](boards.md)).

| Ring | Test | What it proves |
| --- | --- | --- |
| `virt` | `sh tools/qemu-test.sh` | boot on virt, network, storage, appspike in slot 1 and 2 in a stage-2 cage, all its tests green (`HOPOS_APPSPIKE_DONE pass=9 fail=0`); the config in the window of the kernel image (`HOPOS_CFG_WINDOW`); the kernel's seed on the control page (`HOPOS_RNG_SLOTS source=jitter`, and in the app `HOPOS_APP_RNG source=jitter`: virt has no TRNG, [boards.md](boards.md)) |
| `virt-gui` | `GUI=1 sh tools/qemu-test.sh` | the same with the console on a `ramfb`: the bunny, mem, date and time on the screen, captured via the QEMU monitor (`SHOT_KEEP=file.ppm` keeps the screendump) |
| `virt-display` | `GUI=display sh tools/qemu-test.sh` | the whole gui chain: qemu-xhci with keyboard and mouse (`HOPOS_USB_UP`), the display app gets the screen in its cage (`HOPOS_FB_GRANT`, `HOPOS_DISPLAY_UP`), connects to the input listener (`HOPOS_DISPLAY_CONN`), a `sendkey` and a `mouse_move` arrive (`HOPOS_DISPLAY_INPUT keys=1 moves=1`), the screendump shows the app, and after the stop the console comes back (`HOPOS_FB_RELEASE`) |
| `fault` | `sh tools/qemu-test-fault.sh` | the applib vector table at EL1: appspike with `FAULT=1` reads just below its RAM, and the kernel prints the real translation fault (`HOPOS_APP_MMU`, then `HOPOS_SLOT_FAULT` with vector 4 and `esr=0x96000006`); the alignment trap itself is proven only by the Pi 5 |
| `hop` | `sh tools/qemu-test-hop.sh` | the kernel starts Hop with the token on the OS core; a `POST /v1/jobs` from the host makes Hop place appspike; a second boot on the same disk restores the hopfs tree (`HOPOS_FS_UP fresh=0`) and Hop starts clean (no `agent-state.json` on hopfs since Hop 3.0.2) |
| `hop-smp2` | `SMP=2 sh tools/qemu-test-hop.sh` | the same with two cores: kernel plus Hop on one core, the app on the other |
| `hop-oscore1` | `SMP=2 OSCORE=1 sh tools/qemu-test-hop.sh` | the kernel moves to core 1 at boot |
| `welcome` | `sh tools/qemu-test-welcome.sh` | Hop places welcome with `"ports":{"http":80}`; the kernel forwards port 80 to the slot, `curl` from the host gets the page with the bunny and `/health`; on `DELETE /v1/jobs/welcome` the port is withdrawn (`HOPOS_SLOT_UNPUBLISH`), the app hears the stop doorbell (`HOPOS_APP_STOP grace_ms=2500`), returns from main itself (`HOPOS_APP_SHUTDOWN`) and the kernel sees it stop without a revoke (`HOPOS_SLOT_STOPPED`); `HOPOS_APP_STOP_FORCED` or `HOPOS_PART_QUARANTINE` is red |
| | `GO_ELF=path sh tools/qemu-test-welcome.sh` | the same with a Go welcome (tamago) as artifact: a Go image with slot ABI 10 runs unchanged on ABI 1 ([go-apps.md](go-apps.md)); a Go welcome has no stop doorbell, so only the port withdrawal and `HOPOS_SLOT_STOPPED` |
| `smp` | `sh tools/qemu-test-smp.sh` | an SMP app: appspike with `cores: 2` gets its second core in its own cage, counts on it, and keeps both cores across a restart by Hop |
| `share` | `sh tools/qemu-test-share.sh` | a share group: two appspikes share one app core in cooperative rotation, both green, and a member that stops comes back next to the living member |
| `reclaim` | `sh tools/qemu-test-reclaim.sh` | core reclaim: a resident that never yields (bench, BURN=1) holds a shared app core; the new member of its group waits 2 s, the kernel sacrifices the holder (`HOPOS_CORE_RECLAIM`) and the new member comes up |
| `bench` | `sh tools/qemu-test-bench.sh` | the measurement chain: Hop places `apps/bench` with port 80, `tools/netmeter` measures from the host (rtt, storm, in, out), two apps measure each other through the switch, BURN runs, multicast in the node (one bench joins 224.0.0.251, a second sends, the switch floods, `HOPOS_BENCH_MCAST recv=3`), `hopos.idlestat=1` prints the idle counters (`HOPOS_IDLESTAT`), and a second boot does `hopos.nvmebench=1` (`NVME=0` skips it); the numbers are in [measurements.md](measurements.md) |
| | `sh tools/qemu-soak-hop.sh 30` | the soak of the hoplb circuit (Hop, welcome, hoplb with the hairpin through the switch, plus a poller on Hop): N runs in a row, a watchdog on `HOPOS_TICK` that, when a tick is more than 3 s late (`STALL`), dumps `info registers -a` and the OS core's counters from the monitor; also with `SMP=2`, `SMP=8` and `OSCORE=1` |
| `slowdisk` | `sh tools/qemu-test-slowdisk.sh` | a slow disk (`DISKLAT`, default 1.5 s per request) does not stall the OS core: during a hopfs commit welcome answers within 100 ms and the tick skips nothing |
| `volumes` | `sh tools/qemu-test-volumes.sh` | the volumes of a jobspec: a job with `"volumes":{"/volumes/demo":"/data"}` (shared path to task path) writes, hopfs commits, Hop restarts it, and it finds its file again (`HOPOS_SLOT_MOUNTS`) |
| `store` | `sh tools/qemu-test-store.sh` | the store ops: appspike does push, list, pull and drop on its directory `apps/<cluster>/<job>/` in a fake S3 on the host (`tools/fakes3.py`), from app via kernel to Hop and back (`HOPOS_APPSPIKE_STORE ok`) |
| `sync` | `python3 tools/qemu-test-sync.py` | the confirmed storage barrier (`OP_SYNC`, [storage-sync.md](storage-sync.md)): Hop places `apps/syncprobe` with its own volume, which writes and syncs a journal and 8192 database bytes, removes the journal with a barrier on the directory, and QEMU gets SIGKILL; the cold boot of the same volume reads the bytes back and misses the journal (`HOPOS_SYNC_WRITE`, `HOPOS_SYNC_READ`) |
| | `sh tools/qemu-test-cluster.sh` (in the hop repo) | a cluster of a QEMU node and a host agent with a hoplockserver as lock: both nodes in `hop agents`, and after hard-killing the host leader the HopOS node wins the lease and places a job on the host |
| `flip` | `sh tools/qemu-test-flip.sh` | the kernel flip: hopfs frozen and committed, the NAT flows captured, the jump, Hop survives without a restart, and an outgoing TCP connection from an app (role FLIPCONN) carries on: three answers via kernel A, three via kernel B, over one connection; then a reset keeps the black box (`HOPOS_FLIP_BLACKBOX`), and a stale handoff pair in DRAM is ignored at every boot (`HOPOS_FLIP_STALE`) |
| `flip-mismatch` | `MISMATCH=1 sh tools/qemu-test-flip.sh` | a bundle with different switch code is refused before the jump (Hop returns 502) |
| `flip-cold` | `COLD=1 sh tools/qemu-test-flip.sh` | the cold flip (`"cold":true` on `POST /flip`): Hop stops its tasks, the kernel turns the app cores off and jumps without adoption, Hop starts cold and places the job again |
| `flip-oscore` | `OSCORE=1 sh tools/qemu-test-flip.sh` | the flip from a moved kernel (also with `COLD=1`, and with `BOARD=uefi OSCORE=2`) |
| `flip-rpi4` | `BOARD=rpi4 sh tools/qemu-test-flip.sh` | the Pi's flip entry on raspi4b: a core with the marker reaches the kernel and reads the DTB again |
| `uefi` | `sh tools/qemu-uefi-test.sh` | the EFI stub on EDK2, ACPI, PCIe, virtio over PCI with MSI-X via the ITS (`nic=` increases in the tick), the whole appspike chain |
| `uefi-gui` | `GUI=1 sh tools/qemu-uefi-test.sh` | the same with EDK2's GOP as console |
| `nvme` | `sh tools/qemu-test-nvme.sh` | the NVMe kernel of the O6N and the Altra: the Altra image on EDK2 with `-device nvme` and igb, appspike in slot 1 and 2 writes, reads back and reads a bundle (`OP_READ_MANY`) through driver/nvme, with the queue on MSI-X via the ITS (`HOPOS_NVME_UP`, `HOPOS_NVME_IRQ`, `HOPOS_APPSPIKE_FS ok`) |
| `uefi-vhe` | `FEATURES=vhe CPU=neoverse-n1 sh tools/qemu-uefi-test.sh` | the VHE switcher (E2H=1), the flavor the O6N requires, on a VHE model |
| `flip-uefi` | `BOARD=uefi sh tools/qemu-test-flip.sh` | the kernel flip on EDK2: kernel B lands on the PIE base of kernel A (also `COLD=1`, and under VHE with `FEATURES=vhe CPU=neoverse-n1`) |
| `rpi4` | `sh tools/qemu-rpi4-test.sh` | the Pi 4 board on QEMU's raspi4b up to the executor tick (`DTB=path`, `GUI=1`) |
| `riscv` | `sh tools/qemu-riscv-test.sh` | QEMU virt riscv64 in machine mode: boot, net, storage, the PMP cage self-test (with the kill tick), and appspike twice through the kernel's lifecycle in a PMP plus Sv39 cage (`APP=` only the boot) |
| `riscv-hop` | `sh tools/qemu-riscv-test-hop.sh` | Hop as a resident on the kernel's hart on riscv64, a `POST /v1/jobs` places appspike, and Hop comes back after a restart |
| `riscv-share` | `sh tools/qemu-riscv-test-share.sh` | two residents on the one app hart (the shape of the LicheeRV), one never yields (bench, BURN=1): the switcher's time slice keeps welcome reachable, twenty GETs each within `MAX_MS` (default 1000) |
| `riscv-flip` | `sh tools/qemu-riscv-test-flip.sh` | the cold kernel flip on riscv64 with `hop flip --cold`: a warm flip is refused, the cold one lands (`HOPOS_FLIP_BOOT gen=2`), hart 1 comes back from the off stub, Hop starts cold and places the init job again |

Common knobs, per the script headers: `TIMEOUT=` (seconds), `KEEP_LOG=path`
(keeps a green console too), `HOP_DIR=path` (the hop repo, default
`../hop/hop`), and `SYSPORT`, `AGENTPORT`, `LEADERPORT`, `ARTPORT`,
`WEBPORT` for fixed host ports (default: free ones from the OS).

`sh tools/gate.sh` is the gate before every commit: host tests, clippy with
`-D warnings`, rustfmt, netmeter and loc, the target builds of the apps and
of all boards (also with `gui` and `media`, and the riscv64 boards).

`cargo run -p loc` counts the lines as the compiler links them: per board
the release build of the image, the source files from the dep info, and
then the fixed buckets (portable per layer, per ISA, per board, lean, gui)
with the tests under `#[cfg(test)]` separate; `-v` lists the files per
bucket.

`python3 tools/meet.py` runs the measurement round of
[measurements.md](measurements.md) on hardware: vitals on all boards at
once, bench in the node, the wire serially against the O6N (the O6N itself
against the M4), and cleaned up afterwards. It waits for the markers on the
console instead of sleeping, writes a fragment per board in `target/meet/`,
and with `--write` puts the cells into the *Nu* column of measurements.md
(and into *Hoogste v3* where Nu is better); `--no-run` writes from the
fragments, `--selftest` tests the writing. Seven boards take just over
three minutes.

`tools/soak.sh` places and stops jobs on a node for hours, optionally with
kernel flips (`FLIP=1`), and ends with a report ([measurements.md](measurements.md)).

## The planes

| Plane | Doc | What is in it |
| --- | --- | --- |
| Boards | [boards.md](boards.md), [boards-pi.md](boards-pi.md), [boards-radxa.md](boards-radxa.md), [boards-riscv.md](boards-riscv.md), [boards-apple.md](boards-apple.md) | per board the build, the checklist with the expected lines, the interrupts, the watchdog, the clock and the thermals; in boards.md also the config of every node and the config window |
| Kernel flip | [flip.md](flip.md) | the procedure per board, the markers of kernel A and kernel B, the failure modes, the boot guard, the cold path |
| Gui | [gui.md](gui.md) | the console on the screen, the framebuffer grant to a display app, the USB input, the Radxa's display chain |
| Media | [media.md](media.md) | the O6N's video codec (Linlon V8), the codec service, the optical drive, the checklist and the measurement (24 fps 4K P010) |
| Apps | [apps.md](apps.md) | how an app sleeps on events and not on the clock, the building blocks (`readable()`, `select`, `after` as a deadline), the graceful stop (`app.stop()`, `app.stopped()`), the load gauge `HOPOS_SLOT_LOAD`, the Go equivalent, the lessons from the Stulp port (heartbeat, budget, measuring per task and per hop, no state copies, allocator, one owner per peer, not waiting on ACKs, failure paths, queues, timers) and the review checklist |
| Go apps | [go-apps.md](go-apps.md) | Go images (tamago) on the slot ABI: the Go SDK, building, cloudflared in `go/` |
| IPv6 | [ipv6.md](ipv6.md) | the IPv6 slot path for Matter/Thread, the SDK API, the source copy, the remaining hardware proof |
| Measuring | [measurements.md](measurements.md) | per board the latest v3 measurement next to the v2 bar, the tooling, and the guide for updating; `tools/soak.sh` for hours |
| Memory | [heap.md](heap.md), [stacktask.md](stacktask.md) | the shared allocator of kernel and apps (free lists, ceiling, counters) and why; synchronous C code as a task with its own stack (`applib::stacktask`) |
| Storage | [storage-sync.md](storage-sync.md) | the confirmed storage barrier `OP_SYNC` for a database in an app |
| Reviews | [description.md](description.md), [review-2026-10-01.md](review-2026-10-01.md), [review-port-2026-10-02.md](review-port-2026-10-02.md) | the description layer by layer with what is duplicated or skewed; the read-through review of 2026-10-01 (what can go); the port review Go v2.2.7 next to Rust v3 (both dated snapshots) |

## Images per board

How to build and flash a board is in one place: in its own doc, under
"Building". `CFG=` gives each image its own config, `GUI=1` the gui flavor on
the boards with a framebuffer ([gui.md](gui.md)).

| Board | Script | Doc |
| --- | --- | --- |
| QEMU virt arm64 | `image/qemu-run.sh` | this page, [gui.md](gui.md) |
| UEFI generic (EDK2, QEMU), Orion O6N, Ampere Altra | `image/uefi-run.sh` (`BOARD=o6n` or `BOARD=altra` for the physical boards) | [boards.md](boards.md) |
| Raspberry Pi 4 and 5 | `image/rpi4.sh`, `image/rpi5.sh` (both via `image/raspi.sh`) | [boards-pi.md](boards-pi.md) |
| Radxa Zero 3E | `image/radxa-zero3.sh` | [boards-radxa.md](boards-radxa.md) |
| QEMU virt riscv64, LicheeRV Nano (SG2002) | `tools/qemu-riscv-test.sh`, `image/licheerv-agent.sh` | [boards-riscv.md](boards-riscv.md) |
| Mac mini M4 (t8132) | `image/apple-m4.sh` (helpers in `image/apple/`) | [boards-apple.md](boards-apple.md) |
| Kernel flip bundle (any board) | `image/flip-bundle.sh` | [flip.md](flip.md) |
| A release (every board headless and headfull, the bundles, the apps) | `tools/release.sh <version>` (`--dry-run`: build only) | [jobs/README.md](../jobs/README.md) |

The boards' firmware is in `image/firmware/<board>/` (the Pis, the Radxa's
boot chain, the LicheeRV's donor; provenance and sha256 in the
`LEESMIJ.txt` next to it). By default every image script takes the config
in three layers from `image/cfg` (`default.cfg`, `<board>.cfg`,
`headless.cfg` or with `GUI=1` `headfull.cfg`; the last value wins);
`CFG=` with a list of files replaces them. `default.cfg` sets
`hopos.storage=stateless`: a cold boot ignores the hopfs index and starts
with an empty tree; a layer with `hopos.storage=stateful` keeps it. The
config lands in the kernel's window on every board (`image/hopcfg.py`;
[boards.md](boards.md), "The config window"), and `hop image` from the hop
repo puts a different one in, in an image, a bundle or on a card. The cards
and sticks are built by `tools/mkcard`. Go apps such as cloudflared are in
`go/` ([go-apps.md](go-apps.md)).

Hop itself comes from the hop repo (`agentd-hopos`); the image scripts
build it from `HOP_DIR` with `tools/hop-build.sh` (against the applib of
this worktree, like the release) and bake it in as a resident. Secure Boot
must be off on the UEFI boards. The host side of Hop (the `hop` CLI,
`agentd`, the runners and the stores) is in the same hop repo and builds
with `cargo build --release -p agentd -p cli`.

A device day ends with a page in the browser: build
[`apps/welcome`](../apps/welcome/README.md), put `welcome.elf` on an HTTP
server on the laptop, and `curl -X POST -d '{"name":"welcome","driver":"hop","artifacts":[{"url":"http://LAPTOP:8000/welcome.elf"}],"memory_limit":33554432,"ports":{"http":80}}' http://NODE:9080/v1/jobs`
(with `hopos.insecure=1`); then `http://NODE/` shows the bunny, the node,
the slot, the uptime, the requests and the heap. After that the measurement
series from [measurements.md](measurements.md) and a flip from
[flip.md](flip.md).

The console of a node without a UART to the laptop: `nc NODE 5555` first
gives the saved console (a ring of 256 KiB, which holds the boot for about a quarter of an hour) and then follows
live; at most four readers at once, and the fifth hears
`console: full, 4 readers`. A reader that is gone (a FIN, an RST, a failed
write, or 10 s of data without an ack) gives its place back immediately;
every hundredth is a `HOPOS_CONPORT_FREED` line with the reasons and the
stack's budget. On with `hopos.console=1` in the config or cmdline, off
with `hopos.console=0`; without that key it follows `hopos.insecure=1`
(`HOPOS_CONPORT_UP` in the boot).

## What is still to do

One list, per node and for everything at once: [ALLES.md](../ALLES.md) in
the root of the repo. What is done is taken out. The first green node was
the Pi 5 on 2026-09-30, including the first kernel flip on hardware.

How the code is written: the haas.software Rust handbook
(`rustdoc/README.md`, next to this repository) and the port notes
(`rustdoc/PORT.md`).
