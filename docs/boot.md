# Flash & boot

Three ways to a running node. Prebuilt, signed images are on the
[releases page](https://github.com/xinix00/HopOS/releases); verification at
the bottom.

## UEFI arm64 box (USB stick)

Any UEFI arm64 machine with ACPI — from an Ampere Altra server on down.

1. Format a USB stick as FAT32.
2. Copy `BOOTAA64.EFI` from the release to `EFI/BOOT/BOOTAA64.EFI`.
3. Copy the release's `hopos.cfg` to the stick's root and set `hopos.apikey`
   — that default config boots a full desktop (display, launcher, app
   catalog); see [Configure](config.md) to tune it.
4. Boot from the stick. That's the install.

Network needs an igb-family NIC (Intel i210/i211); without one the node
boots headless.

## Raspberry Pi 4 / 5 (SD card)

The Pi boots from its firmware, not UEFI — so it's the SD card's boot
partition instead:

1. Take an SD card with the standard Pi boot partition (`bootfs`).
2. Unzip `hopos-rpi5.zip` (or `hopos-rpi4.zip`) onto it — this drops the
   kernel, a `config.txt` pointing at it, and the default `hopos.cfg`
   (a full desktop: display, launcher, app catalog).
3. Edit `hopos.cfg` on the card: set `hopos.apikey` — see
   [Configure](config.md) for all keys.
4. Insert, power on.

## LicheeRV Nano — RISC-V (SD card, build from source)

The first non-ARM board: a Sophgo SG2002 (XuanTie C906, 256 MB) for about €10.
No prebuilt image yet — build one from the tree (needs riscv64 binutils and
the Sipeed donor fip for its first-stage loader and DRAM parameters):

```sh
image/licheerv-agent.sh                # → metal/out/hopos-licheerv.img
diskutil unmountDisk /dev/diskN
sudo dd if=metal/out/hopos-licheerv.img of=/dev/rdiskN bs=4m
```

That image is the whole card: partition table, FAT boot partition, `fip.bin`.
Nothing else to copy. For quick iteration on a card that already has one,
`image/licheerv-agent.sh /dev/diskN` replaces just `fip.bin`.

Our kernel replaces OpenSBI in the SD card's `fip.bin`; the vendor's first-stage
loader does clock and DRAM init and enters us in M-mode. What runs today is the
slot lifecycle — a Go app in a verified cage on the second hart, with kill and
restart — not the agent: this board's ethernet is not in service yet. The cage
here is a **PMP whitelist** rather than an ARM stage-2 mapping, because
the C906 has no hypervisor extension; see
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

On UEFI machines the console is the screen (GOP) and the SPCR serial port;
on the Pi it's HDMI and the UART pins.

## Verify a download

Every release ships `SHA256SUMS` signed with the project's ed25519 key
(`tools/release_key.pub` in this repo):

```sh
ssh-keygen -Y verify -f allowed_signers -I hello@gethop.org \
    -n gethop-release -s SHA256SUMS.sig < SHA256SUMS \
  && shasum -a 256 -c SHA256SUMS
```
