# Configuration reference

Node configuration selects identity, authentication, boot jobs, and optional services. Job manifests describe application resources and access. Installation procedures are in [getting started](getting-started.md); operational requests are in [operations](operations.md).

## Node configuration files

The file format is `key=value`, one entry per line. Values extend to the end of the line. Lines beginning with `#` are comments. Keep each JSON job on one line. Repeat `hopos.init[]` or `hopos.apps[]` to supply multiple entries; ordinary keys should occur once.

| Boot route | Configuration source |
| --- | --- |
| Raspberry Pi | `hopos.cfg` on the boot medium, through the Pi boot/config path |
| UEFI | `hopos.cfg` on the FAT boot medium, read by the loader |
| Radxa Zero 3E | Boot configuration passed through its U-Boot/board path |
| LicheeRV Nano | Configuration embedded by `image/licheerv-agent.sh` |
| Apple M4 | Loader-provided configuration when present; otherwise the embedded configuration |
| QEMU `agent` mode | Build-time `HOPCFG` passed by `image/qemu-run.sh` |

Editing a separate file does not change a configuration already embedded in an installed kernel. Rebuild that image or supply the intended configuration in a compatible [flip bundle](kernel-flip.md). Preserve authentication and identity across the update. The differing `CFG` path conventions of the scripts are documented in [development](development.md).

## Authentication

A node needs either a nonempty `hopos.apikey` or the explicit development choice `hopos.insecure=1` to start its management API.

| Configuration | Result |
| --- | --- |
| Nonempty `hopos.apikey` | API requests use `X-Hop-Auth` HMAC authentication. |
| No key, `hopos.insecure=1` | API authentication is disabled. |
| No key and no insecure flag | API startup is refused, with `HOPOS_API_NO_AUTH`. |
| Key and insecure flag both present | The key still enables authentication; remove the redundant flag for clarity. |

The supplied headless and GUI templates explicitly enable insecure operation. They are usable on a trusted test network. For authenticated operation, generate a key, set `hopos.apikey`, and remove `hopos.insecure=1`. Nodes in one cluster use the same key. A key is not a bearer token: use the HOP client's signing path rather than putting it in an arbitrary HTTP header.

```sh
openssl rand -hex 24
```

The console is separate from API authentication. `hopos.console=5555` exposes a read-only, unauthenticated TCP log stream; a management key does not protect it. Set the console to `0` when it should not listen.

## Node keys

Defaults below refer to code behavior unless a template default is explicitly stated.

| Key | Type / default | Meaning |
| --- | --- | --- |
| `hopos.node` | String; board identity, then `hopos-1` fallback | Node ID. Set unique IDs where the board cannot supply one. |
| `hopos.cluster` | String; `hopos` | Cluster name. Naming nodes alike is not sufficient to configure durable cluster state. |
| `hopos.apikey` | String; empty | Shared HMAC key. See authentication rules above. |
| `hopos.insecure` | `1` opts in | Allow the API without a key. |
| `hopos.console` | Positive TCP port enables it; absent/0 disables it | Templates select `5555`. |
| `hopos.cores` | Integer; `1` | Cores used by the node runtime, including core 0. This reduces the cores available to applications. Requests above physical capacity are clamped. |
| `hopos.flip.enable` | Enabled unless exactly `0` | Enables compatible live kernel replacement. |
| `hopos.init[]` | Repeated compact JSON job | Jobs seeded on a clean boot. Existing committed/adopted state has its own recovery path. |
| `hopos.apps[]` | Repeated compact JSON job | Application catalog. It is supplied to applications opting in with `"HOPOS_APPS":""`; catalog entries are not automatically started. |
| `hopos.s3.endpoint` | URL; empty | S3 endpoint for cluster locking, committed state, and the configured app object store. |
| `hopos.s3.bucket` | String; empty | Bucket. Both endpoint and bucket are required to select S3 cluster storage. |
| `hopos.s3.region` | String; empty | S3 region. |
| `hopos.s3.key`, `hopos.s3.secret` | Strings; empty | S3 credentials retained by the node. |
| `hopos.s3.pathstyle` | `1` enables it | Use path-style S3 addressing. |
| `hopos.wd` | `off` disables it | Otherwise use the board's watchdog policy where hardware is wired. |
| `hopos.reboot` | `1` requests reset at boot | Deliberate reset path using the board watchdog; not a normal service setting. |
| `hopos.blackbox` | `1` | Print the previous boot's retained console. |
| `hopos.rxpoll` | `1` | Force receive polling. Physical IRQ integration and its test scope are tracked in [status](status.md). |
| `hopos.idleyield` | `1` | Force the application's idle-yield path for diagnostics. |
| `hopos.idlestat` | `1` | Enable idle statistics output. |

`hopos.nvmebench=1` enables a boot-time disk benchmark that performs writes. It belongs in a controlled storage test, not an ordinary node template. Board-specific settings belong in [boards and drivers](boards-drivers.md).

The watchdog's flip-boot grace is two minutes of raw architectural counter time, shared across early bring-up and the initial canary phase. Cold boot retains its existing unlimited bring-up policy until first agent liveness. Subsequent pets require fresh self-connections to the agent. This does not test physical NIC ingress; see [status](status.md) for hardware reset evidence.

## Job fields

The management schema comes from HOP, pinned to `v1.0.2` in this tree. Use `driver:"hop"` for HopOS applications.

| Field | Type / default | HopOS meaning |
| --- | --- | --- |
| `name` | String; required | Unique job name. |
| `driver` | String | Set `hop`. The generic default is not the Hop driver. |
| `artifacts` | Array | Agent chooses the first artifact matching node attributes. Supply a raw ELF; no extraction or container image. |
| `artifacts[].url` | URL | Download location reachable from the node. |
| `artifacts[].match` | String map; unrestricted when absent | Attribute equality constraints, such as `node.arch: arm64` or `riscv64`. |
| `affinity` | String map | Job-level node attribute constraints; all must match. |
| `count` | Integer; `1` | Desired number of instances. |
| `memory_limit` | Positive byte count | Required for a usable HopOS partition; 0 is rejected by the partition allocator. Rounded up to 2 MiB. Includes image and a 2 MiB ABI tail, not just heap. The rounded partition must be at least 4 MiB; the ELF and runtime need additional usable space. |
| `cpu_shares` | Integer; 0/absent becomes `1024` | 1024 per whole core. The agent rounds positive fractional-core requests upward. |
| `ports` | Name → integer | Node port publication. `0` requests a dynamic port. The assigned port is passed as `ER_PORT_<NAME>`. |
| `env` | String → string | Application environment. |
| `volumes` | Shared path → application path | Filesystem access granted to the application. |
| `tags.sharegroup` | String; absent means dedicated | Named group sharing a core pool; members retain separate memory partitions. |
| `tags.core-class` | String; absent means unrestricted | Physical core class, as reported by the board. |
| `max_restarts` | Integer; `5` | `0` disables restarts; `-1` permits unlimited restarts. |
| `restart_window` | Integer duration in nanoseconds; 0 selects 5 minutes | Window used for restart-count handling. |
| `update_policy` | String; `rolling` | HOP update policy; also accepts `recreate` and `blue-green`. |

Without `sharegroup`, `cpu_shares` is the application's dedicated/SMP width. With `sharegroup`, it is the shared pool size and each member runs on one core. Members must agree on pool size; every existing pool core must match a joining member's explicit class. There is no implemented `sharecores` field or placement tag. Examples are in [applications](apps.md).

## Version numbers

| Identifier | Purpose |
| --- | --- |
| HopOS / metal `v2.2.2` | Release/source version; application imports retain the `/metal/v2` module path. |
| Go / TamaGo `1.26.4` | Compiler/runtime and TamaGo module versions. |
| Slot ABI `10` | Application-to-kernel contract (`layout.ABIVersion`); checked separately from release naming. |
| Flip ABI `2` | Kernel-handoff compatibility (`kernflip.ABI`); checked independently of the release tag. |

Do not infer compatibility or completed hardware coverage from a release number alone. See [kernel flip](kernel-flip.md) and [status](status.md).

Sources: [node setup](../metal/cmd/hopos/main.go), [watchdog](../metal/cmd/hopos/watchdog.go), [default configuration](../image/hopos-headless.cfg), [partition allocator](../metal/kern/slots/partmem.go), [pool placement](../metal/kern/slots/pool.go), [module pins](../metal/go.mod).
