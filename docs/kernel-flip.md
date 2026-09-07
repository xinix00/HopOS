# Kernel flip

A kernel flip replaces the running node kernel without rebooting the machine or restarting its applications. The new kernel occupies a borrowed pool window; applications retain their memory and execution contexts while their management records are transferred.

Release **2.2.2** uses **flip ABI 2**. The release number, flip ABI and application slot ABI identify different interfaces; see [Configuration](configuration.md).

## Operator sequence

1. Build or obtain a flip bundle for the node's board and architecture, with its SHA-256. Use the flip artifact rather than a bare ELF or a cold-install disk image.
2. Verify that flip was enabled at boot, only one node core is active, and the pool has room for a new contiguous kernel window. Applications may remain running, including supported shared and SMP residents.
3. Submit the update through the management interface using the bundle URL and expected hash. See [Operations](operations.md) for the command and [Development](development.md) for artifact creation.
4. Reconnect management after the handover. Check the new kernel generation, the existing app identities and progress, and any connections whose continuity matters.
5. Confirm that normal management still works, including stopping and replacing an app. Record any rejected request separately from a completed flip.

A cold installation changes the kernel on the boot medium. A live flip changes the running kernel. If an older running downloader cannot load the update bundle, install the new kernel through the cold path described in [Getting started](getting-started.md).

## Compatibility and capacity

The bundler supplies an ELF with required symbols and a `HOPRELO1` relocation trailer. The kernel checks its flip ABI, entrypoint, image bounds, relocations and RAM symbols. With live residents, the new switch code must match the persistent code on which their contexts execute.

Only one active node core is supported during a flip (`hopos.cores=1`). SMP application cores are a different ownership domain and can be transferred. Quarantined owners or unresolved start/stop state cause rejection. The handoff also has fixed limits on the state it can represent; exceeding them is an error before the jump.

The pool must provide a contiguous window without overlapping any existing owner. The downloaded bundle and placed payload, including BSS, must coexist in that window during preparation. Total free bytes alone do not establish that the update fits.

## Download and placement

The URL path uses the existing lifecycle window, pool allocator, memory scrubber and staging calculation. Conflicting management changes wait while it prepares the replacement. Application computation and network processing can continue.

1. Validate the expected SHA-256, HTTP 200 response and declared length. The accepted length is 1 byte through 64 MiB; the available window imposes a further limit.
2. Borrow the destination window and scrub it. Place the staging region at its upper end, below the handoff tail.
3. Download directly into staging through a fixed **32 KiB buffer**, computing SHA-256 and the content identity along the way. The complete bundle is not allocated in the old kernel heap.
4. Parse through the shared bounded `dev.ReaderAt`. Read relocation metadata in fixed-size chunks rather than copying the whole ELF or relocation table.
5. Validate compatibility and resident state. Reject any payload that would overlap staging before writing its segments.
6. Copy segments into the lower part of the window, clear BSS, apply relocations and patch the new runtime's RAM declaration.

```text
borrowed destination window
+-------------------------+ low addresses
| new payload and BSS     |
| available space         |
| downloaded bundle       | source for placement
+-------------------------+
| handoff tail            | outside new runtime RAM
+-------------------------+ high addresses
```

The LicheeRV kernel reservation remains **32 MiB**, including the 256 KiB handoff tail. After a flip, runtime RAM occupies 31.75 MiB inside that reservation. Pool staging replaces the former large heap allocation, and subsequent flips keep the same reservation size.

Sources: [fetch.go](../metal/kern/kernflip/fetch.go), [bundle.go](../metal/kern/kernflip/bundle.go), [flip.go](../metal/kern/kernflip/flip.go), [dev/reader.go](../metal/dev/reader.go), [layout](../metal/abi/layout/), [partmem.go](../metal/kern/slots/partmem.go).

## Handoff and adoption

After placement, the old kernel records app partitions, core spans, sharegroups and complete group pools, published ports, mount mappings, supported NAT state and agent state. These are serialized values, not pointers into the old Go heap. It writes the handoff, publishes its location with the required memory ordering and jumps to the new entrypoint.

The new kernel recognizes and validates the handoff before cold initialization. It restores all owners and group claims before accepting new placement, then reconnects services to the existing rings. A missing heartbeat does not make an app's partition free.

Application memory, TCP stacks and persistent execution structures stay in place. Supported NAT transfer allows existing app connections to continue. Node-owned connections are recreated: management clients reconnect, and the app's standard system client reconnects on the eligible transport failures described in [Networking](networking.md).

The allocator reconstructs ownership from the reusable board regions and excludes the active kernel. A departed pool kernel has no remaining claim. Boards can also declare their cold kernel reservation reusable when it contains no persistent boot, DMA or control data. The same allocator then makes it available to apps or another kernel. Capacity reporting excludes the active kernel reservation.

LicheeRV declares its original 32 MiB kernel window reusable. FLIP prefers that original address when the complete destination is free; if an app owns it, FLIP chooses another free window. A second flip can therefore restore the original placement geometry. Hardware verification covers four swaps on one persistent app TCP connection, returning to the original window at swaps two and four, followed by the full 206 MiB cloudflared/Stulp/plugins workload. Other boards retain their fixed cold reservations until their persistent structures are accounted for. Total free bytes still do not guarantee a sufficiently large contiguous destination.

Sources: [adopted.go](../metal/kern/kernflip/adopted.go), [slots/adopt.go](../metal/kern/slots/adopt.go), [handoff.go](../metal/net/hopswitch/handoff.go).

## Failure boundaries

An ordinary error before the jump closes the fetch, returns this operation's temporary pool loan and releases its lifecycle window. Existing owners keep their claims. This includes download, hash, format, compatibility, overlap and handoff-capacity errors. Requesting the bundle already running is also rejected.

After the jump, the old kernel is no longer an ordinary rollback target. If the handoff cannot be read safely, the new kernel refuses to continue as an empty cold boot over potentially live applications.

Where enabled and supported, the watchdog can recover through a cold reboot. The flip boot guard permits blind watchdog pets for at most two minutes, measured by the architecture counter; the eventual reset also depends on the hardware timeout. Missing or disabled watchdog support provides no such recovery. A watchdog reboot ends application continuity and is not a successful flip.

See [watchdog.go](../metal/cmd/hopos/watchdog.go) for the policy, [Boards and drivers](boards-drivers.md) for hardware support, and [Status](status.md) for the tested candidates and remaining hardware work.
