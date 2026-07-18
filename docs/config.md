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

## Trust model

The config (including the API key) is plaintext on the boot medium — the
same trust model as the Pi's own `cmdline.txt`: whoever holds the stick
holds the node. Keep the stick as safe as you'd keep a root password.
