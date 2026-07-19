# Configure a node

One set of keys, every board. Only the file differs:

| board | file | format |
|---|---|---|
| UEFI (USB stick) | `hopos.cfg` in the stick's root | `key=value`, whitespace-separated |
| Raspberry Pi | `cmdline.txt` on the SD bootfs | same keys, on the single cmdline |

Editing the file **is** node management — no shell, no rebuild, no agent.

## The keys

| key | meaning | default |
|---|---|---|
| `hopos.node` | node name (shows up in `hop agents`) | generated |
| `hopos.cluster` | cluster name — nodes with the same name form one cluster | — |
| `hopos.cores` | cores reserved for the node runtime itself | `1` |
| `hopos.apikey` | HMAC key for the HTTP API — requests must be signed with it | auth off |
| `hopos.s3.endpoint` | S3 endpoint for cluster state + leader election | state off |
| `hopos.s3.bucket` | bucket name | — |
| `hopos.s3.region` | region | — |
| `hopos.s3.key` / `hopos.s3.secret` | credentials | — |
| `hopos.s3.pathstyle` | `1` = path-style URLs (required for e.g. BunnyCDN) | virtual-host |
| `hopos.init[]` | a job to seed on a clean boot — one compact-JSON job per entry, repeatable | none |

## Example

```
hopos.node=altra-1 hopos.cluster=prod hopos.cores=2
hopos.apikey=<random-hex>
hopos.s3.endpoint=https://s3.example.com hopos.s3.bucket=hop-prod
hopos.s3.region=eu hopos.s3.key=AK... hopos.s3.secret=... hopos.s3.pathstyle=1
```

With S3 configured the node commits its cluster state there and **reloads
its own jobs after any reboot or power cut** — see
[Stateless](technical/stateless.md).

## Init jobs — a baseline on the stick

`hopos.init[]` seeds jobs on a **clean boot** so a node comes out of the box
already running something. Each entry is one job as **compact JSON** (same
schema as `POST /v1/jobs` / `hop apply`, so it's copy-pastable) — repeat the
key for more jobs:

```
hopos.init[]={"name":"dashboard","driver":"hop","artifacts":[{"url":"http://10.0.0.5/dash.elf"}],"memory_limit":100663296,"ports":{"http":80}}
hopos.init[]={"name":"worker","driver":"hop","artifacts":[{"url":"http://10.0.0.5/worker.elf"}],"memory_limit":67108864,"tags":{"sharegroup":"svc"},"cpu_shares":2048}
```

- **No spaces inside the JSON** — the config is whitespace-tokenised, so each
  entry must be one token. Keep it compact (no pretty-printing).
- **Standalone, without S3:** there is no committed state, so *every* boot is
  clean — the node always comes up with exactly these jobs. This is the way to
  ship a self-contained node.
- **With S3:** they seed only on the *first* clean boot; after that the
  committed state is the truth. A seeded job you later delete stays deleted
  (init jobs are a baseline, not a continuously enforced set).
- Order sets priority; an init job whose name already exists is skipped (a seed
  never overwrites operator state). A malformed entry is logged and skipped.

## Trust model

The config (including the API key) is plaintext on the boot medium — the
same trust model as the Pi's own `cmdline.txt`: whoever holds the stick
holds the node. Keep the stick as safe as you'd keep a root password.
