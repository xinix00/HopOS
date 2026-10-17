# Jobspecs

The jobspecs of the apps that run on the nodes, taken over from the root of
the Go tree (only `job-lumen.json` was in git there; the rest carried secrets
and was ignored). A spec goes to Hop's leader:

```sh
cp jobs/job-stulp.json jobs/job-stulp.local.json   # fill in the secrets
curl -X POST -d @jobs/job-stulp.local.json http://NODE:9080/v1/jobs
```

| Spec | What | Artifact |
| --- | --- | --- |
| `job-cloudflared.json` | cloudflared's own `tunnel run` as a slot app (30 MB, 256 MB partition) | release `apps` of xinix00/HopOS, Go (`-tamago.elf`) |
| `job-cloudflared-lean.json` | the tunnel protocol itself on lean (a 530 kB image, 32 MiB partition, two connections, share group `huis`); `TUNNEL_URL` is `http://10.100.0.2:80` | release `apps` of xinix00/HopOS, Rust (`apps/cloudflared-lean`, arm64 only) |
| `job-spin.json` | the Spin server with its volume `/spin` | release `rolling` of xinix00/Spin |
| `job-stulp.json` | Stulp, the home automation, port 80 and the attach port 7000 | release `apps` of xinix00/stulp |
| `job-stulp-plugins.json` | Stulp's plugins (Matter on 5540), attached to Stulp | release `apps` of xinix00/stulp |
| `job-lumen.json` | Lumen, the media app on the O6N, with the codec firmware on `/firmware` | a local server, `http://192.168.1.208:8002` |
| `job-webdav.json` | the WebDAV server on the media volumes | a local server, `http://10.100.0.1:8088` |
| `hopos-media-o6n.cfg` | the media node's config: Lumen as init job | (no spec; `CFG=jobs/hopos-media-o6n.cfg MEDIA=1 BOARD=o6n sh image/uefi-run.sh`) |

The content is that of the Go generation, except for the secrets:
`TUNNEL_TOKEN`, `SPIN_MASTER_KEY`, `SPIN_WORKER_TOKEN`, `STULP_TOKEN` and
`STULP_ATTACH_SECRET` appear here as `<NAME>` (for example
`"TUNNEL_TOKEN": "<TUNNEL_TOKEN>"`), because this tree is public. Fill them in
a copy; git ignores `jobs/*.local.json`.

The Lumen and WebDAV specs point to a server on the local LAN (the Mac, or
the node itself on `10.100.0.1`), not to a release: put the ELF there before
you send the spec. Lumen and the media config pin the node to `hopos-1`
(`affinity`, `hopos.node`).

## The release naming scheme

Every app of this tree is in the rolling release `apps` of xinix00/HopOS as
`<app>-<arch>.elf`, with `<arch>` Hop's `node.arch` (`arm64`, `riscv64`):

```
https://github.com/xinix00/HopOS/releases/download/apps/welcome-arm64.elf
https://github.com/xinix00/HopOS/releases/download/apps/cloudflared-lean-arm64.elf
```

`tools/release.sh` builds them (the list `APPS` in `tools/lib.sh` plus decode
for arm64, `APPS_RISCV64` also for riscv64) and says how they get into
`apps`; so the URL does not shift per version, and the headfull layer of the
config (`image/cfg/headfull.cfg`) starts welcome from there.
The Go apps (`go/apps-release.sh`, [go-apps.md](../docs/go-apps.md)) are in
the same release as `<app>-<arch>-tamago.elf`; `job-cloudflared.json`
points to that form.
