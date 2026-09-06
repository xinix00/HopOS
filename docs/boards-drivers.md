# Boards and drivers

[Documentation index](index.md) · [Architecture](architecture.md) · [Test status](status.md)

The shared framework owns app partitions, placement, lifecycle and services. Architecture code implements the execution boundary. Board code supplies the memory plan, core operations and devices. The tables below describe implemented paths; [status](status.md) records which builds and hardware transitions have actually been tested.

## Boot, cores and memory

| Board | Boot and execution boundary | Core and memory source |
| --- | --- | --- |
| QEMU `virt`, ARM64 | QEMU virt boot; EL2 and per-app stage-2 tables | PSCI core discovery/start; virt memory plan and supplied device tree. [Board](../metal/board/qemuvirt/hop/board.go), [plan](../metal/board/qemuvirt/qemuvirt.go). |
| Mac mini M4 / t8132, ARM64 | Apple boot object installed through Recovery, then iBoot; EL2 with VHE and per-app stage-2 tables | Apple CPU topology, own core-start path; high physical RAM and reserved node structures. The tested M4 has nine app cores. [Board](../metal/board/apple/hop/board.go), [plan](../metal/board/apple/apple.go). |
| Raspberry Pi 4, ARM64 | Pi firmware loads the kernel; EL2 and per-app stage-2 tables | Shared Raspberry Pi core implementation and board memory plan. [Board](../metal/board/rpi4/hop/board.go), [shared core code](../metal/board/raspi/hop/base.go), [plan](../metal/board/raspi/plan.go). |
| Raspberry Pi 5, ARM64 | Pi firmware loads the kernel; EL2 and per-app stage-2 tables | Shared Raspberry Pi core implementation with Pi 5 device addresses. [Board](../metal/board/rpi5/hop/board.go), [plan](../metal/board/rpi5/rpi5.go). |
| Radxa Zero 3 / RK3566, ARM64 | U-Boot loads the kernel; EL2 and per-app stage-2 tables | PSCI core operations; board carve and app pool. [Board](../metal/board/rk3566/hop/board.go), [plan](../metal/board/rk3566/plan.go). |
| UEFI ARM64, including the Altra profile | UEFI stub retains ACPI, memory-map and GOP information; EL2 required | MADT CPU topology and PSCI; reserved kernel/carve window, app pool checked against the firmware memory map. [Board](../metal/board/uefi/hop/board.go), [plan](../metal/board/uefi/plan.go). |
| LicheeRV Nano / SG2002, RISC-V64 | FIP monitor replaces OpenSBI; HOP runs in M-mode, apps use the PMP execution boundary | Two known harts, with the non-HOP hart available to apps; board-defined memory plan. [Core code](../metal/board/licheerv/hop/hart.go), [plan](../metal/board/licheerv/hop/plan.go). |

Memory plans reserve node structures and DMA separately from app partitions. Available capacity depends on the actual topology, memory plan and current owners. A board name does not imply support for every device in the same silicon family. Installation profiles and toolchains are described in [development](development.md).

## Devices

| Board | Network path | Local storage | Display |
| --- | --- | --- | --- |
| QEMU virt | Modern MMIO virtio-net, [`virtionet`](../metal/driver/nic/virtionet/virtionet.go) | PCIe NVMe when present in the VM | None in the normal `-nographic` profile |
| M4 | Broadcom 57762, [`tg3`](../metal/driver/nic/tg3/tg3.go), through [Apple PCIe setup](../metal/board/apple/hop/net.go) | ANS NVMe through RTKit/SART; the selected unused GPT extent | Headless; no working display scanout in this board profile |
| Pi 4 | BCM [`genet`](../metal/driver/nic/genet/genet.go) | No NVMe path exposed by the current board implementation | Firmware framebuffer; GUI build available |
| Pi 5 | Cadence [`gem`](../metal/driver/nic/gem/gem.go) in RP1, through Broadcom PCIe | RP1 is wired; NVMe-HAT support is not provided by that wiring | Firmware framebuffer; GUI build available |
| RK3566 | DesignWare [`dwmac4`](../metal/driver/nic/dwmac4/dwmac4.go) | No storage ECAM window exposed by this board profile | VOP2 scanout in the GUI build |
| UEFI ARM64 | Supported Intel adapter through [`igb`](../metal/driver/nic/igb/igb.go) and firmware-configured PCIe | Generic NVMe probe, limited to the existing bus-0 discovery path | GOP framebuffer if supplied by firmware |
| LicheeRV | DesignWare [`dwmac`](../metal/driver/nic/dwmac/dwmac.go) and board PHY setup | No local block-storage driver; SD is the boot medium | Headless |

The node brings up storage in [`main.go`](../metal/cmd/hopos/main.go). Absence of storage does not prevent compute, but a job requiring volumes cannot start without the storage service. Local `hopfs` is scratch storage; its durability limits are described in [operations](operations.md).

GUI is an optional build profile. It registers display grants separately from the framebuffer log console. USB input starts only in a GUI build and only where the board registers controllers; a framebuffer alone does not imply keyboard or mouse support. See [GUI registration](../metal/cmd/hopos/gui.go), [USB input startup](../metal/cmd/hopos/usbinput.go) and the board-specific sources under [`gui`](../metal/gui/).

## Interrupts, idle and reset

Only QEMU virt currently supplies the physical NIC interrupt path: virtio-net through GICv3, with a 10 ms fallback check. The physical boards use the existing 300 µs RX poll interval. Physical IRQ integration on those boards is a subsequent task, separate from the v2.2.2 hardware round. The implementation is in [hopnet](../metal/net/hopnet/hopnet.go) and [QEMU IRQ wiring](../metal/board/qemuvirt/hop/net.go).

The architectural rule remains one network interrupt for external work and one logical doorbell per resident. The doorbell tells the app to inspect published work; it is not a message counter. App wakeup and the current physical RX polling policy are different layers. [Networking](networking.md) explains their relationship.

| Mechanism | Current implementation |
| --- | --- |
| ARM app idle | Architecture event/wait path; M4 uses its EL2 idle and fast-IPI implementation. WFE under QEMU TCG does not prove physical power saving. |
| RISC-V app idle | M-mode wait/timer path around the app context; available hart operations and timer behavior are board-specific. |
| Core release | Context termination and confirmation precede reuse. Where no supported electrical reset exists, the existing switch/park path performs revocation. |
| Watchdog | Board hooks for Raspberry Pi, RK3566, UEFI, M4 and LicheeRV; arming can fail or depend on a probe. QEMU has no default node watchdog. |

Core start, idle and wake hooks are defined by [`board.Cores`](../metal/board/board.go). Their callers follow the same [lifecycle](lifecycle.md); a failed reset or missing acknowledgement does not make an owner free. Watchdog policy and its coverage limits are in [operations](operations.md).

## DMA ownership and driver scope

NIC and NVMe DMA regions belong to the node, outside app partitions. Apps access these devices through existing services. A NIC transmit copies into its own buffer before returning; receive processing completes its copy before recycling the DMA buffer. Driver checks bound received lengths and descriptor indices to the buffers actually supplied.

NVMe uses one polled I/O queue pair and the existing controller mutex. A timeout or uncertain completion blocks further I/O before buffer reuse. Initialization confirms the controller is disabled before clearing queue memory. On M4, RTKit allocations begin after the complete reserved NVMe data mapping. Sources: [NVMe](../metal/driver/nvme/), [RTKit](../metal/driver/rtkit/rtkit.go), [Apple storage allocation](../metal/board/apple/storage.go).

A kernel flip reconstructs drivers in their reserved node regions; it does not adopt old Go driver objects. Retained app connections and successful device reinitialization require separate tests. Cache maintenance, MMIO ordering, PHY behavior and firmware handshakes cannot be established by host tests alone. Consult [status](status.md) before treating a buildable board as hardware-verified, and [measurements](measurements.md) for measured performance.
