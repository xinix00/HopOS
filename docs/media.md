# The media plane: the O6N video codec

The hardware decoder of the Orion O6N (Arm China Linlon V8, "mve") and the
codec service an app uses to run a stream through the decoder, in v3.

Everything sits behind the feature `media` (on `hopos`, `kern`, `applib` and
`board-o6n`). Without it the kernel does not know the codec ops and answers
"unknown op".

## The shape

| Crate / file | What it owns |
| --- | --- |
| `driver/codec` (`driver-codec`) | The contract: `Engine::open(&Config) -> Session`, `feed`, `offer`, `next_event`, `close`, `reap`; `Buffer { pa, size }`; `Event` with `Kind::{Consumed, Produced, Format, Done, Fault}`; the `Firmware` source. A `Session` is a handle without `Clone`; whoever drops it puts its number in the engine's `Graveyard`, and the engine closes the session on its next turn. |
| `media/mve` (`media-mve`) | The Linlon V8: registers (typed, `const` asserts on every offset), the arena (bitmap, first-fit, alignment), the page tables of the codec MMU, firmware loading, the rings with the running checksum, the pump (RPC memory, messages, buffers, the output flush handshake, EOS on the last frame), the fixed places per buffer. One `Device` per VPU, owned by one task; no lock. |
| `kern/src/codecabi.rs` | The system functions (open, feed, offer, poll, close), the session table per slot lifetime, `codec_grant` with its three checks, and cache maintenance (the VPU is not coherent). |
| `abi/src/hopabi.rs`, `hopabi::codec` | The payloads on the wire: `OpenArgs` (8), `FeedArgs` (24), `BufArgs` (4), `Event` (64 bytes), byte for byte those of v2. |
| `applib/src/codec.rs` | The client: `Session::{open, feed, offer, poll, close}` over `sys::Client::call_once` (never retried: the same feed twice is two chunks of bitstream). `poll` waits: if nothing is pending, the kernel holds the answer until the first event (at most `wait`, capped at one second) and pumps the engine every millisecond; the app sleeps on the answer. |
| `board/o6n/src/codec.rs` | Powering the VPU: windows, interrupt and `_CCA` from the DSDT (`scan_dsdt`), arena uncached, power domains via SCMI to the TF-A, clocks and perf domain via the SCP, reset, and the power cycle when the block is stuck (`needs_recovery`, `cycle_domains`, `recover`: at most `RECOVER_CYCLES` (3) cycles, with a fresh look at the registers after each). |
| `media/optical` (`media-optical`) | The bulk-only transport layer of an optical drive over a `Transport` trait (see below). |
| `hopos/src/codec.rs` | `codec::up` after storage: register the service with the system API and run the bring-up as a task: the arena from the lifecycle actor, the firmware from the hopfs actor, on the O6N the VPU on and the driver in, and the cleanup task that closes the sessions of stopped residents once per second. |
| `kern/src/slots.rs`, `kern/src/partmem.rs` | The arena outside the partition pool: `Request::ReserveDevice` and `ReleaseDevice` to the lifecycle actor (the owner of the pool), `PartitionPool::reserve_device` underneath. One line, `HOPOS_POOL_DEVICE`. |
| `kern/src/rpc.rs` | The kernel as a hopfs reader: `FsMsg::KernRead`, `kern_read` (in chunks, the buffers there and back) and `read_file` (a whole file, bounded). No slot and no generation; the task roots are closed to the kernel too. |
| `apps/decode` | The smallest app on the codec service: a stream from the volume or a URL through `applib::codec`, and the fps (`HOPOS_DECODE`). On a stop from the kernel it ends the measurement and closes its codec session before it exits (`app.stopped()`); a session that dies with the app left the codec with a stuck slot ("session slot 0 will not terminate", 2026-10-04). |

### Large data does not go through the kernel

A 4K frame in P010 is 3840 x 2160 x 1.5 x 2 = 24,883,200 bytes; at 24 fps
that is 597 MB/s. The slot LAN peaks at 550 MB/s. So the frames cannot go
over system calls, and they do not belong there: a buffer is a piece of the
app's own partition (`off`, `n` from `RamStart`, whole pages). The kernel
checks it (`codec_grant`: inside the partition, no wraparound, page-exact),
the driver maps it in the codec page tables, and that is both the grant and
the isolation: by construction the VPU cannot touch anything that does not
belong to this task.

The kernel does cache maintenance: `feed` cleans (`dc cvac`), `offer` cleans
and invalidates (`dc civac`), a filled result is invalidated before the app
reads it, a returned input costs nothing.

### Lifetime and evict

The session table belongs to one lifetime of a slot (the servicer's
generation). Every call carries the generation its connection was admitted
for; a table of another generation gives `None` (Revocable). A request from
the previous tenant therefore never lands in the successor's table. On
evict the table drops, the handles drop, and the engine closes the sessions:
`Drop` is the release, there is no `ReleaseCodecs` any more.

## Tests (v2 to v3)

| v2 | v3 | Tests |
| --- | --- | --- |
| `mve/session_test.go` (975 lines, 15 tests) | `media/mve/src/tests.rs` | all 15 (37 tests in the crate), with the same fake VPU (a real page walk through memory, a fake firmware that recomputes the checksum), plus: a dropped handle closes the session, a foreign handle is closed, a stuck slot is an error without a leak, describe/state carry the numbers |
| `mve/mve_test.go` (11) | same | all 11 (arena, MMU, firmware header) |
| `mve/queue_test.go` (7) | same | all 7 (the ring protocol) |
| `kern/slots/codecabi_test.go` (5) | `kern/src/codecabi/tests.rs` | all 5 (the race of the interrupted Open is indivisible in Rust; the test now proves the refusal before the hardware), plus cache maintenance per direction, the refusal outside the partition and the refusal without hardware |
| `board/o6n/hop/vpu_recovery_test.go` (3) | `board/o6n/src/codec.rs` | all 3, plus the DSDT reader and `vpu_recover_checks_after_every_cycle` (healthy without a cycle, healthy after 1 to 3, stuck after 3 is an error, a domain error stops) |
| `abi/hopabi/codec.go` | `abi/src/hopabi.rs` | the offsets of `EncodeEvent` byte for byte, the roundtrips |
| `optical/optical_test.go` (19) | `media/optical/src/tests.rs` | the BOT tests: the CBW from the spec, bounds and directions, the residue, a failed data phase that is not hidden, plus a lost track (tag, phase error, CBW) that resets, the retry on the status and both sense formats; the MMC tests follow with the MMC layer |
| (new) | `driver/codec`, `applib/src/codec.rs` | numbering equal to v2, the graveyard; the client speaks the kernel's wire and never retries; the codec from the extension |
| `slots/partmem_test.go` (`ReserveDevice`) | `kern/src/slots.rs` `a_device_block_leaves_the_pool_and_comes_back` | the block via the actor: on the grain, off the capacity, never in a partition, a refusal with the numbers, and back |
| (new) | `kern/src/rpc/tests.rs` `the_kern_reads_a_firmware_blob_without_a_slot` | the kernel read on the fake hopfs: a 300 KB blob whole through mailbox and actor, too large without allocation, a missing name as `NoEnt`, in chunks with the same buffers, the task roots closed, frozen is `Busy` |
| (new) | `apps/decode` | buffers on whole pages and findable at their offset, the fps and MB/s of the measurement, the codec from the stream name |

## The three hooks

All three exist (2026-09-29). The bring-up in `hopos/src/codec.rs` runs them
one after the other, each as a message to its owner:

1. **The partition of a live resident** (`kern/src/slots.rs`):
   `Servicers::partition(slot) -> Option<Region>` gives base and size from
   `ServicerCtl` while the servicer lives, and `hopos::codec::NodeLives`
   uses it; after the stop every grant is refused with `STATUS_DENIED`.
2. **An arena outside the partition pool**:
   `Request::ReserveDevice` to the lifecycle actor, once at boot, of
   `hopos.codec` MB (default 768 on the O6N; 0 is off, `HOPOS_CODEC_OFF`).
   The actor says where the block is and what is left for the slots
   (`HOPOS_POOL_DEVICE base=… mb=…`, or `_FAIL` with the numbers). The actor
   handles it after any adoption (kernel flip) and before Hop is placed. If
   the hardware does not come up, the block goes back
   (`HOPOS_POOL_DEVICE_RELEASE`). On a board without a VPU the default is
   64 MB, and it goes back right after `HOPOS_CODEC_NONE`: that way QEMU
   proves the path.
3. **A kernel read from hopfs** (`kern/src/rpc.rs`): the hopfs actor reads
   for the kernel itself, after the mount and before the VPU comes on, the
   sixteen blobs (`media_mve::FIRMWARE`, eleven decoders and five encoders
   of about 300 KB, fetched by Lumen from Sky1-Linux/sky1-firmware) as
   `/firmware/<name>.fwb` or
   `/codec-firmware/<name>.fwb` (where Lumen puts them: its jobspec mounts
   `/firmware` on that volume, `jobs/hopos-media-o6n.cfg`). One line with
   the count; what is missing is named in `HOPOS_CODEC_NOFW
   missing=hevcdec,…`, and an open of that codec is refused with "no
   firmware for this codec". Without a volume: `HOPOS_CODEC_NOFW missing=all`.

### The trial on QEMU

A media kernel on QEMU virt (`--features board-qemuvirt,media`) with Hop in
slot 1 and apps/decode placed via Hop, measured 2026-09-29:

```
pool: 64 MB for the codec arena at 0xbc000000 outside the partition pool, 1712 MB left for slots (largest 1536 MB) HOPOS_POOL_DEVICE base=0xbc000000 mb=64
codec: no video codec on this board, codec calls refused HOPOS_CODEC_NONE
pool: 64 MB of the codec arena at 0xbc000000 back in the partition pool, 1776 MB for slots HOPOS_POOL_DEVICE_RELEASE
HOPOS_HOP_START slot=1 core=0 cpu=0 entry=0x50010000 part=0xbc000000+0x4000000 ...
slot 2: decode: the node has no hevc decoder for this app: system call op 14: status 1: this node has no codec hardware; staying up without one HOPOS_DECODE_NOCODEC
```

The reservation comes before Hop is placed, and Hop then gets exactly the
block that came back. Decode stays up (no exit, no restart).

## The optical drive

The optical drive (`media/optical`) is
connected in code: the bulk-only transport layer (CBW, data phase, CSW with
the checks on signature, tag, residue and status, reset recovery, the single
retry on a stalled status, REQUEST SENSE) against an
async `Transport` trait (`media/optical/src/asynchronous.rs`) with seven
tests against a fake drive, the MMC layer above it (`mmc.rs`: open, the
size, READ(10) over 2048-byte sectors) and the device command ABI
(`OP_DEVICE_COMMAND`, `abi/src/hopabi/device.rs`). An app reaches a drive only through an
explicit mount on `/devices/discN` (`kern/src/deviceabi.rs`); the
optical owner (`hopos/src/optical.rs`, feature `media`) is its own actor
next to hopfs and lends its transfers one at a time to the USB owner, which
therefore never waits in the hopfs actor. The MMC layer has no host test of
its own yet and has not run on QEMU or hardware. The owner also starts when
no USB controller is available, so a disc request then gives `NoEnt`
instead of waiting forever. BOT reset has its own bounded deadline; an
expired read deadline does not block the reset right away. The encoder stays
in the separate Lumen app; the SDK offers at most twelve app cores and 512 KiB
of stack per secondary core actually started.

## Checklist: the O6N media test

Build: `MEDIA=1 BOARD=o6n CFG=jobs/hopos-media-o6n.cfg sh
image/uefi-run.sh` (the gate builds the same). The ESP is in
`target/uefi-esp-o6n-media/`: `EFI/BOOT/BOOTAA64.EFI` on a FAT32 stick. With
`CFG` the config goes into the config window of `BOOTAA64.EFI`
(`image/hopcfg.py`, see the config section of docs/boards.md) and there is no
`hopos.cfg` on the ESP. The config sets `hopos.codec=768`. It does not set
`hopos.storage`, and since 3.0.11 the default is stateless
(`HOPOS_FS_STATELESS`): a cold boot ignores the index and starts with an
empty tree, so firmware blobs and a stream put on the volume in an earlier
boot are not seen. Add `hopos.storage=stateful` to keep them; a kernel flip
landing keeps the tree either way. For the measurement without Lumen,
apps/decode with a stream at `/data/clip.hevc` on the volume: `cargo build
--release --target aarch64-unknown-none-softfloat -p decode`, the jobspec in
`apps/decode/README.md`.

| Step | Marker or line | What it proves | Deviation |
| --- | --- | --- | --- |
| 0a | `pool: 768 MB for the codec arena at 0x… outside the partition pool, N MB left for slots … HOPOS_POOL_DEVICE base=0x… mb=768` | the arena comes out of the pool, before the first placement | `HOPOS_POOL_DEVICE_FAIL` with free and largest gap: lower `hopos.codec`, or the pool is smaller than thought; "did not answer": the slots did not come up |
| 0b | `codec: 16 of 16 firmware blobs read from the volume (… KB)` | the kernel read from hopfs: `/firmware` or `/codec-firmware` | `HOPOS_CODEC_NOFW missing=…`: those blobs are not on the volume (Lumen fetches them before its first backup; the next open loads them late); `missing=all` without a volume, or after a stateless cold boot |
| 1 | `vpu: TF-A SCMI channel alive, power protocol vX.Y` | the SMC channel to the TF-A answers | no line: the channel at 0x84380000 is not mapped or the SMC function is wrong |
| 2 | `vpu: power domains 4 5 11-15 on (confirmed by the firmware)` | hub, top and four cores on, read back | `power domain N not on`: the TF-A refuses; stop here, the first register read would be an SError |
| 3 | `vpu: mm ni700 clock on ...`, `vpu: vpu apb clock on ...` | the clocks of interconnect and block | "no SCMI channel offers the clock protocol": registers then read zero |
| 4 | `HOPOS_VPU_RECOVER` (only after a stuck block), one line per cycle with `(n of 3)`; then `vpu: recovered after N power cycle(s), pgctrl=… terminate=… HOPOS_VPU_RECOVERED` | the power cycle 15..11 off, 11..15 on, then clocks, perf, unreset and 10 ms of rest before PGCTRL and TERMINATE are read again; at most three cycles | healthy means PGCTRL `0x07cefffc` and TERMINATE zero (measured 2026-09-27). Still stuck after three cycles: `vpu: still stuck after 3 power cycles, pgctrl=… terminate=…` and `HOPOS_CODEC_OFF` instead of a false UP (on 2026-10-04, 3.0.11, one cycle without a second look gave UP, and Lumen never got an event) |
| 5 | `vpu: id 0x56648002 rcsu ... (windows 0x14240000/0x14230000, intid 358, cca Some(false))` | the block is alive; windows, interrupt and `_CCA = 0` from the DSDT | id 0: no clock or hub; 0xffffffff: no bus; `cca Some(true)`: the firmware calls the block coherent, the arena may then be cached |
| 6 | `codec: Linlon V8 (id 0x56648002 rev ...): 4 cores, N sessions, fuse ..., arena 768 MB (768 MB free) HOPOS_CODEC_UP` | the driver runs, the arena is whole | `HOPOS_CODEC_OFF` with the reason (power, probe), and then `HOPOS_POOL_DEVICE_RELEASE`: the arena is back in the pool |
| 6a | apps/decode via Hop (`apps/decode/README.md`): `slot N: decode: hevc session 1 open … HOPOS_DECODE_OPEN`, then `… HOPOS_DECODE fps=… MBps=…` | the same measurement through the whole ABI: open, feed, offer, poll over the wire, the grant in its own partition | `HOPOS_DECODE_NOCODEC` with the kernel's text ("no firmware for this codec", "all hardware sessions in use"); `HOPOS_DECODE_FAIL` with the reason |
| 7 | Lumen starts a backup | open, feed, offer, poll over the wire; the firmware asks for its reference frames (RPC) | a Fault with `firmware asked for N MB and arena ran out`: raise `hopos.codec` |
| 8 | fps in Lumen's `/api/state` | the measurement: **24 fps 4K P010 = 24,883,200 bytes per frame = 597 MB/s through the grant**, not a byte over the connection. v2 reached 27.25 fps with Lumen on GAMEOFTHRONES_S1_D1 (2026-09-27, with the software encoder behind it) | below 24 at 4K: first the cache ops (`dc civac` over 24 MB per frame), then the pump (the cleanup task and the waiting poll, which the kernel pumps every millisecond) |
| 9 | stop Lumen in the middle of a film; start it again | evict closes the sessions within one second (the cleanup task); the new lifetime opens on the same LSID | "all hardware sessions in use": a lifetime that did not drop |
| 10 | two films in a row | after the first the arena is whole again (`describe` after close) | less free than total: a leak in RPC memory or tables |

A silent session (no events, no Fault) is the first question for `state`:
`enable`, `jobqueue`, per LSID `sched`, `irqhost`, `mmu` and the counters
(`flushes`, `flushback`, `eos`, `rpc[allocs]`, `bufs[offered back held]`).

## Installing firmware after boot

The media candidate reads a missing blob late on `OP_CODEC_OPEN` via the
`FsInbox` of the same system connection. First `/firmware/<name>.fwb`,
then `/codec-firmware/<name>.fwb` if the first does not exist. The driver
supplies the fixed name; requests cannot choose a free file path. The read
is bounded at 4 MiB. Missing, empty, oversized or invalid blobs give the app
an error right away, without a hardware session.

The read happens outside the synchronous codec cell. The reply slot stays
with the connection until the filesystem actor answers; no pending read is
given up and then reused with the same reply. After the read MVE validates
the firmware, the cache takes ownership of it, and the normal open checks
the slot generation again. A stopped app therefore gets no late session.
Existing sessions have their own firmware copy in the arena. Cache hits do
no filesystem I/O.

The app stays responsible for downloading and content pins before opening;
the kernel gets no HTTP client for this. Lumen does this for all sixteen
firmwares via its existing installer. The system wire ABI does not change.

Host tests use a real HopFS actor and test both directories, a second
open without a storage read, missing/oversized files and a lifecycle change
while the read runs. The emulated MVE validates a late firmware and then
opens without probing again. The O6N kernel (`board-o6n,media`, PIE)
builds with strict clippy. This is not yet a decoder run on physical O6N
hardware.
