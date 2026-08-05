# BCM2712 C1 erratum: interconnect deadlock under PCIe inbound DMA

**Affected:** Raspberry Pi 5 boards with BCM2712 stepping **C1**.
**Fixed in silicon:** stepping **D0** (later Pi 5 boards / Pi 500).
**Symptom:** total, silent machine freeze — every core hangs mid memory
access, no fault, no exception, no UART output. Only a hardware reset
recovers the machine.

HopOS hits this bug harder than Linux does, because HopOS's normal job
lifecycle (loading an app image into a memory partition, cache maintenance
over the stage-2 tables, TLB invalidation, starting a core) is exactly the
kind of fabric-wide traffic that triggers it. This document records what the
bug is, how we isolated it, and what happened when we came back to it three
weeks later. It should be useful to anyone doing bare-metal or OS work on the
Pi 5.

> **Status (2026-08-05).** The three-layer workaround described below shipped
> in July and was **removed on 2026-08-04** after a storm test suggested it
> did nothing — and that test turned out to be measuring the wrong load. The
> second investigation (see *[Second pass](#second-pass-2026-08-04-05)*) put
> the freeze back on the map, narrowed it further than in July, and ended in a
> hardware decision instead of more software. Read the second half before
> copying anything from the first.

## The bug

The QoS arbitration logic in the memory fabric between PCIe inbound DMA and
the memory controller contains a race (the QoS *forwarding search* in the
AXI→SDC path). When **sustained inbound RX DMA** (in our case: the RP1's GEM
NIC writing received frames into DRAM) coincides with **fabric-wide
operations** — large memcopies, cache clean/invalidate sweeps, broadcast
TLBIs, secondary-core starts — the interconnect can deadlock. Every core
then stalls on its next memory access. Because nothing faults, the freeze is
completely silent.

The bug is not fixable from software. D0 silicon exposes QoS fix bits
("chicken bits for 2712D0") that resolve it; on C1 those same register bits
are reserved and read as zero, which is also how HopOS detects the stepping
at boot:

```
brcmpcie: C1 silicon detected (QoS fix bits reserved) — AXI outstanding throttled to 4
brcmpcie: D0 silicon (QoS fix bits active)
```

## How we isolated it

A soak test (all cores computing, sustained network traffic, a job
start/stop cycle every few seconds) froze the node within minutes. The
freeze survived every software-side theory: it reproduced with the MMU
ladder verified, with SMP disabled, and with the workload replaced by an
idle spin — but never without network RX, and never on QEMU. Correlating
the freeze moments against the slot lifecycle showed every hang landed
inside a slot-start window (image copy, stage-2 cache maintenance, heap
zeroing and TLBI of the booting core) while RX DMA was streaming. An AXI
outstanding-request sweep then gave a clean dose-response curve, which
pointed at fabric arbitration rather than any driver.

## The workaround — three layers

### 1. Reduce collision probability (register configuration)

Match the Linux driver's C1 mitigations, then go stricter:

- inbound burst size 128 bytes (`pcie-brcmstb.c` does the same for BCM2712);
- VDM QoS enabled with the DT's priority map (`brcm,vdm-qos-map =
  0xbbaa9888`) — the RP1 sends QoS VDMs to raise priority precisely when
  its FIFOs fill up during sustained RX; dropping those messages makes the
  congested case worse;
- AXI outstanding requests throttled to **4** (Linux uses 15) — on C1 the
  broken arbitration can only be damped, and the outstanding limit is the
  knob with the clearest measured effect;
- GEM AMP outstanding limits per `rp1.dtsi` (`ar2r`/`aw2w` max 8,
  `aw2b-fill`);
- Ethernet pause frames enabled, so the link backs off instead of the
  fabric.

**Measured effect: ~8× fewer freezes.**

### 2. Avoid the collision (safe job lifecycle)

The trigger is the *overlap* of RX DMA with fabric-wide operations, so the
OS simply never lets them overlap:

- slot starts/stops are serialized — one lifecycle at a time;
- NIC **RX is quiesced for the whole window**, from just before the image
  copy until the app reports READY (its runtime boot includes heap zeroing,
  the last fabric-heavy phase). Dropped frames are ordinary Ethernet loss —
  TCP retransmits. TX stays on, so ACKs and heartbeats keep flowing;
- a **2 ms drain** after quiescing lets in-flight DMA land before the heavy
  work starts;
- **500 ms pacing** between consecutive lifecycles (a kill immediately
  followed by a start was a reliable trigger). Exposed as a board
  capability, so it costs nothing on boards without the erratum.

**Measured effect: another ~5× — 40× combined, down to one freeze per ~400
torture rounds.**

### 3. Make the residue harmless (hardware watchdog)

The remaining rare freeze is converted from "walk to the device and pull
the plug" into a non-event:

- the BCM PM-block hardware watchdog is armed early in boot (default on)
  and petted from software at a third of its timeout (the PM counter ticks
  independently of everything the bug can freeze; max timeout ~15 s);
- a full freeze therefore becomes a self-recovering hardware reset: the
  node is back on the LAN in roughly 40 seconds, and HOP reschedules the
  jobs that were running.

## Second pass (2026-08-04/05)

The July workaround was removed after a 100-wave storm test froze the node
**zero** times without any of the three layers, where July's baseline had been
one freeze per ten rounds. That looked conclusive. It wasn't: the storm
started cages that downloaded **nothing** and ran no graphical app, so it
never produced the load the mitigations existed for. *A mitigation measured
without the load that motivates it measures nothing* — this was the single
most expensive mistake in both investigations, and it was made twice in one
day (see below).

### A reproducer, at last

With the desktop apps running (display + launcher on real glass, viewers on
the web-KVM, HTTP requests arriving) the node dies in **20–30 seconds**, and
starting four apps at once kills it in the first or second wave. That is a
usable instrument where July had one freeze per 400 torture rounds. The
signature is always identical:

```
slot 6: partition 128 MB @ 0x23400000
slot 6: image placed in 77ms
<nothing — watchdog reset 12 s later>
```

No panic, no fault, nothing on the UART either. The next boot reports
`watchdog: hardware reset armed (12s)`. Self-recovery is 20–40 s.

### What is established, and what is not

Varying one thing at a time on a monitor-attached node:

| load | display app | traffic into that cage | result |
| --- | --- | --- | --- |
| 80 cage starts, plain apps (they do download) | no | yes | **20/20 waves clean** |
| desktop apps, no viewers, no input | yes | no | **9 min clean** |
| + KVM streams (outbound) | yes | barely | **4 min clean** |
| + requests into the display app's cage | yes | yes | **dead in 20–30 s** |
| + requests to the *agent* (never enter a cage) | yes | no | **4 min clean** |

Two conditions coincide with every death: **a graphics app is running** and
**inbound traffic is delivered into that app's cage**. Both are necessary;
neither alone is sufficient.

What is **not** established — and was claimed too early in our own notes:

- that writing the framebuffer matters. One death happened while the display
  app was headless (a regression had silently denied it the FB grant), so it
  wrote no pixels at all, on a node with no scanout.
- that scanout matters. It was present in every experiment above and never
  varied cleanly; the comparison that suggested it was confounded by an AXI
  setting that changed at the same time.

The remaining hypothesis is therefore not about pixels: the display app is
special because it **composes 1920×1080 in its own partition** — a large,
sustained memory movement — while DMA lands in that same partition. That is
the shape from the top of this document, with the framebuffer playing no part.
Open test: a non-graphical app that churns memory while being hammered with
requests. If that freezes too, the word "display" can leave this file.

### Two dead ends worth recording

**Double buffering via the firmware mailbox.** The obvious fix for "don't
write the buffer that is being scanned out" is a page flip. The Pi 5 firmware
accepts a virtual framebuffer height of 2×1080 *and*, right after
`allocate_buffer`, even echoes back a virtual offset of 1080 — then answers
`y=0` to every pan request once the display pipeline is live. A
capability probe at boot cannot see this: it cannot distinguish *stored* from
*applied*. A real page flip on this SoC needs the HVS registers (the vc4
path), which is the display driver HopOS deliberately does not build. The
whole double-buffer implementation (ABI words, board interface, app contract)
was written, measured, and reverted.

**Mapping the framebuffer write-combine.** `cpu/memattr` moves the FB window
from Device-nGnRnE to Normal-NC, so the fabric may gather pixel stores into
bursts instead of shipping ~1M ordered transactions per 1080p frame. It works
and the image stays correct, and it is the right way to write DRAM — but it
did **not** change stability, and no measurement showed it faster either (the
damage-stream proxy saturates elsewhere). It ships as hygiene, not as a fix.

### A real bug found on the way

`driver/vcmail` had no lock while three callers shared one hardware mailbox
and one property buffer (framebuffer discovery, the clock governor, and — in
the abandoned flip — the display path). Two concurrent callers overwrite each
other's request and read each other's reply. The class had already been
measured in July from the other side ("a grant read back `3x1500000000`", the
clock rate dvfs had just asked for) and papered over with a cache. It now has
a mutex.

### Decision

Stop working around C1 silicon: buy D0. The **2 GB Pi 5** is the only variant
guaranteed to be D0 (it launched on that cost-reduced die), so the stepping
comes with the SKU instead of with the production batch; 4 GB and 8 GB boards
are a gamble on manufacturing date. For HopOS the RAM is irrelevant — HOP
itself uses ~20 MB of its 128 MB window, and the binding limit is app cores,
not memory.

Keep one C1 board as the regression rig: it is the only hardware on which this
class reproduces in twenty seconds.

## Applicability beyond HopOS

If you run bare-metal on a C1-stepping Pi 5 with PCIe inbound DMA (network
or NVMe), you are exposed to this erratum. The register layer (1) and the
watchdog (3) translate directly. Layer 2 generalizes as a principle: don't
let sustained inbound DMA overlap with cache-maintenance sweeps, broadcast
TLBIs or core power transitions if you can schedule around them.

The second pass adds two lessons that cost us more than the code did:

- **Never evaluate a mitigation without the load it exists for.** Both wrong
  verdicts in this file (July's "the register layer is what saves us" and
  August's "the layers do nothing") came from a test that omitted part of the
  triggering load.
- **Log whether an optimization is actually active.** Two hours went into a
  measurement of a write-combine mapping that was never applied, because the
  code swallowed the error. A single log line would have caught it.

References: `drivers/pci/controller/pcie-brcmstb.c` and
`arch/arm64/boot/dts/broadcom/bcm2712*.dts*` (raspberrypi/linux,
rpi-6.12.y) for the register recipes; RP1 datasheet for the GEM/AMP side.
