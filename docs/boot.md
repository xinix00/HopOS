# Flash & boot

Two roads to a running node: **the imager** — one window, no terminal — or the
image files by hand. Every download link on this page resolves to the **newest
release**, so nothing here goes stale
([all releases](https://github.com/xinix00/HopOS/releases)); the signature
check is at the bottom.

Every board ships as a **complete dd-able image** — SD card or USB stick.
Firmware and boot chain are already on it, and there is nothing to format,
rename or copy. The boot partition is plain FAT, so after writing it mounts on
macOS/Windows/Linux and `hopos.cfg` stays a text file you can edit; a kernel
update is a file copy.

Every arm64 asset comes in two flavours. **GUI** boots a desktop (display,
launcher, app catalog); **headless** is not a switched-off GUI but the same
image built with *zero* lines of GUI code linked, and its default config runs
one job — a `welcome` page on port 80 that tells you the node is up. The
RISC-V board is headless only: no framebuffer on that silicon.

## The imager — burn, configure, find

[hop-imager](https://github.com/xinix00/hop-imager) is the desktop tool for
HopOS nodes: it picks the release, writes the card and reads it back, edits a
node's config afterwards, and finds the nodes you already have.

| macOS | Linux | Windows |
|---|---|---|
| [`hop-imager-macos-universal.zip`](https://github.com/xinix00/hop-imager/releases/download/rolling-release/hop-imager-macos-universal.zip) | [`hop-imager-linux-amd64.tar.gz`](https://github.com/xinix00/hop-imager/releases/download/rolling-release/hop-imager-linux-amd64.tar.gz) | [`hop-imager-windows-amd64.zip`](https://github.com/xinix00/hop-imager/releases/download/rolling-release/hop-imager-windows-amd64.zip) |

A new node is three steps:

1. **RELEASE** — the channel and the version. HopOS ships betas now and then,
   so "the latest release" is a choice rather than a given; the tool opens on
   the newest stable, and every release that carries card images is in the list
   if you need to match the version your other nodes run.
2. **Type and board** — *Desktop* or *Headless*, then your board. The three
   together select the image, which is downloaded once and cached. A board
   without that flavour stays visible but disabled (the LicheeRV Nano has no
   desktop image — that is the silicon, not a missing file).
3. **WRITE + VERIFY** — a card or stick shows up in TARGET by itself. One
   button writes it and reads it back, comparing byte for byte on the same open
   device, *before anything can mount it*. That is the only moment the answer
   means something: verify a minute later and you are comparing against bytes
   the operating system wrote itself when it mounted your fresh card.

**Updating a node keeps its config.** *Keep this card's config* is on by
default, because that is what an update should do: new code, same node. Before
a byte is written the tool reads the config off the card, saves a copy on your
machine (mode `0600` — it holds your keys), and puts it back into the fresh
image *after* the comparison. Name, API key, S3 credentials and jobs survive,
**verbatim** — it never merges your config with a newer default, but it does
tell you which keys that default sets and yours lacks.

**CONFIGURE** changes the config of a card you already wrote — no mount, no
rebuild, see [Configure](config.md). **FIND** scans your network: every node
serves its console on `tcp/5555` and hands over its retained boot log the
moment you connect, so a single TCP connect identifies a node *and* tells you
its name, architecture, type and state. No mDNS, no broadcast, no extra code
on the node.

Each platform reaches its devices the way that OS demands. On **macOS** run the
app bundle — since Sequoia an app needs an identity to hold local-network
permission, and a loose binary makes FIND see an empty network. On **Linux**,
`sudo` or the `disk` group. On **Windows**, administrator (it offers to restart
elevated).

The imager verifies the *write*; it does not check the release signature for
you. If you want that too, see [Verify a download](#verify-a-download) below.

## By hand — one image per board

The manual road is the same image, `gunzip`ped straight onto the medium:

```sh
diskutil unmountDisk /dev/diskN
gunzip -c hopos-rpi5.img.gz | sudo dd of=/dev/rdiskN bs=4m
```

| board | GUI | headless | what is on the medium |
|---|---|---|---|
| UEFI arm64 box (USB stick) | [`hopos-uefi.img.gz`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-uefi.img.gz) | [`hopos-uefi-headless.img.gz`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-uefi-headless.img.gz) | `EFI/BOOT/BOOTAA64.EFI` + the flavour's default `hopos.cfg`. Boot from the stick; that's the install |
| Raspberry Pi 5 | [`hopos-rpi5.img.gz`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-rpi5.img.gz) | [`hopos-rpi5-headless.img.gz`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-rpi5-headless.img.gz) | the Pi firmware (it boots from that, not UEFI), the kernel, `config.txt`, `hopos.cfg` |
| Raspberry Pi 4 | [`hopos-rpi4.img.gz`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-rpi4.img.gz) | [`hopos-rpi4-headless.img.gz`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-rpi4-headless.img.gz) | same as the Pi 5 |
| Radxa Zero 3E (RK3566) | [`hopos-radxa-zero3.img.gz`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-radxa-zero3.img.gz) | [`hopos-radxa-zero3-headless.img.gz`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-radxa-zero3-headless.img.gz) | the vendor boot chain (TPL/SPL + TF-A + U-Boot, from the official Radxa image) on the raw sectors where the boot ROM reads it; our kernel, `extlinux.conf` and `hopos.cfg` on the FAT partition U-Boot's distro-boot finds |
| LicheeRV Nano (RISC-V) | — | [`hopos-licheerv-headless.img.gz`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-licheerv-headless.img.gz) | the whole card: partition table, FAT partition, `fip.bin` with our kernel in it — and the config **baked in** |

A fresh card is a working node: the GUI flavour boots a desktop, headless runs
`welcome` on port 80. Set `hopos.apikey` before the node leaves a LAN you trust
— in the imager's CONFIGURE, or by editing the file — see
[Configure](config.md).

Notes per board, all of them consequences of the hardware:

- **UEFI** — network needs an igb-family NIC (Intel i210/i211); without one the
  node boots without external networking.
- **Radxa Zero 3E** — HDMI, gigabit ethernet and the hardware watchdog are all
  driven by HopOS itself on this board.
- **LicheeRV Nano** — a Sophgo SG2002 (two XuanTie C906 harts, 256 MB) for about
  €15; signed image since v1.6.0, one hart for HOP and one for apps. It has no SD
  driver of its own — the vendor's first-stage loader reads the card, HopOS never
  does — so there is no file on it to edit and its config lives *inside* the
  kernel. The imager still configures it (it patches the window in the FIP and
  fixes the checksums that guard it); by hand you rebuild, see below. What is
  baked in is the same headless default every other board gets: `welcome`, the
  console on TCP port 5555 (`nc <node-ip> 5555` — there is no framebuffer here
  and the UART needs a cable), and, because that image carries no key, an API
  that is **deliberately open** (`hopos.insecure=1`). Fine on a trusted LAN, not
  anywhere else. Two default-image boards on one LAN share a MAC until you give
  them node names — the boot log warns about it.

### Updating a card you already have

Not a reflash: the imager writes a new release onto the card and keeps its
config. By hand, unzip the board's `.zip` onto the mounted boot partition —
kernel, boot config and `hopos.cfg`, no reflash:
[`hopos-rpi5.zip`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-rpi5.zip),
[`hopos-rpi4.zip`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-rpi4.zip),
[`hopos-radxa-zero3.zip`](https://github.com/xinix00/HopOS/releases/latest/download/hopos-radxa-zero3.zip)
(each also as `-headless`). On a UEFI stick, copy
[`BOOTAA64.EFI`](https://github.com/xinix00/HopOS/releases/latest/download/BOOTAA64.EFI)
to `EFI/BOOT/BOOTAA64.EFI` — the headless build is published as
[`BOOTAA64-headless.EFI`](https://github.com/xinix00/HopOS/releases/latest/download/BOOTAA64-headless.EFI)
and has to be renamed to that exact name, because the firmware looks for it.

### Rebuilding the LicheeRV image

From a clone of this repo; needs riscv64 binutils and the Sipeed donor fip —
that board's first-stage loader and DRAM parameters:

```sh
CFG=~/my-node.cfg image/licheerv-agent.sh   # → metal/out/hopos-licheerv.img
image/licheerv-agent.sh /dev/diskN          # fast iteration: replaces just fip.bin
```

Your `CFG` file is the node's config: set `hopos.apikey`, drop
`hopos.insecure`, put your own `hopos.init[]` jobs in it. Keep it outside the
repo — it holds keys.

Our kernel replaces OpenSBI in the card's `fip.bin`; the vendor's first-stage
loader does clock and DRAM init and enters us in machine mode — U-Boot and
Linux never get a turn. What runs on it is the full node, not a subset: agent
and leader on the LAN over our own DWMAC + internal-ePHY driver (100 Mb, DHCP,
NTP), the slot lifecycle with kill and restart, on-die temperature, the
hardware RNG, and **two apps sharing the one app hart** — measured with a web
server and a Cloudflare tunnel side by side at ~37 % of the hart each. The cage
here is a **PMP whitelist** plus a supervisor page table rather than an ARM
stage-2 mapping, because the C906 has no hypervisor extension; the app ABI is
identical, only the mechanism under it differs. See
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
network (`hopos.console`, see [Configure](config.md)) — which is also where the
imager's FIND reads the boot log. Once the node is up, `http://<node-ip>/` is
the install check: the default config runs a page there that reports cores, RAM
partition, architecture and uptime.

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
