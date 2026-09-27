# Development

Build recipes are maintained in `image/` and `tools/`. Run the commands below from the repository root unless a command changes directory. They describe the checked source recipes; the [status page](status.md) records executed tests and exact candidates.

## Toolchain and module versions

| Component | Source baseline |
| --- | --- |
| Standard Go tools | Go 1.26.4, as declared by the modules |
| Bare-metal compiler | `usbarmory/tamago-go`, tag `tamago1.26.4`, plus this tree's runtime/net patches |
| Metal module | `github.com/xinix00/HopOS/metal/v2`; app modules pin v2.2.2 |
| TamaGo module | `github.com/usbarmory/tamago v1.26.4` |
| HOP agent/runner | `github.com/xinix00/hop v1.0.2` |
| Node network/HTTP support | `github.com/xinix00/lean v1.1.0` in `metal/go.mod` |

The TamaGo **compiler** and the TamaGo **module** are separate dependencies. Standard Go runs host tools and tests; the patched compiler produces `GOOS=tamago` images. Do not substitute a standard Go compiler for the latter.

For a new toolchain checkout:

```sh
git clone --branch tamago1.26.4 \
  https://github.com/usbarmory/tamago-go "$HOME/tamago-go"
(cd "$HOME/tamago-go/src" && ./make.bash)
sh tools/tamago-go/apply.sh
```

The patch script defaults to `$HOME/tamago-go`; `TAMAGO_SRC` selects another source checkout. Image builders use `$HOME/tamago-go/bin/go` unless `TAMAGO` is set. [The patch inventory](../tools/tamago-go/README.md) explains the idle/wake and network-address patches.

Most image builds use `GOWORK=off`. Apple builds instead use [image/apple/go.work](../image/apple/go.work), which replaces the TamaGo module with the sibling `../tamago` checkout. That checkout must contain the required high-RAM support. A clone of this repository alone does not supply that local checkout; inspect the workspace and use the tested fork revision recorded with the candidate. Do not remove the workspace requirement to make an Apple build appear reproducible.

RISC-V image/flip builds also require `riscv64-elf-as`, `riscv64-elf-ld`, and `riscv64-elf-objcopy`. Cold LicheeRV image builds require the vendor FIP and FIP tool selected by `LICHEERV_DONOR` and `LICHEERV_FIPTOOL`. UEFI/QEMU runs need the firmware files under `QEMU_SHARE` and the relevant QEMU executable.

## Build node images

These are the actual agent builders. The similarly named `rpi4-hopos.sh` and `rpi5-hopos.sh` build embedded demonstration kernels, so they are not the normal agent recipes.

```sh
GUI=0 sh image/rpi4-agent.sh
GUI=0 sh image/rpi5-agent.sh
GUI=0 sh image/radxa-zero3.sh
sh image/licheerv-agent.sh
AGENT=1 sh image/apple-m4.sh
BUILD_ONLY=1 GUI=0 sh image/uefi-run.sh agent
```

| Builder | Principal output / prerequisite |
| --- | --- |
| `rpi4-agent.sh` | `sd-rpi4/kernel8.img`; complete `metal/out/hopos-rpi4.img` additionally needs `start4.elf`, `fixup4.dat`, board DTB, and `bl31.bin` in `sd-rpi4/` |
| `rpi5-agent.sh` | `sd-rpi5/hop-agent5.img`; complete `metal/out/hopos-rpi5.img` needs the board DTB and `overlays/bcm2712d0.dtbo` in `sd-rpi5/` |
| `radxa-zero3.sh` | `metal/out/hopos-radxa-zero3.img`; downloads/caches the donor boot chain if absent |
| `licheerv-agent.sh` | `metal/out/hopos-licheerv.img` and FIP output; requires the vendor inputs |
| `apple-m4.sh` with `AGENT=1` | `metal/out/hopos-apple.img`; without `AGENT=1`, the default is a probe |
| `uefi-run.sh agent` | EFI output and `metal/out/hopos-edk2.img` (QEMU/EDK2); `BOARD=altra` or `BOARD=o6n` builds for that machine instead |

The Pi scripts report missing firmware and omit the complete card image when prerequisites are absent. Building the kernel alone does not create a bootable full image. [Getting started](getting-started.md) distinguishes writing a card, updating boot files, and installing an Apple boot object.

### Configuration

There are exactly two node configurations: `image/hopos-gui.cfg` and `image/hopos-headless.cfg`. Every node runs one of them with the same values; there is no per-board or per-node config. The builders pick one through `GUI=` (headless where a board has no display: LicheeRV, Mac mini). Boards that read their config from the image itself (Mac mini, LicheeRV) get it embedded, and so do their flip bundles; the others read it from their card or stick.

`CFG=` still overrides the template for a one-off measurement build. It is not a way to keep a node's own configuration. Path convention: `flip-bundle.sh` and `apple-m4.sh` take a repository-relative path; `licheerv-agent.sh` and the Pi, Radxa and UEFI builders read the path as given after changing directory, so use an absolute one.

The builders create temporary embedded files and remove them on exit. Run image builds **sequentially in one checkout**: they share output names, config embeds, and cage-stub embeds. Use separate checkouts for parallel builds. Do not run `tools/test.sh` concurrently with an image build in the same tree.

## Build flip bundles

```sh
sh image/flip-bundle.sh rpi5
```

Accepted board arguments are `rpi4`, `rpi5`, `radxa`, `virt`, `uefi`, `apple`, and `licheerv`. Output is `metal/out/hopos-<board>.flip`, followed by its SHA-256. The builder links two kernels at different addresses and derives the relocation table from their difference. It retains symbols and clears build IDs for that comparison.

`FLIPABI` overrides the bundle's declared flip ABI. It does not translate an incompatible kernel or replace compatibility checks. Use the default unless deliberately building an ABI test. Applying the bundle, maintaining configuration, and validating retained residents are covered in [kernel flip](kernel-flip.md).

## Applications and publication

[Applications](apps.md) gives direct ARM64 and RISC-V build commands. To build the configured application set against the current metal tree without publishing:

```sh
PUBLISH=0 sh tools/apps-release.sh
```

This script reads [hopos-apps.list](../tools/hopos-apps.list), including external module directories. Those checkouts must exist. It temporarily adjusts their metal dependencies, restores module files on exit, and writes ELF artifacts under `metal/out/apps/`. `LIST` selects another list; `METAL` selects a published metal version instead of the current source tree.

`tools/apps-release.sh` publishes by default. `tools/release.sh <tag>` also builds, signs, and uploads release assets. Use the individual image builders for local work; do not invoke release scripts as ordinary compilation checks. Release signing and publication use credentials outside the repository.

## Existing checks

```sh
sh tools/test.sh
```

The gate checks import direction, runs its selected host suites, and builds the configured TamaGo targets when the compiler is present. Read the final output: a missing toolchain is reported as a skipped target gate, not a target-build pass. Test arguments can select host tests, for example `sh tools/test.sh -run TestPool`.

Run the QEMU agent separately when testing behavior:

```sh
GUI=0 HOPCFG='hopos.insecure=1' sh image/qemu-run.sh agent
```

Host tests validate logic and formats. Target builds validate compilation. QEMU and physical hardware exercise different boot and driver paths; one result does not substitute for the others. Refer to [status](status.md) and [measurements](measurements.md) for recorded coverage and measurement conditions.

## Code boundaries

Application code belongs under `metal/app/` or separate app modules. The shared kernel framework is under `metal/kern/`; architecture-specific CPU mechanisms are under `metal/cpu/`, while boards wire hardware and drivers under `metal/board/` and `metal/driver/`. Preserve these existing boundaries when changing behavior. [Architecture](architecture.md), [lifecycle](lifecycle.md), and [networking](networking.md) describe the relevant ownership and call paths.

Return to the [documentation index](index.md).
