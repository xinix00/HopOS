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
bug is, how we isolated it, and the three-layer workaround that ships in
HopOS. It should be useful to anyone doing bare-metal or OS work on the
Pi 5.

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

## Applicability beyond HopOS

If you run bare-metal on a C1-stepping Pi 5 with PCIe inbound DMA (network
or NVMe), you are exposed to this erratum. The register layer (1) and the
watchdog (3) translate directly. Layer 2 generalizes as a principle: don't
let sustained inbound DMA overlap with cache-maintenance sweeps, broadcast
TLBIs or core power transitions if you can schedule around them.

References: `drivers/pci/controller/pcie-brcmstb.c` and
`arch/arm64/boot/dts/broadcom/bcm2712*.dts*` (raspberrypi/linux,
rpi-6.12.y) for the register recipes; RP1 datasheet for the GEM/AMP side.
