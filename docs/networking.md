# Networking and system calls

Each application runs its own TCP/IP stack over frame rings in its partition. The node switch forwards frames between applications, the node stack and the external network. Application computation and its TCP state remain in the application's memory.

## Frame paths

| Destination | Path |
| --- | --- |
| Another local app | Source TX ring → node frame switch → destination RX ring. The node does not terminate the app's TCP connection. |
| Node service | App rings → switch port 0 → node TCP/IP stack → service. |
| External peer | App rings → switch/NAT → physical NIC; replies are translated and delivered to the app's RX ring. |
| Published app port | Node port publication and NAT select the app; the app's stack accepts the connection. |

The internal address plan is deterministic. The node is `10.100.0.1`; app IP and MAC addresses are derived from their slot identities. The gateway path translates between the internal node address and the node stack's configured address. An internal service request does not need to leave the physical NIC.

The switch is the single consumer of an app's TX ring and producer of its RX ring. It limits bursts and uses bounded backpressure rather than waiting indefinitely for a receiver. Frame bounds and source identity are checked at the switch boundary.

Sources: [hopswitch.go](../metal/net/hopswitch/hopswitch.go), [gateway.go](../metal/net/hopswitch/gateway.go), [nat.go](../metal/net/hopswitch/nat.go), [hopnet](../metal/net/hopnet/), [appnet](../metal/app/applib/appnet/).

## One standard caller per app

The standard [application library](../metal/app/applib/applib.go) shares a persistent TCP connection to the system service at `10.100.0.1:10100`. Its caller lock serializes a request and its response, including sequence validation, so concurrent goroutines do not interleave protocol exchanges on that connection. Normal logging uses the same connection with a distinct frame kind.

The [system protocol](../metal/abi/systemapi/systemapi.go) defines bounded call, result and log frames. Bulk I/O is split into chunks of at most 1 MiB. The node reuses request and bulk-response buffers for the lifetime of a system connection.

This is per-app serialization in the standard client, not a global single caller for the node. The service accepts separate app connections and handles them independently. It caps connections at two per app lifecycle to permit reconnection while the old connection is closing; that allowance does not make concurrent calls on the standard client's connection independent.

The network must be initialized before normal system calls. Boot, context dispatch, yield and physical wake operations use their architecture interfaces. Fatal runtime output has a bounded logging fallback for cases where the normal caller cannot run.

## Identity and permissions

The switch binds the source MAC and IPv4/ARP source address to the originating slot. The system service derives the slot from the accepted peer address, then attaches the connection to that slot's existing servicer. It checks that the same servicer still owns the slot before handling each frame. A reused slot number does not transfer an old connection to its new owner.

The servicer resolves filesystem paths through the app's root and configured mounts. Object-store calls construct keys inside the job's configured namespace; the app does not select arbitrary bucket credentials or a new identity. These checks belong to the service, after network peer identification.

See [system.go](../metal/kern/slots/system.go), [rpc.go](../metal/kern/slots/rpc.go) and [storage.go](../metal/kern/slots/storage.go). User-facing filesystem and persistence behavior is described in [Applications](apps.md) and [Operations](operations.md).

## Logical doorbell and physical receive

Publishing a received frame makes work available in a ring. The logical doorbell asks the resident to inspect that work. Several arrivals can share a notification; the consumer must recheck the published ring before sleeping. Runtime wait conditions determine whether RX is a permitted reason to resume.

The architecture and board provide the actual event, IPI or wait-loop mechanism. This is distinct from how a physical NIC reports received packets. The single network-IRQ direction does not imply one physical interrupt bit across the machine, and it does not turn polling implementations into IRQ implementations. Current board wiring is listed in [Boards and drivers](boards-drivers.md); the subsequent IRQ integration work is tracked in [Status](status.md).

## Connections across a kernel flip

Application-owned TCP state stays with the app. The handoff preserves supported NAT state and published ports so existing app connections can continue through the replacement kernel.

The system service's TCP endpoint belongs to the old node kernel and is recreated. The app library closes a failed system connection and retries a request once on a non-timeout transport failure. It does not retry a timeout, because the operation may still be running. Protocol and operation errors are returned to the caller. The first system call after a flip can therefore include reconnection latency.

This internal reconnect is separate from retaining an app's connection to an external client. See [Kernel flip](kernel-flip.md) for the transfer sequence and [Measurements](measurements.md) for observed latency and throughput.


## Artifact download progress

The streamed artifact path distinguishes idle time from total duration: header waiting is bounded, and the body can take longer than a minute while bytes continue arriving. The corrected HOP implementation refreshes its existing 60-second idle timer on every positive read, independently of batched UI progress. A silent stream is closed and an unstarted app's claim is released. See [Status](status.md) for the tested candidate and dependency publication state.
