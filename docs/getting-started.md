# Getting started

This procedure covers a cold installation and a first job. Later kernel updates use the separate [kernel-flip procedure](kernel-flip.md). The source baseline is v2.2.2; check [status](status.md) for which exact builds and board operations have been tested.

## Select a board image

Use the release asset matching the board. These names come from [the release builder](../tools/release.sh); an absent asset is not supplied by selecting a similar board's image.

| Board / boot route | Headless release asset | Installation route |
| --- | --- | --- |
| Raspberry Pi 4 | `hopos-rpi4-headless.img.gz` | Complete SD image, including the Pi firmware/TF-A boot chain |
| Raspberry Pi 5 | `hopos-rpi5-headless.img.gz` | Complete SD image |
| Radxa Zero 3E | `hopos-radxa-zero3-headless.img.gz` | Complete SD image with its U-Boot boot chain |
| LicheeRV Nano | `hopos-licheerv-headless.img.gz` | Complete SD image with `fip.bin` and embedded node configuration |
| ARM64 UEFI | `hopos-uefi-headless.img.gz` | FAT boot medium containing `EFI/BOOT/BOOTAA64.EFI` |
| Mac mini M4 | `hopos-m4-headless.img.gz` | Installer medium; run its installer from macOS Recovery |

The M4 asset is an installer medium, not an SD-style replacement for the Mac's internal disk. Hardware details and prerequisites belong in [boards and drivers](boards-drivers.md). The Pi, Radxa, and UEFI release builders also produce GUI variants; start with headless when display and input are not required.

## Configure the node

The supplied headless template starts a `welcome` job on port 80, enables the TCP console on 5555, and explicitly sets `hopos.insecure=1`. This means anyone able to reach the management API can submit work. Use it only on a trusted test network, or configure `hopos.apikey` and remove the insecure flag before booting on another network.

For a node-specific configuration, copy the [headless template](../image/hopos-headless.cfg) and edit it. A nonempty API key enables HMAC authentication; removing both key and insecure flag refuses API startup. See [configuration](configuration.md) for the exact rules.

Pi and UEFI configurations are files on their boot media. Radxa supplies its configuration through the boot chain. LicheeRV embeds the configuration in its image; M4 normally uses an embedded configuration unless a loader supplies one. For those embedded routes, editing a separate `hopos.cfg` after installation does not change the installed kernel. Build the intended configuration into the image as described in [development](development.md).

## Write an SD or USB image

Download the selected image together with its release checksums and check the download against `SHA256SUMS`. The release also supplies a signature and verification keys; [operations](operations.md) covers verification and update handling.

For example, on macOS, with a Pi 5 image in the current directory:

```sh
shasum -a 256 hopos-rpi5-headless.img.gz
gunzip -k hopos-rpi5-headless.img.gz
diskutil list
```

Identify the **removable target disk** before the following commands. Replace `N` with that disk number. The write replaces its contents; do not use an internal system disk.

```sh
diskutil unmountDisk /dev/diskN
sudo dd if=hopos-rpi5-headless.img of=/dev/rdiskN bs=4m
sync
diskutil eject /dev/diskN
```

Use the corresponding image filename for Pi 4, Radxa, LicheeRV, or the UEFI boot medium. On Linux, use the appropriate block-device path and `dd` block-size syntax for that host; the image contents do not change.

A local Pi build may produce a kernel file even when required firmware files are missing and no complete card image was produced. Confirm that the `.img` exists and the script reported a complete image; [development](development.md) lists the prerequisites.

## Install on M4

Boot into Recovery and open Terminal. The installer medium contains the kernel image and `install.sh`. Assuming it is mounted at `/Volumes/HOPOS`, inspect the installation plan first:

```sh
sh /Volumes/HOPOS/install.sh
```

The installer checks the selected macOS volume, image alignment, security policy, and disk layout. Installation changes the boot object and may shrink the macOS container. Follow the script's reported security-policy requirements and verify the selected volume before applying the plan:

```sh
sh /Volumes/HOPOS/install.sh go
```

The [installer source](../image/apple/install.sh) documents `VOLUME`, `IMAGE`, `revert`, and `m1n1` options. Keep the installed macOS/Recovery environment required by this boot route. M4 bootloader and hardware details are in [boards and drivers](boards-drivers.md).

## Boot and find the node

Connect Ethernet and boot the node. Read the assigned IP address from the router's DHCP lease table or the board console. The headless welcome job downloads its artifact, so its startup also requires access to the configured artifact server. A working management endpoint does not imply that this download has completed.

For a node deliberately configured without API authentication, replace the address below with its assigned IP:

```sh
NODE=192.168.1.100
curl "http://$NODE:8080/health"
curl "http://$NODE:8080/tasks"
nc "$NODE" 5555
```

Open `http://<node-ip>/` for the default welcome page. The TCP console is read-only and unauthenticated; it is not a shell. Authenticated nodes require the HOP signing path for protected API requests; see [operations](operations.md).

## Submit a first job

Build and serve an application using [applications](apps.md). Save its job manifest as `job.json`, using an artifact URL the node can reach. A URL using `localhost` refers to the node itself, not the development machine.

On a node with only one application core, stop the default dedicated welcome job before placing another dedicated job. The following examples are for the explicitly insecure test configuration:

```sh
curl -X DELETE "http://$NODE:8080/v1/jobs/welcome"
curl -X POST "http://$NODE:8080/v1/jobs" \
  -H 'Content-Type: application/json' --data-binary @job.json
curl "http://$NODE:8080/tasks"
```

The agent forwards `/v1/jobs` management requests to the leader. Check task state and logs before assuming the application is running. A task waiting for capacity does not indicate a successful start. Port publication, replacement, and retained state are covered in [operations](operations.md).

## QEMU without removable media

After preparing the toolchain and QEMU, run the actual agent mode from the repository root:

```sh
GUI=0 HOPCFG='hopos.insecure=1' sh image/qemu-run.sh agent
```

Default forwarded host ports are 8080 for the agent, 9080 for the leader, and 18080 for an application publication on 18080. An artifact server on the development host is reachable from QEMU at `10.0.2.2`, for example `http://10.0.2.2:8000/app.elf`. The ordinary `agent` command is a development boot; use the dedicated flip build/procedure for flip tests.

Next: [applications](apps.md), [operations](operations.md), [configuration](configuration.md). Return to the [documentation index](index.md).
