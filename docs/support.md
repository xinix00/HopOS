# Board support and follow-up checklist

[Documentation index](index.md) · [Boards and drivers](boards-drivers.md) · [Test evidence](status.md) · [Release plan](v1/technical/release-afronding.md)

Source inventory: **9 September 2026, current working tree**. This is the current board capability checklist, replacing the historical [3 September matrix](v1/support.md). It includes Radxa Orion O6N separately from Radxa Zero 3 / RK3566. Source support is not hardware acceptance: successful tests remain tied to their recorded build in the [logbook](v1/technical/release-logboek.md).

## The small contract

These are responsibilities of the existing framework and its hardware boundary, not proposals for more interfaces or subsystems.

| Responsibility | Required behavior | Existing boundary |
| --- | --- | --- |
| Boot and memory | Enter the required privilege level; identify usable RAM; reserve firmware, kernel, administration and DMA regions; allocate one private partition per app. | [Board](../metal/board/board.go), board memory plans, [layout](../metal/abi/layout/layout.go) |
| Cores and ownership | Discover physical cores/classes; start an owner; keep cage IDs independent of core IDs; support dedicated or explicitly trusted shared placement; confirm termination before releasing memory and the last core claim. | `Board.Cores`, [slots](../metal/kern/slots/) |
| Wait and wake | Publish work before notification; one logical doorbell per resident; preserve deadlines and progress when all residents sleep. Distinguish app idle from node idle and physical power-off. | [idle](../metal/cpu/idle/), architecture switch, `Cores.IdleMode/Kick`, optional `HartTimerer` |
| Network and services | One caller identity for each service request through the existing network path; bound and own DMA; receive external work. The intended external wake source is one network IRQ, with polling currently the physical-board fallback. | `ProbeNIC`, `Net`, optional `LeaseHolder` / `NICInterrupter`, [hopnet](../metal/net/hopnet/) |
| Kernel replacement | FLIP into an owned window, transfer compatible residents, rebuild node devices, and release the previous eligible window. No deliberate reboot update mode. | [kernel flip](kernel-flip.md), board plans and watchdog hooks |
| Board facilities | Expose only facilities actually wired: storage, framebuffer/input, temperature, clocks, entropy and watchdog. Their absence must be explicit; they do not all become requirements for headless compute. | Existing optional hooks and board startup; [devices](boards-drivers.md) |

`Board` also supplies core identity, timer offset/wall time, privilege/firmware description, PCIe windows and framebuffer discovery. `Cores` supplies app-core enumeration, start, optional reset/reset eligibility, state, idle mode and kick. These methods are covered below. A nil reset or kick is not automatically a defect: confirmed revocation/parking and an event-stream wake path can satisfy the same lifecycle contract.

## How to read the tables

**Wired** means a production call path exists in the inspected source. **Fallback** means the named alternative is active. **Missing** means this board profile has no corresponding implementation. **Conditional** means firmware, boot probing, configuration or the build profile decides availability. **N/A** means the stated profile cannot exercise it. None of these labels means hardware-tested. The acceptance table and checklist below record that separately.

### Boot, allocation and execution

| Board | Boot / privilege | Memory and topology | Start / state | Stop and physical power-off | Sharing / SMP / FLIP |
| --- | --- | --- | --- | --- | --- |
| QEMU virt | ARM EL2, stage-2 | DT memory plan; PSCI probe; homogeneous class | PSCI / affinity state | Revoke and park; no remote reset hook | Wired / wired / wired; emulated |
| Mac mini M4 | iBoot; ARM EL2 with VHE, stage-2 | Boot-argument RAM; Apple topology and E/P classes | Apple release/start path / software state | Revoke and park; PMGR stop is not a usable remote reset | Wired / wired / wired |
| Raspberry Pi 4 | Pi firmware; ARM EL2, stage-2 | DT RAM; shared Pi plan; homogeneous cores | PSCI / affinity state | Revoke and park; no remote reset hook | Wired / wired / wired |
| Raspberry Pi 5 | Pi firmware; ARM EL2, stage-2 | DT RAM; Pi 5 plan; homogeneous cores | PSCI / affinity state | Revoke and park; no remote reset hook | Wired / wired / wired |
| Radxa Zero 3 / RK3566 | U-Boot; ARM EL2, stage-2 | Board carve and RAM discovery; homogeneous cores | PSCI with board affinity mapping / affinity state | Revoke and park; no remote reset hook | Wired / wired / wired |
| UEFI / Altra | UEFI PE stub; ARM EL2 required, stage-2 | Firmware memory map and MADT; generic homogeneous class | PSCI with MADT mapping / affinity state | Revoke and park; no remote reset hook | Wired; final physical acceptance open |
| Radxa Orion O6N | O6N profile over UEFI; ARM EL2 required, stage-2 | UEFI memory map/MADT; efficiency classes when supplied, otherwise all `big` | Inherits UEFI PSCI / affinity state | Inherits revocation/parking; no remote reset hook | Shared implementation; boot and operation confirmed by Derek; measured release sequences tracked below |
| LicheeRV Nano | FIP M-mode monitor; PMP | Fixed 256 MiB declaration and board plan; known two harts, one for apps | Board hart start / reset-register-based state | App-hart kill-tick and park; reset recipe only covers C906L, normally the node hart | Wired / N/A with one app hart / wired |

LicheeRV's `Privilege()` currently calls `RequireMMode(3)` with a constant: the boot contract supplies M-mode, but this is not an independent privilege measurement. Freeing a core claim does not promise electrical power-off. Cold-kernel reservation reuse is enabled on LicheeRV; other board profiles still need persistent boot structures accounted for before those original reservations can be returned to the pool.

### Idle, clocks and external wake

| Board | App idle / wake | IPI to app cores (HOP → app: "there is traffic" or "you are due") | Node idle | Network receive wake | DVFS / clock policy | Temperature to node heartbeat |
| --- | --- | --- | --- | --- | --- | --- |
| QEMU virt | ARM WFE/event stream; shared switch path | **Wired:** GICv3 SGI 1 (`GIC_IPI` switcher: WFI, IAR1/EOIR1 ack); one- and two-core apps verified on EDK2/neoverse-n1 on 20 September | WFE sleeper; TCG is not a power measurement | **Wired:** virtio SPI, GICv3; 10 ms fallback check | N/A for physical frequency control | N/A in this profile |
| M4 | Yield to EL2 WFI; fast IPI kick | **Wired and verified:** Apple fast IPI from the waker (due) and from the switch on ring empty → non-empty (direct RX kick, `wakeRX`); the doorbell of a one-core app arrives as a virtual FIQ | WFE sleeper | **Wired and verified (L82):** tg3 INTx through the Apple PCIe root port and the AIC3 (port line + 4, found by delivery at boot); ack masks the tg3 interrupt mailbox and the rearm re-checks for work and forces a status update (`HOSTCC_MODE_NOW`, as `tg3_int_reenable`; without it a lone frame waited for the 10 ms failsafe and the wire carried 45 instead of 118 MB/s, L83); only PSTATE.I is opened; `hopos.nicirq=0` polls | Fixed board clock setup; no load-following DVFS policy | Conditional SMC hook, off by default; successful sensor transaction still open |
| Pi 4 | ARM WFE/event stream; shared switch path | **Missing:** WFE wakes on the event-stream tick only; GIC-400 (GICv2) has no SGI path in HopOS yet | WFE sleeper | **Fallback:** 300 µs polling; NIC IRQ missing | **Wired:** shared Pi policy through firmware mailbox | **Wired:** firmware mailbox |
| Pi 5 | ARM WFE/event stream; shared switch path | **Missing:** as Pi 4 | WFE sleeper | **Fallback:** 300 µs polling; NIC IRQ missing | **Wired:** shared Pi policy through firmware mailbox | **Wired:** firmware mailbox |
| RK3566 | ARM WFE/event stream; shared switch path | **Missing:** GICv3 present, `GIC_IPI` not built for this board yet | WFE sleeper | **Fallback:** 300 µs polling; NIC IRQ missing | Bootloader clock; no HopOS DVFS policy | Missing heartbeat hook; TSADC source exists |
| UEFI / Altra | ARM WFE/event stream; shared switch path | **Wired (L83):** `Cores.Kick` = SGI 1 to the core's MPIDR, `IdleMode = yield`; verified for one-core apps (bundle 89/90: rtt p99 6 → 0.8 ms); the sibling wake over SGI (bundle 94) is unverified on hardware | WFE sleeper | **Polled, and that is the profile.** HopOS targets nodes of up to about 20 cores where the idle wake rate is a measurable share of the power budget (M4: about 1 W of 4 W). The 128-core Altra is a reference platform, not a target: its ACPI `_PRT` INTx line is delivered but enabling it kills the SoC (L83, UART evidence), and the MSI-X/ITS route Linux takes there is out of scope. `hopos.nicirq=auto` remains for measurement only |
| O6N | Inherited ARM WFE/event stream and shared switch path | **Wired in the build**, inherited from UEFI; re-verification on the board pending | Inherited WFE sleeper | **Wired and verified (L80):** RTL INTx through the Group 1 GICv3 adapter, INTID from the root port (477 for the NIC behind bus 0x30; `hopos.nicirq=` override, `0` polls); the ack masks the NIC and clears all status bits until the pump has drained; stray or stuck lines are disabled with a console line; rtt p50 156–201 µs against 160–168 µs polled, one interrupt per received frame instead of 3333 polls/s | `_CPC` fast-channel `StartClock` runs at startup and raises every domain to its `_CPC` maximum (`hopos.mhz=` cap, `hopos.clock=firmware` opt-out); adaptive DVFS missing; register addresses unverified on hardware | **Conditional:** SCMI `Thermometer` hook; requires Cix identification and a responding sensor protocol |
| LicheeRV | Yield to M-mode; board-probed timer, bounded sleep; no remote MSIP kick | **Missing:** no MSIP kick; a sleeping hart wakes on its bounded timer | `MSleep` when CLINT probe permits; fallback otherwise | **Fallback:** 300 µs polling; NIC IRQ missing | No HopOS DVFS policy | **Wired:** TEMPSEN |

All sleeping residents must still wake for published work. Shared ARM residents use the architecture switch; a board's default dedicated-core WFE mode does not mean shared apps block each other. RISC-V timer presence, permission to sleep, and kill-tick support are probed separately. Measure node and app idle separately: NIC polling keeps scheduling receive checks even when app cores sleep. Watchdogs, timer events and Apple IPIs are separate from the intended single external network IRQ.

Sources: [ARM idle](../metal/cpu/idle/idle_arm64.go), [M4 cores](../metal/board/apple/hop/board.go), [Pi DVFS](../metal/board/raspi/hop/dvfs.go), [RV hart timer](../metal/board/licheerv/hop/hart.go), [QEMU IRQ](../metal/board/qemuvirt/hop/net.go), [node idle counters](../metal/cmd/hopos/node.go).

### Devices and board services

**Scope of the board profiles.** HopOS is built for small, efficient nodes: single boards and mini machines, indicatively up to about 20 cores. The efficiency claim (interrupt-driven idle, a few hundred wakes per second instead of thousands) is made only for boards with a proven interrupt path: the Mac mini M4, the Orion O6N and the LicheeRV doorbell. On those it is a real share of the power budget (M4: about 1 W of 4 W). Larger servers such as the Ampere Altra run the UEFI profile polled and serve as a test bench for the generic code, not as a target: idle wake rate is noise against their idle draw.

| Board | Ethernet / addressing | PCIe and block storage | Framebuffer / display | USB keyboard and mouse | Watchdog | Entropy source path |
| --- | --- | --- | --- | --- | --- | --- |
| QEMU virt | virtio-net; static VM config | ECAM; generic NVMe if attached | None in normal headless profile | No controller registration | None by default | CPU source if available, otherwise jitter fallback |
| M4 | `tg3` BCM57762; DHCP | Apple PCIe; ANS NVMe via RTKit/SART; selected unused GPT extent | Missing scanout; profile reports no framebuffer | No controller registration | Apple hardware watchdog | CPU entropy probe / fallback |
| Pi 4 | `genet`; DHCP | GUI path brings up VL805 PCIe; no block-storage path | Firmware framebuffer; optional GUI grants | **Conditional GUI:** VL805 xHCI | BCM-PM | RNG200; warning/fallback on failure |
| Pi 5 | RP1 `gem`; DHCP | RP1 PCIe wired; NVMe HAT path missing | Firmware framebuffer; optional GUI grants | **Conditional GUI:** RP1 xHCI | BCM-PM | RNG200; warning/fallback on failure |
| RK3566 | `dwmac4`; DHCP | No storage window exposed | **Conditional GUI:** VOP2/HDMI scanout | **Conditional GUI:** DWC3/xHCI | Board watchdog | Boot jitter; hardware TRNG reseed after boot if successful |
| UEFI / Altra | `igb` or `rtl8126` via configured PCIe; DHCP; first supported port | MCFG hierarchy scan; first NVMe, whole device assigned to HopOS | GOP when firmware supplies it | No controller registration | GTDT SBSA when supplied and armed | RNDR or SMCCC TRNG; jitter fallback |
| O6N | Inherited `rtl8126` path; DHCP; first supported port, not two-port aggregation | Inherited MCFG hierarchy scan and whole-device NVMe | Inherited GOP; native display driver missing | Native xHCI registration; ten controllers, Logitech keyboard/mouse receiver and display input stream checked; physical movement pending | GTDT SBSA; armed and controlled failed-FLIP recovery verified in L77 | Inherited RNDR / SMCCC / jitter selection; record actual source |
| LicheeRV | `dwmac` plus board PHY; DHCP | No local block driver; SD used for boot only | Headless; no framebuffer | No controller registration | Probe-gated board watchdog | Jitter fallback with warning |

The file/volume API is shared and available when storage is initialized. HopFS uses bounded extent metadata in the current candidate; its index remains volatile across FLIP. On O6N, two 9 GiB source-plus-backup passes with full position-dependent readback and removal passed on the 18 September RXACK candidate. This is completed-write integrity evidence, not a flush or power-loss guarantee. Generic UEFI/O6N `Disk()` walks the firmware-configured PCIe hierarchy; the old bus-0-only description no longer applies to that path. Whole-device allocation is a concrete storage ownership rule, not a partition-preservation promise.

Framebuffer discovery, an app display grant, scanout and USB input are separate capabilities. GOP alone does not supply USB input. No production audio, Wi-Fi, Bluetooth, USB mass-storage or general SD/eMMC filesystem path is claimed by these board profiles; peripheral presence is not a commitment to implement it.

Sources: [UEFI NIC discovery](../metal/board/uefi/hop/board.go), [UEFI NVMe](../metal/board/uefi/hop/storage.go), [O6N profile](../metal/board/o6n/hop/board.go), [O6N startup](../metal/cmd/hopos/board_o6n.go), [O6N SCMI/clock implementation](../metal/board/o6n/hop/scp.go), [GUI](../metal/cmd/hopos/gui.go), [USB registration](../metal/cmd/hopos/usbinput.go), [board drivers](boards-drivers.md).

## Hardware acceptance is separate

**17 September 2026:** Derek confirms that the O6N boots and is fully operational. Board bring-up is complete. The remaining release checks below concern recorded test sequences and measurements; they do not indicate a non-working board. See [L74](v1/technical/release-logboek.md#l74--o6n-boot-and-operation-confirmed).

| Board | Recorded evidence | Still required for the final candidate |
| --- | --- | --- |
| QEMU virt | Framework, mixed sharing/SMP and live FLIP with retained TCP; L59 | Build-specific checks when relevant code changes; no claim about physical caches or power |
| M4 | Earlier lifecycle/sharing/SMP and FLIP sequences; L67 adds 5/18 GiB partitions, live adoption of the 18 GiB resident and complete release; Spin restored at 2 GiB | Latest ARM cage/core changes; explain initial transition fault; large HopFS copy; remaining I/O and soak checks |
| Pi 4 / Pi 5 / RK3566 | L72: twenty lifecycle cycles, sampled reuse canaries, sharing/SMP, quiet wake, three live FLIPs retaining residents/TCP, full thirty-minute compute/network runs and final Vitals; exact review artifacts recorded | Final published dependency/artifact scope; optional board-specific idle/power/device checks from B01–B18 |
| LicheeRV | L53–L60: window reuse, transport, sharing and operator-confirmed Stulp operation | Final published dependency/artifact acceptance; no SMP requirement on one app hart |
| UEFI / Altra | Existing source/build history; no final physical signoff in this release round | Boot, devices, lifecycle, shared/SMP ownership, idle, watchdog and FLIP on the actual machine |
| O6N | L77: lifecycle, heterogeneous sharing, all-eleven-core reclamation, three live FLIPs and full thirty-minute compute/network/NVMe run pass; large-file integrity passes on the recorded RXACK candidate | Exact artifact scopes and measurements are in L77; USB input, network IRQ and further performance/power work remain separate |

`tools/test.sh` builds the `o6n` agent (plain and GUI) and probe; `image/flip-bundle.sh o6n` and `tools/release.sh` carry the O6N target (since L66). The build route is `BOARD=o6n image/uefi-run.sh probe|agent`. O6N now boots and operates on physical hardware. Record the exact release image and repeated live-FLIP results alongside the remaining release measurements.

## Follow-up work list

Each row is an item to assess and close, not an instruction to add every peripheral. Close it with a dated implementation/test result, an explicit supported fallback, or a deliberate out-of-scope decision. Keep the shared contract small and reuse its existing hooks. Fix current correctness failures before adding optional capabilities.

| ID | Scope | Remaining work | Done when |
| --- | --- | --- | --- |
| B01 | All boards | Finish the existing release acceptance on exact artifacts, keeping old results distinct from new changes. | Applicable H0–H6 results, source/dependency/image hashes and limitations are recorded. |
| B02 | All boards | Measure app idle, node idle, timer deadlines and doorbell wake with dedicated and shared residents, including all residents asleep. | Repeated wake/progress succeeds; measured idle/wake rates and power conditions are recorded. No lost-work repair loop is added. |
| B03 | All boards | Verify claim release, forced termination, park/reuse and real core-state reporting. | A busy or unresponsive resident cannot retain or corrupt a released owner; the shared neighbor remains alive. Distinguish parked from electrically off. |
| B04 | All boards | Complete physical NIC IRQ integration through the existing receive path; QEMU remains the reference. | Receive/drain/rearm, quiet-to-busy wake, bursts, link recovery and FLIP pass; polling comparison records idle cost and throughput. No per-device scheduling framework is introduced. |
| B05 | M4 | Resolve the observed ARM transition fault and assess instruction-cache publication when switch code changes. | Reproducible cause/fix or bounded evidence is recorded; the initial transition and subsequent FLIPs pass. A diagnostic rebuild alone is insufficient. |
| B06 | M4 | Verify extent-backed storage with a large source plus backup, contents and release/reuse; repeat the reported approximately 4000 MB/s workload. | Hardware capacity and data checks pass; three measured runs include workload, device, units and flush/cache conditions. |
| B07 | M4 | Assess SMC temperature and existing fixed clocks; decide whether adaptive clock control is useful. | Sensor reports a valid value or remains explicitly unavailable; any selected clock work has measured benefit and safe limits. |
| B08 | Pi 4 / Pi 5 | Verify firmware DVFS, temperature, low/high transitions, failure retry and sustained load. | Requested and observed behavior is recorded with idle/load/thermal measurements. |
| B09 | RK3566 | Assess TSADC heartbeat wiring before deciding on clock changes. | Valid temperature or explicit limitation; any later DVFS choice follows measurements. |
| B10 | UEFI / Altra | Assess connecting existing SMpro temperature to the common thermometer hook. | Heartbeat reports the actual sensor value when available; other UEFI machines retain an explicit no-sensor result. |
| B11 | O6N | Bring-up complete (L74). Record the final image, actual topology/memory and lifecycle/FLIP test sequence; retain boot/config observations as evidence. | Actual board topology and usable memory match the plan; start/share/SMP/stop/reuse and compatible FLIP pass on identified O6N images. |
| B12 | O6N | Validate RTL8126, configured PCIe hierarchy, NVMe DMA/high BARs, GOP and GTDT watchdog. | Each exposed device is exercised across startup and FLIP; missing firmware facilities are recorded. Confirm first-port selection. |
| B13 | O6N | Validate the SCMI thermometer; finish/verify startup wiring for the existing `_CPC` clock helper; assess adaptive DVFS and actual entropy source. | Each has a measured implementation, documented firmware behavior/fallback, or deliberate scope decision. No invented cluster table. |
| B14 | LicheeRV | Review fixed RAM/privilege assumptions, per-hart timer/sleep probes and entropy fallback. | Supported board variant and fallback limits are explicit; failed probes preserve progress and confirmed termination. |
| B15 | Storage profiles | Validate NVMe timeouts, content, capacity, release/reuse and device reinitialization; assess Pi 5 NVMe HAT as optional later work. | Storage ownership and failure behavior are proven on each supported controller; absent storage remains explicit elsewhere. |
| B16 | Display/input profiles | Exercise Pi framebuffer, RK3566 scanout, UEFI/O6N GOP and registered GUI USB controllers, including replug/recovery. M4 scanout and generic UEFI input remain separate; finish O6N physical input/replug checks. | Display grants/geometry and input isolation/recovery work on the selected profiles; no headless pass is counted as GUI evidence. |
| B17 | Physical boards | Verify watchdog arming, bounded boot guard and recovery; record missing or failed probes. | Controlled failure recovers where promised; watchdog reset is not counted as a successful online FLIP. |
| B18 | All boards | Reconcile documentation, website, measurements and this matrix after each accepted change. | Public pages describe current supported behavior; historical failures remain in the logbook rather than temporary public placeholders. |

For B04, the board-specific investigation is: M4 AIC/PCIe interrupt delivery; Pi 4 GENET/GIC; Pi 5 RP1/PCIe interrupt delivery to the GIC; RK3566 GMAC/GIC; UEFI/Altra PCIe NIC routing from firmware; O6N RTL8126/PCIe routing from firmware; LicheeRV MAC/PLIC delivery. Confirm actual lines, routing and controller mode from the board before implementation. Existing controller code does not prove a complete NIC wake path, and no line-count or effort estimate is assumed here.

The follow-up after the current release is **this board checklist**, including IRQ, idle, clocks, storage, display and observability. It does not turn every optional facility into a blocker for the current compute release. Choose the next bounded item from the gaps and measurements above, then update its evidence.
