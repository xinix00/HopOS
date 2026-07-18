# Flash & boot

Three ways to a running node. Prebuilt, signed images are on the
[releases page](https://github.com/xinix00/HopOS/releases); verification at
the bottom.

## UEFI arm64 box (USB stick)

Any UEFI arm64 machine with ACPI — from an Ampere Altra server on down.

1. Format a USB stick as FAT32.
2. Copy `BOOTAA64.EFI` from the release to `EFI/BOOT/BOOTAA64.EFI`.
3. Create `hopos.cfg` in the stick's root — see [Configure](config.md).
4. Boot from the stick. That's the install.

Network needs an igb-family NIC (Intel i210/i211); without one the node
boots headless.

## Raspberry Pi 4 / 5 (SD card)

The Pi boots from its firmware, not UEFI — so it's the SD card's boot
partition instead:

1. Take an SD card with the standard Pi boot partition (`bootfs`).
2. Unzip `hopos-rpi5.zip` (or `hopos-rpi4.zip`) onto it — this drops the
   kernel and a `config.txt` pointing at it.
3. Put the [config keys](config.md) in `cmdline.txt` (one line, space-separated).
4. Insert, power on.

## QEMU (no hardware)

```sh
# needs: qemu-system-aarch64 + the tamago-go toolchain (see app.md)
git clone https://github.com/xinix00/HopOS && cd HopOS
./image/uefi-run.sh agent
```

Forwards the agent to `localhost:8080` and the leader API to `localhost:9080`.

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
