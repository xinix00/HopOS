# Operations

[Documentation index](index.md) · [Apps](apps.md) · [Configuration](configuration.md) · [Test status](status.md)

Normal operation consists of submitting a job, following its task state, and updating or removing the desired job. HOP handles placement and management; HopOS owns the app partition, execution context and core claims. A successful dispatch response is not by itself evidence that the app is ready or that its work is correct.

## Deploy and inspect

The node agent listens on TCP 8080. The leader API uses 9080; agents also proxy the `/v1/` routes to the current leader. A healthy local agent can therefore still return a leader-connectivity error for a cluster request.

| Operation | HTTP request |
| --- | --- |
| Local agent health | `GET :8080/health` |
| Local capacity | `GET :8080/capacity` |
| Local tasks | `GET :8080/tasks` |
| Cluster agents and tasks | `GET /v1/agents`, `GET /v1/tasks` |
| Submit or update desired job | `POST /v1/jobs` with the job JSON |
| List jobs or inspect one | `GET /v1/jobs`, `GET /v1/jobs/{name}/status` |
| Remove desired job | `DELETE /v1/jobs/{name}` |
| Stop one local task | `POST :8080/stop-task/{task_id}` |
| Stop local tasks of one job | `POST :8080/stop/{job_name}` |
| Stream app logs | `GET :8080/logs/{task_id}/stdout` or `/stderr` |
| Request a kernel flip | `POST :8080/flip` with `url` and `sha256` |

Use the job routes for declarative operation; the agent's direct `POST /run` route also exists for dispatch. Stopping a local task does not necessarily remove the desired job. The leader may place a replacement while that job remains desired. Job fields and a deployable example are in [apps](apps.md).

For each change, retain the node ID, task ID, artifact hash, request and response status. Inspect readiness and actual app output after deployment. For replacement, follow the resulting tasks rather than assuming that a repeated job name identifies the same running instance.

## Authentication

`hopos.apikey` enables `X-Hop-Auth` on management routes. The header contains HMAC-SHA256 over the request method, decoded URL path without its query string, and SHA256 of the exact body bytes. Use the matching HOP client or signing helper; this is not a bearer-token header. HMAC authenticates requests but does not encrypt HTTP traffic.

Without a key, management starts only with explicit `hopos.insecure=1`. Otherwise the node reports `HOPOS_API_NO_AUTH` and remains alive without starting the management API. A ping response with no agent API can therefore indicate configuration rather than a crash. `/health` is an unauthenticated health route. See [configuration](configuration.md) for the actual image defaults and settings.

## Verify and request an update

Release downloads include `SHA256SUMS`, its SSH signature, and public verification files. Use an `allowed_signers` file whose key you have already accepted, such as the pinned [repository copy](../tools/allowed_signers). From the download directory, verify the manifest and then compare the selected asset's checksum:

```sh
ssh-keygen -Y verify -f allowed_signers -I hello@gethop.org \
  -n gethop-release -s SHA256SUMS.sig < SHA256SUMS
shasum -a 256 hopos-licheerv-headless.img.gz
```

The second command prints the asset checksum; compare it with the matching line in the verified manifest. If every listed asset is present, `shasum -a 256 -c SHA256SUMS` checks the complete set. Release signature verification and the running kernel's expected SHA-256 check are separate steps.

For a node deliberately configured with `hopos.insecure=1`, save the following as `flip.json`, replacing the example URL and hash with the actual board bundle values:

```json
{
  "url": "https://artifacts.example/hopos-licheerv.flip",
  "sha256": "REPLACE_WITH_THE_64_CHARACTER_SHA256"
}
```

```sh
NODE=192.168.1.100
curl --fail-with-body -X POST "http://$NODE:8080/flip" \
  -H 'Content-Type: application/json' --data-binary @flip.json
```

Authenticated requests need the HOP signature over these exact body bytes. A successful response accepts the request; follow the console for validation, jump and adoption, then check the resident apps as described in [kernel flip](kernel-flip.md). A live flip does not update the boot medium.

## App calls and local storage

The standard app library serializes ordinary requests over its shared system connection. Calls travel through the app TCP/IP stack, frame rings and node switch to the internal system service. The node associates that connection with the app's existing servicer, which applies its root, mounts and permissions before executing the operation. Responses return through the same network path. This serialization is per app, not one global executor for all apps. Details are in [networking](networking.md).

A job's `volumes` map maps a shared path to an app-local mount path. For example, `"volumes":{"/scratch/common":"/data"}` exposes that shared directory at `/data` for the job. Paths outside declared mounts resolve against the task's own root. A shared directory deliberately exposes the same data to its participating jobs; an isolated memory partition does not make shared files private.

Local `hopfs` is scratch storage: file metadata resides in node RAM and file blocks reside on NVMe. It is initialized empty at boot. Do not use local roots or volumes as durable storage across reboot or kernel flip. A node without usable block storage can still run compute, but jobs requiring volumes are rejected.

The file index stores allocated contiguous block runs (extents); holes require no entries. File length is bounded by the configured disk window rather than the former aggregate 16-GiB index ceiling. A contiguous file uses one extent regardless of length. Fragmentation remains bounded to 262,144 live file extents across the filesystem; free physical runs are coalesced and shortened index arrays release excess capacity. Reads and writes retain the device's transfer-size batching. Physical capacity, fragmentation metadata and the node-count limit are separate constraints.


S3-backed cluster state and the app object store are separate services. Restoring a desired job after reboot means placing a new task, not resuming its old memory. Use the configured object-store operations for data that needs the durability of that service; their scope and configuration are described in [apps](apps.md) and [configuration](configuration.md).

## Stop and resource reuse

Stop prevents conflicting dispatch and drains admitted access, terminates the app contexts, and waits for confirmation. Only then may the partition and core claims be reused. A shared neighbour or an existing group reservation keeps its core rights.

When termination is uncertain, claims remain retained. A timeout is not evidence that the old context can no longer access memory. Check task state, errors and capacity after stopping; a stop endpoint response alone is insufficient evidence of released resources. Repeated placement attempts do not repair an uncertain owner. Kernel flip is refused while ownership is unresolved. The full rules are in [lifecycle](lifecycle.md).

## Logs and console

App stdout/stderr are exposed as agent log streams using SSE. Following a flip, log attachments are recreated for adopted apps. Logs are best effort, not a persistent archive. Verify fresh output and continuing app progress; an unchanged task ID alone proves neither.

The optional node console is raw TCP. With `hopos.console=5555`, read it using `nc <node> 5555`. It sends retained console output followed by new bytes, supports at most four concurrent readers, and is not authenticated by the API HMAC. It is disabled unless configured. It is a diagnostic stream, not a shell or a second kernel-flip control path.

Both console history and the reserved-RAM black box are bounded. A subsequent boot may recover earlier output from the black box, but power loss or firmware memory initialization can remove it. Absence of an earlier error message does not establish that the earlier boot was healthy.

## Watchdog and recovery

Watchdog behavior depends on successful board-specific arming and is disabled by `hopos.wd=off`. After agent liveness is established, feeding requires a successful new connection to the node's own agent port. That tests the local accept path, not the complete physical NIC receive path, router or external connectivity.

Cold-boot bring-up can continue blind feeding while waiting for initial agent liveness. The early flip-boot guard limits blind feeding to two minutes, measured with the architectural counter. Hardware expiry follows after its own timeout. A watchdog is therefore a bounded recovery mechanism with stated coverage, not a guarantee that every failure causes a prompt reset.

If a node becomes unreachable, record the last action and time. Check console output, local health and external reachability separately; establish whether the watchdog actually armed. If needed, use the normal board boot method with a known image and matching configuration. A watchdog reboot restarts apps and does not count as a successful zero-reboot kernel flip.

For planned kernel changes, follow [kernel-flip](kernel-flip.md): validate compatibility, preserve the node configuration in the replacement image, then inspect task continuity and fresh output. Remaining hardware checks are tracked in [status](status.md).

## Existing I/O measurement

The `vitals` app can measure the existing storage path without a new kernel or reboot. On the app's published HTTP port, request `GET /api/run?test=disk&mb=64&kb=1024`, then inspect `GET /api/state`. The test writes a temporary file, reads it back with content checking, and removes it. Run only one vitals test at a time.

Use the same node, app build, data size, chunk size and mount before and after a change. Throughput, correctness and durability are separate observations. Recorded measurements, including Derek's M4 result, are in [measurements](measurements.md).

## Source reference

The API routes and signing format above were checked against the HOP version required by [metal/go.mod](../metal/go.mod), currently `v1.0.2`: `internal/agent/agent.go`, `internal/agent/handlers.go`, `internal/api/server.go` and `pkg/httputil/auth.go`. These commands are source-checked examples, not a claim that they were executed on every board.

Node integration: [main](../metal/cmd/hopos/main.go), [watchdog](../metal/cmd/hopos/watchdog.go), [console](../metal/kern/conport/conport.go), [system service](../metal/kern/slots/system.go), [storage service](../metal/kern/slots/storage.go), [hopfs](../metal/kern/hopfs/hopfs.go), and [vitals disk test](../apps/vitals/internal/vitals/disk.go).
