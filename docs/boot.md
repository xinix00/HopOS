# Flash & boot

Four ways to a running node. Every download link on this page resolves to the
**newest release** — GitHub resolves `latest`, so nothing here goes stale
([all releases](https://github.com/xinix00/HopOS/releases)); verification at
the bottom.

Every arm64 asset comes in two flavours. **GUI** boots a desktop (display,
launcher, app catalog); **headless** is not a switched-off GUI but the same
image built with *zero* lines of GUI code linked, and its default config runs
one job — a `welcome` page on port 80 that tells you the node is up. The
RISC-V board is headless only: no framebuffer on that silicon.

## UEFI arm64 box (USB stick)

Any UEFI arm64 machine with ACPI — from an Ampere Altra server on down.

1. Format a USB stick as FAT32.
2. Copy [`BOOTAA64.EFI`](https://github.com/xinix00/HopOS/releases/latest/download/BOOTAA64.EFI)
   to `EFI/BOOT/BOOTAA64.EFI` on the stick. Headless:
   [`BOOTAA64-headless.EFI`](https://github.com/xinix00/HopOS/releases/latest/download/BOOTAA64-headless.EFI),
   renamed to `BOOTAA64.EFI` — the firmware looks for that exact name.
3. Copy [`hopos.cfg`](https://github.com/xinix00/HopOS/releases/latest/download/hopos.cfg)
   (headless:
   [`hopos-headless.cfg`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-headless.cfg),
   renamed to `hopos.cfg`) to the stick's root and set `hopos.apikey` — that
   default config boots as-is; see [Configure](config.md) to tune it.
4. Boot from the stick. That's the install.

Network needs an igb-family NIC (Intel i210/i211); without one the node
boots without external networking.

## Raspberry Pi 4 / 5 (SD card)

The Pi boots from its firmware, not UEFI — so it's the SD card's boot
partition instead:

1. Take an SD card with the standard Pi boot partition (`bootfs`).
2. Unzip
   [`hopos-rpi5.zip`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-rpi5.zip)
   (or
   [`hopos-rpi4.zip`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-rpi4.zip);
   headless:
   [`hopos-rpi5-headless.zip`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-rpi5-headless.zip),
   [`hopos-rpi4-headless.zip`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-rpi4-headless.zip))
   onto it — this drops the kernel, a `config.txt` pointing at it, and the
   flavour's default `hopos.cfg`.
3. Edit `hopos.cfg` on the card: set `hopos.apikey` — see
   [Configure](config.md) for all keys.
4. Insert, power on.

## LicheeRV Nano — RISC-V (SD card)

The first non-ARM board: a Sophgo SG2002 (two XuanTie C906 harts, 256 MB) for
about €15. Signed image since v1.6.0 — one hart for HOP, one for apps:

```sh
gunzip hopos-licheerv.img.gz
diskutil unmountDisk /dev/diskN
sudo dd if=hopos-licheerv.img of=/dev/rdiskN bs=4m
```

That
[`hopos-licheerv.img.gz`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-licheerv.img.gz)
is the **whole card**: partition table, FAT boot partition, `fip.bin`. Nothing
to copy onto it afterwards — and nothing to edit either. This board has no SD
driver of its own (the vendor's first-stage loader reads the card, HopOS never
does), so its config is **baked into the image**. What went into the release
build is published next to it as
[`hopos-licheerv.cfg`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-licheerv.cfg)
so you can see exactly what you are booting: a bench node that seeds the
`welcome` job, streams its console over TCP port 5555 (`nc <node-ip> 5555` —
there is no framebuffer here and the UART needs a cable), and — because that
image carries no key — runs its **API deliberately open** (`hopos.insecure=1`).
Fine on a trusted LAN, not anywhere else: for your own key, or any other
change, rebuild.

Rebuilding it (from a clone of this repo) needs riscv64 binutils and the Sipeed
donor fip — that board's first-stage loader and DRAM parameters:

```sh
CFG=~/my-node.cfg image/licheerv-agent.sh   # → metal/out/hopos-licheerv.img
image/licheerv-agent.sh /dev/diskN          # fast iteration: replaces just fip.bin
```

Your `CFG` file is the node's config: set `hopos.apikey`, drop
`hopos.insecure`, and put your own `hopos.init[]` jobs in it. Keep it outside
the repo — it holds keys.

Our kernel replaces OpenSBI in the SD card's `fip.bin`; the vendor's
first-stage loader does clock and DRAM init and enters us in machine mode —
U-Boot and Linux never get a turn. What runs on it is the full node, not a
subset: the agent and leader on the LAN over our own DWMAC + internal-ePHY
driver (100 Mb, DHCP, NTP), the slot lifecycle with kill and restart, on-die
temperature, the hardware RNG, and **two apps sharing the one app hart** —
measured with a web server and a Cloudflare tunnel side by side at ~37 % of
the hart each. The cage here is a **PMP whitelist** plus a supervisor page
table rather than an ARM stage-2 mapping, because the C906 has no hypervisor
extension; the app ABI is identical, only the mechanism under it differs. See
[isolation](technical/isolation.md) and `metal/kern/cage`.

## QEMU (no hardware)

```sh
# needs: qemu-system-aarch64 + the tamago-go toolchain (see app.md)
git clone https://github.com/xinix00/HopOS && cd HopOS
./image/uefi-run.sh agent
```

Forwards the agent to `localhost:8080` and the leader API to `localhost:9080`.

> **QEMU runs hot — by design.** HopOS parks idle cores with WFE, which is a
> real clock-gated sleep on silicon but a no-op under QEMU's TCG emulation, so
> every powered core burns a full host core even when idle. We deliberately
> don't carry interrupt plumbing just to cool an emulator: QEMU is the test
> bench, and on every board HopOS actually targets an idle core costs
> ~nothing. Hardware acceleration is no way out either — HVF (macOS) cannot
> give the guest EL2, and HopOS requires an EL2 boot for the stage-2 cage.
> Fewer cores = less heat: `SMP=2 image/qemu-run.sh agent`.

## What you should see

```
   (\(\
   ( -.-)     HopOS
   o_(")(")   --------------
              the Go-only OS

hop: agent starting — node <name> · HOPOS_AGENT_UP
```

On UEFI machines the console is the screen (GOP) and the SPCR serial port; on
the Pi it's HDMI and the UART pins; on the LicheeRV it's the UART pins, or the
network (`hopos.console`, see [Configure](config.md)). Once the node is up,
`http://<node-ip>/` is the install check — the default config runs a page
there that reports cores, RAM partition, architecture and uptime.

## Verify a download

Every release ships
[`SHA256SUMS`](https://github.com/xinix00/HopOS/releases/latest/download/SHA256SUMS)
plus a
[`SHA256SUMS.sig`](https://github.com/xinix00/HopOS/releases/latest/download/SHA256SUMS.sig)
made with the project's ed25519 key — one signature covers every asset —
and the verification keys
[`allowed_signers`](https://github.com/xinix00/HopOS/releases/latest/download/allowed_signers)
and
[`release_key.pub`](https://github.com/xinix00/HopOS/releases/latest/download/release_key.pub)
ship next to it (and live in `tools/` in this repo):

```sh
ssh-keygen -Y verify -f allowed_signers -I hello@gethop.org \
    -n gethop-release -s SHA256SUMS.sig < SHA256SUMS \
  && shasum -a 256 -c SHA256SUMS
```
