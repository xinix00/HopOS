# Documentation structure

The documentation explains how to use HopOS, how it works, and what each board provides. All pages are in English. Descriptions are factual: behavior, ownership, interfaces, configuration, and measurements. The previous documentation is retained under `docs/v1/`.

## Reading order

| Section | Page | Contents |
| --- | --- | --- |
| Start | `index.md` | The compute model, terminology, and navigation. Separate execution where requested; explicitly trusted apps may share cores while retaining their own memory partitions. |
| Use | `getting-started.md` | Select an image, configure a node, install, boot, find the node, and run a first job. Separate cold installation from subsequent kernel flips. |
| Use | `apps.md` | Minimal app, application APIs, build artifacts, job manifests, dedicated placement, trusted sharing, and SMP. Examples before implementation details. |
| Use | `operations.md` | Deploy, inspect, stop, replace, read logs, mount volumes, retain durable state, and recover after failure. |
| Design | `architecture.md` | The shared framework, architecture-specific mechanisms, board wiring, drivers, and a source-tree map. One diagram follows an app request through the existing network and service path. |
| Design | `lifecycle.md` | Ownership rules E1–E9, partition allocation, start/publication, wait/wake, confirmed stop, reuse, and retained claims when termination is uncertain. Shared and dedicated ownership are described together. |
| Design | `networking.md` | Per-app network stacks, frame rings, the node switch, internal system calls, caller serialization, identity, permissions, NAT, and the logical doorbell. Physical IRQ and polling are described separately. |
| Design and procedure | `kernel-flip.md` | Compatibility, request, download into the borrowed window, validation, placement, handoff, adoption, retained app connections, internal reconnection, and failure cleanup. A short operator procedure precedes the implementation sequence. |
| Reference | `configuration.md` | Node configuration keys and job fields: types, units, defaults, dependencies, and examples. Distinguish release versions, slot ABI, and flip ABI. |
| Reference | `boards-drivers.md` | Per-board architecture, boot method, cores, memory layout, NIC, storage, optional display/input, idle/wake, and watchdog. Map each function to its driver and board integration. |
| Reference | `development.md` | Toolchain and dependencies, building images, reusable framework boundaries, source locations, and existing verification commands. |
| Evidence | `measurements.md` | Throughput, latency, memory use, and lifecycle costs. Each row states hardware, workload, units, build when known, measurement source, and conditions. |
| Evidence | `status.md` | Implemented capabilities, per-board test coverage, exact tested candidates, remaining release checks, and planned work. Link to the existing release log and retained evidence. |

`index.md` is the website entry page. `menu.md` uses the existing viewer format: section headings and Markdown page links, with `index.md` first. `README.md` is a repository entry pointing to the index; `STRUCTURE.md` is an editorial record and is excluded from the website menu.

The set stays flat under `docs/`; no additional documentation framework is required. Each topic has one primary page. Other pages link to it rather than repeat configuration tables or implementation explanations.

## Architectural thread

The central explanation follows the existing path:

```text
app API / caller
    -> app TCP/IP stack
    -> frame rings and node switch
    -> internal system service
    -> the app's existing servicer and access checks
    -> the requested operation / driver
    -> response through the same network path
```

“Single caller” is documented at its actual scope: the standard app library serializes requests over its shared system connection; the node associates that connection with the app's existing servicer. It does not mean that all apps share one global execution goroutine. The node's management endpoints also use the network stack.

The logical doorbell notifies a resident to inspect published work. Its architecture-specific wake mechanism belongs in the design explanation. Boot, context switching, and the bounded panic-log fallback are documented as their actual mechanisms; the network model should not imply that low-level hardware transitions themselves are TCP requests.

Code anchors: `metal/app/applib/applib.go`, `metal/abi/systemapi`, `metal/kern/slots/system.go`, `metal/kern/slots/storage.go`, `metal/net/hopswitch`, and `metal/net/hopnet`.

## Measurements

Include approximately **4000 MB/s writes on the Mac mini M4**, measured by Derek. Attribute the measurement and leave unrecorded workload, drive, build, and cache conditions unspecified until supplied. It is a useful result, not a performance guarantee and not a measurement reproduced during the release review.

Keep throughput, latency, and correctness observations distinct. For example, retained TCP connections during a flip and the longest observed response time describe different properties. Present the numbers in tables with their conditions, without promotional comparisons.

## Completion criteria

- All new prose is English and describes the current implementation.
- A reader can complete the basic usage sequence without reading review history.
- Architecture explains shared paths and ownership before board-specific details.
- Commands, module paths, configuration fields, defaults, and version numbers are checked against source.
- Each page and source reference has a valid relative link when the page set is complete.
- Measurements identify their origin; hardware support is not inferred from compilation alone.
- The existing documentation remains available for comparison.
- The final v2.2.2 hardware tests, starting with LicheeRV, remain scheduled for 7 September 2026. Physical network IRQ integration remains a separate subsequent task.
