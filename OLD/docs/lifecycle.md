# Application lifecycle

The lifecycle manages one application owner: its partition, execution contexts, core claims, group membership and grants. Dedicated applications and trusted applications sharing cores follow the same ownership rules. Sharing changes core assignment; each app still owns a separate memory partition.

## Ownership rules

| Rule | Required behavior |
| --- | --- |
| E1 | Every allocated memory range has a known owner; layout arithmetic and alignment must preserve bounds. |
| E2 | Memory and core claims become reusable only after previous access has ended. |
| E3 | A new owner receives initialized memory; resuming the same owner does not clear it. |
| E4 | Privilege, physical bounds, core spans and trusted ring limits come from node-owned data. |
| E5 | Build the complete context and protection boundary before publishing and dispatching it. |
| E6 | Conflicting lifecycle actions run in order; finish admitted work before release or transfer. |
| E7 | Every permitted wait has a wake route, including work published while sleep begins. |
| E8 | A kernel flip preserves every continuing owner and placement claim before reopening allocation. |
| E9 | Success requires the operation's end conditions; a timeout or missing heartbeat does not prove termination. |

A saved context is still live. A parked core may still be reserved for an app's secondary context or a sharing group. Cage IDs, physical cores and memory partitions describe different resources.

## Allocate and start

1. Validate the request, placement requirements and image. Reserve the application identity, partition and complete core claim before writing into the partition.
2. Clean and initialize the new owner's memory. Place the image and construct its ABI data and architecture-specific cage using trusted boundaries.
3. Publish the prepared context and dispatch it through the board and architecture interface.
4. Classify the result: confirmed start, confirmed absence of execution, or uncertain execution. Only confirmed absence permits ordinary rollback.

The pool allocates aligned regions without overlapping existing owners. Streaming app placement reserves its destination before receiving bytes. The shared [placement code](../metal/abi/place/) checks ELF ranges; [Scrub](../metal/kern/slots/slots.go) initializes memory in chunks and yields between them.

An app becoming ready is distinct from starting execution. Failure to become ready cannot be treated as permission to reuse its memory.

## Share cores or reserve an SMP span

A sharegroup reserves a pool of whole cores. Its members retain separate partitions and run cooperatively on cores in that pool. The pool's unoccupied capacity remains reserved while the group still owns it. Removing one member must preserve its neighbors and the group's remaining claims.

An SMP app reserves several dedicated cores as one owner. Requests to start secondary execution are checked against that reserved span and serialized with stop and flip. Stopping the primary alone does not release the app while another context may still execute.

SMP and sharegroups are not combined in this release. A shared app that does not yield can delay its group peers. See [Applications](apps.md) for placement fields.

## Wait and wake

A producer writes work, publishes it with the required memory ordering, then rings the resident's logical doorbell. The work lives in rings or control data; the doorbell is not a message counter. Multiple notifications may merge because the consumer rechecks the published state.

The wait path publishes its reason and deadline, checks for eligible work and transfers execution. A physical core waits only when its execution policy has no runnable resident. The architecture supplies the necessary barriers and event/IPI mechanism so a concurrent publication is not lost at the sleep boundary.

Wake processing covers the resident rather than assuming its cage number equals its core number. SMP secondary contexts must also be reached. RX is eligible only for the runtime wait conditions that permit it; a network arrival does not override every wait reason.

The implementation is in [waker.go](../metal/kern/slots/waker.go), [cpu/idle](../metal/cpu/idle/) and the architecture switchers. Physical network IRQ wiring is described separately in [Networking](networking.md).

## Stop, quarantine and reuse

Stop closes admission of new execution and partition users, then finishes or cancels work already admitted. It requests cooperative termination and, when needed, uses the architecture's revocation/reset mechanism. All contexts belonging to the owner must be confirmed stopped.

On confirmed termination, HOP stops the servicer and remaining diagnostic users, preserves final diagnostics, detaches networking, releases grants and returns the partition and placement claims. A core remains assigned when another resident or group reservation still owns it. The next owner receives newly initialized memory.

If dispatch or termination remains uncertain, HOP retains the owner in quarantine. Its partition, full core claim and relevant grants remain reserved. A later stop can retry confirmation. The error does not turn the resource into free space, and a kernel flip refuses an owner it cannot safely transfer.

The governing paths are [slots.go](../metal/kern/slots/slots.go), [stream.go](../metal/kern/slots/stream.go), [partmem.go](../metal/kern/slots/partmem.go), [share.go](../metal/kern/slots/share.go) and [smp.go](../metal/kern/slots/smp.go). Device access must also end before associated memory can be reused; individual driver behavior belongs in [Boards and drivers](boards-drivers.md).

## Transfer ownership

During a [kernel flip](kernel-flip.md), existing application memory and execution remain in place. The new kernel restores partitions, full core spans and group pools before making the allocator available. Health information is checked separately from ownership. A missing heartbeat does not release a claim.

Operator actions are described in [Operations](operations.md); implementation and hardware coverage are recorded in [Status](status.md).


## Large partitions and translation storage

The allocator, release path and FLIP adoption use one ownership model on ARM64 and RISC-V. A job's `memory_limit` describes its visible partition, including the existing 2 MiB ABI tail. Architecture code reports any additional translation storage needed; the allocator reserves it with that partition and releases the entire claim only after confirmed termination. It is not a second allocation or a second lifecycle.

Small mappings use existing table storage. When that is insufficient, one additional 2 MiB block is reserved beyond the visible partition. ARM keeps its stage-2 tables inaccessible to the app. RISC-V permits its hardware page walker to read its own tables inside the PMP-owned claim; modifying a mapping still cannot escape that claim. Actual table size is checked before publication. The FLIP record keeps the visible partition size; the same architecture rule reconstructs its full reservation before reuse is possible.

The current large-memory change uses ARM's 39-bit stage-2 address space and RISC-V's positive Sv39 range. These are address-space bounds, not fixed physical slices or per-core reservations. The available contiguous pool usually provides the lower limit. A larger memory allowance does not change the app's ABI-tail layout, require a board-specific app image, or merge a neighbor's memory. Hardware acceptance of the change is recorded in the release logbook.
