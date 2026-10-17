# decode

A stream through the node's hardware decoder, with the fps. The smallest app
that uses the codec service the way Lumen does: open a session with
`applib::codec`, write bitstream into its own partition and feed it, offer
frame buffers, fetch events and count them. No frame goes over the
connection: a buffer is a piece of the app's own partition, the kernel maps it
into the codec's page tables (docs/media.md).

At the end, one line on the kernel console (the shape; not yet measured on
hardware):

```
slot 2: decode: <n> frames <w>x<h> p010 from /data/clip.hevc (<MB> MB) in <ms> ms: <fps> fps, <MB/s> MB/s through the grant HOPOS_DECODE fps=<fps> MBps=<MB/s>
```

The bar is the one from docs/media.md: 24 fps 4K P010 is 597 MB/s through the
grant.

After that the app closes its codec session and stays up (no restart loop in
Hop); remove it with `DELETE`. A graceful stop (`HOPOS_APP_STOP`) during the
measurement ends it with `decode: <file>: stopped after <n> frame(s)` (no
`HOPOS_DECODE_FAIL`) and closes the session before the exit: a session that
dies with the app leaves the codec holding a slot ("session slot 0 will not
terminate" on the O6N, 2026-10-04).

## The env

| Key | Default | What |
| --- | --- | --- |
| `DECODE_FILE` | `/data/clip.hevc` | the stream as the app sees it: a volume from the jobspec |
| `DECODE_URL` | | the stream from an `http://` URL over `appnet` (wins over `DECODE_FILE`) |
| `DECODE_CODEC` | from the extension | `hevc`, `h264`, `av1`, `vp9`, `vp8`, `mpeg2`, `vc1`, `jpeg` |
| `DECODE_PIXEL` | `p010` | the output format (`nv12` for 8 bit) |
| `DECODE_BUFS` | `12` | the frame buffers (at least what the firmware asks for plus one, at most 32) |

## Markers

| Marker | When |
| --- | --- |
| `HOPOS_DECODE_OPEN` | the kernel granted a session |
| `HOPOS_DECODE fps=… MBps=…` | the measurement, after Done and an empty queue |
| `HOPOS_DECODE_NOCODEC` | the open was refused (no VPU, no firmware for this codec, all sessions taken); the app stays up without doing anything |
| `HOPOS_DECODE_FAIL` | the stream, the heap, a decoder error or 20 s without an event |

## Memory

The buffers come from the app's heap, in whole pages: four input buffers of
1 MB and `DECODE_BUFS` frame buffers of the size the firmware names. 4K P010
is 24 MB per frame, so twelve frames is almost 300 MB: `memory_limit`
512 MiB for 4K, 128 MiB for 1080p. On QEMU (no VPU) 64 MiB is enough.

## Build

```sh
cargo build --release --target aarch64-unknown-none-softfloat -p decode
rust-objcopy --strip-debug target/aarch64-unknown-none-softfloat/release/decode decode.elf
```

## The jobspec

To Hop's leader (port 9080 on the node's address), with the stream on the
volume `/data`:

```sh
curl -X POST -H 'Content-Type: application/json' \
  -d '{"name":"decode","driver":"hop",
       "artifacts":[{"url":"http://LAPTOP:8000/decode.elf"}],
       "memory_limit":536870912,
       "env":{"DECODE_FILE":"/data/clip.hevc"},
       "volumes":{"/data":"/data"}}' \
  http://NODE:9080/v1/jobs
```

Or from a URL, without a volume:

```sh
  -d '{"name":"decode","driver":"hop",
       "artifacts":[{"url":"http://LAPTOP:8000/decode.elf"}],
       "memory_limit":536870912,
       "env":{"DECODE_URL":"http://LAPTOP:8000/clip.hevc"}}'
```

Any app that mounts `/data` can put the stream on the volume, or Lumen can;
the kernel also reads it from there for its measuring tool.

The 1 MB chunks are cut at the last Annex-B start code, so that each NAL unit
reaches the decoder whole and the rest goes to the next chunk. MEASURED
2026-09-30 on the O6N: a NAL that spanned two chunks made the decoder fault
after 14 frames; fed whole, the same clip did 85.7 fps.

## On QEMU

Without a VPU the kernel refuses the open loudly, and the app says so and
stays up (measured 2026-09-29 with a media kernel on QEMU virt and Hop in
slot 1):

```
slot 2: decode: the node has no hevc decoder for this app: system call op 14: status 1: this node has no codec hardware; staying up without one HOPOS_DECODE_NOCODEC
```

## Layout

- `src/main.rs`: the start, the measurement (feed, offer, fetch) and the
  line. One task: the codec calls are not idempotent and go over one system
  client, one after the other.
- `src/mem.rs`: buffers in whole pages in the app's own partition (the
  grant).
- `src/source.rs`: the stream from a file (system API) or a URL (leanhttp
  over `applib::tcp::Dialer`), straight into the input buffer.
